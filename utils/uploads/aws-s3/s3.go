package aws_s3

import (
	"fmt"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type Uploader struct {
	s3Client   *s3.Client
	s3Region   string
	s3Bucket   string
	uploadFile func(string, string, chan interface{}) <-chan uploadFileDoneDTO
}

func NewUploader(s3Region, s3Bucket string, s3Client *s3.Client) (*Uploader, error) {
	uploader := &Uploader{
		s3Client: s3Client,
		s3Region: s3Region,
		s3Bucket: s3Bucket,
	}
	uploader.uploadFile = uploader.UploadFile
	return uploader, nil
}

func isPathDirectory(directoryPath string) (bool, error) {
	fileInfo, err := os.Stat(directoryPath)
	if err != nil {
		// error handling
		return false, err
	}

	if !fileInfo.IsDir() {
		// is not a directory
		return false, nil
	}

	return true, nil
}

// uploadConcurrency is how many files are uploaded at once. It replaces the
// old "sleep 10 seconds every 10 files" throttle, which cost a 241-file
// code-split build about four minutes of doing nothing.
//
// 12 is high enough that such a build finishes in tens of seconds, far below
// S3's per-prefix request quotas (3500 writes/second), and — the reason this
// is a fixed number rather than one goroutine per file — it bounds how many
// multipart part buffers exist at once. UploadFile buffers up to ~2 parts of
// the file it is sending, so unbounded fan-out would let memory grow with the
// size of the build.
const uploadConcurrency = 12

// fileToUpload pairs a local path with the object key it will be written under,
// so the walk can finish before any upload starts.
type fileToUpload struct {
	path      string
	objectKey string
}

// we can assume that this function is called only for directory
//
// Returns the set of object keys this upload is responsible for — every file
// walked, not only the ones that reported success. Callers prune the bucket
// down to this set, so it must describe the build in full: a key missing from
// it is a live file the prune would delete.
func (u *Uploader) uploadDirectory(directoryPath string, logsWriter io.Writer) (map[string]bool, error) {
	filesToUpload, uploadedKeys, err := collectFilesToUpload(directoryPath)
	if err != nil {
		return nil, err
	}
	if err := u.uploadFiles(filesToUpload, logsWriter); err != nil {
		return nil, err
	}
	return uploadedKeys, nil
}

// collectFilesToUpload walks the build directory and returns every file in it,
// along with the set of object keys those files will occupy.
func collectFilesToUpload(directoryPath string) ([]fileToUpload, map[string]bool, error) {
	filesToUpload := make([]fileToUpload, 0)
	uploadedKeys := map[string]bool{}
	root := directoryPath
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			//	upload if it's not directory
			outputS3ObjectKey := strings.TrimPrefix(path, directoryPath+"/")
			// Recorded HERE, from the same expression that names the object,
			// so the caller's view of the build cannot diverge from what was
			// actually written. Re-deriving these keys from the directory
			// somewhere else would usually agree and occasionally not, and the
			// failure mode of disagreeing is deleting the live site.
			uploadedKeys[outputS3ObjectKey] = true
			filesToUpload = append(filesToUpload, fileToUpload{
				path:      path,
				objectKey: outputS3ObjectKey,
			})
		}
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return filesToUpload, uploadedKeys, nil
}

// uploadFiles sends every file through a pool of uploadConcurrency workers and
// returns the first upload error, if any. The *s3.Client the workers share is
// safe for concurrent use across goroutines, which is the only concurrency
// guarantee this scheduling needs.
func (u *Uploader) uploadFiles(filesToUpload []fileToUpload, logsWriter io.Writer) error {
	abortUploadSignal := make(chan interface{})
	// Unbuffered, like every other channel here: the feeder hands a file over
	// only when a worker is free to take it, so nothing about this queue grows
	// with the size of the build. The feeder cannot be left blocked, because
	// the workers below take from jobs until it closes and the drain below
	// keeps them from stalling on a send.
	jobs := make(chan fileToUpload)
	go func() {
		defer close(jobs)
		for _, file := range filesToUpload {
			jobs <- file
		}
	}()

	results := make(chan uploadFileDoneDTO)
	workers := uploadConcurrency
	if len(filesToUpload) < workers {
		workers = len(filesToUpload)
	}
	var waitGroup sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for file := range jobs {
				// Receiving the done signal here, before taking the next job,
				// is what holds each worker to one in-flight upload — and so
				// the pool to uploadConcurrency of them.
				results <- <-u.uploadFile(file.path, file.objectKey, abortUploadSignal)
			}
		}()
	}
	go func() {
		waitGroup.Wait()
		close(results)
	}()

	// DRAIN EVERY CHANNEL, then fail. The done channels are unbuffered, so a
	// return from inside this loop would leave every not-yet-received upload
	// goroutine blocked on its send — forever, in a long-lived runner, one
	// leaked goroutine per remaining file on every failed deploy. Collecting
	// the first error and returning it after the loop keeps the abort without
	// the leak. Ranging until results closes is the drain: it ends only once
	// every worker has finished every job it took.
	//
	// Log lines are per file and customers read them; with a pool they arrive
	// interleaved rather than in walk order, which is fine.
	var firstErr error
	for done := range results {
		if done.done {
			io.WriteString(logsWriter, fmt.Sprintf("Successfully uploaded file: %s\n", done.objectKey))
		} else {
			io.WriteString(logsWriter, fmt.Sprintf("Error uploading file: %s\n", done.objectKey))
		}
		if done.err != nil && firstErr == nil {
			firstErr = done.err
		}
	}
	// Was `return err` — the WalkDir error, which is always nil by the
	// time this loop runs. So a failed file upload logged its line and
	// then reported SUCCESS to the caller.
	//
	// Harmless while a deploy only ever added files. Not harmless now:
	// the caller prunes everything absent from this build immediately
	// afterwards, so a swallowed upload failure would delete the old
	// file and leave nothing in its place. Prune-after-upload is only
	// safe if a failed upload stops the deploy.
	return firstErr
}

var DirectoryErr = fmt.Errorf("path is not a directory path")

// UploadDirectory uploads a tree and returns the object keys it wrote.
func (u *Uploader) UploadDirectory(directoryPath string, logsWriter io.Writer) (map[string]bool, error) {
	isDirectory, err := isPathDirectory(directoryPath)
	if err != nil {
		return nil, err
	}
	if !isDirectory {
		return nil, DirectoryErr
	}
	return u.uploadDirectory(directoryPath, logsWriter)
}

func (u *Uploader) UploadFile(inputFilePath, outputS3ObjectKey string, abortUploadSignal chan interface{}) <-chan uploadFileDoneDTO {
	fileBytesStream := u.fileByteStreamGenerator(inputFilePath, abortUploadSignal)
	fileUploadDoneSignal := u.uploadByteStreamToS3(inputFilePath, outputS3ObjectKey, fileBytesStream, abortUploadSignal)
	return fileUploadDoneSignal
}
