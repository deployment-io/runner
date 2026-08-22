package aws_s3

import (
	"fmt"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Uploader struct {
	s3Client *s3.Client
	s3Region string
	s3Bucket string
}

func NewUploader(s3Region, s3Bucket string, s3Client *s3.Client) (*Uploader, error) {
	return &Uploader{
		s3Client: s3Client,
		s3Region: s3Region,
		s3Bucket: s3Bucket,
	}, nil
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

// we can assume that this function is called only for directory
//
// Returns the set of object keys this upload is responsible for — every file
// walked, not only the ones that reported success. Callers prune the bucket
// down to this set, so it must describe the build in full: a key missing from
// it is a live file the prune would delete.
func (u *Uploader) uploadDirectory(directoryPath string, logsWriter io.Writer) (map[string]bool, error) {
	fileUploadDoneSignals := make([]<-chan uploadFileDoneDTO, 0)
	abortUploadSignal := make(chan interface{})
	uploadedKeys := map[string]bool{}
	root := directoryPath
	//10 concurrent uploads sleep for 10 seconds after 10 requests for now
	//TODO fix later
	i := 0
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
			fileUploadDoneSignal := u.UploadFile(path, outputS3ObjectKey, abortUploadSignal)
			fileUploadDoneSignals = append(fileUploadDoneSignals, fileUploadDoneSignal)
			i++
			if i%10 == 0 {
				time.Sleep(10 * time.Second)
			}
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	for _, fileUploadDoneSignal := range fileUploadDoneSignals {
		done := <-fileUploadDoneSignal
		if done.done {
			io.WriteString(logsWriter, fmt.Sprintf("Successfully uploaded file: %s\n", done.objectKey))
		} else {
			io.WriteString(logsWriter, fmt.Sprintf("Error uploading file: %s\n", done.objectKey))
		}
		if done.err != nil {
			// Was `return err` — the WalkDir error, which is always nil by the
			// time this loop runs. So a failed file upload logged its line and
			// then reported SUCCESS to the caller.
			//
			// Harmless while a deploy only ever added files. Not harmless now:
			// the caller prunes everything absent from this build immediately
			// afterwards, so a swallowed upload failure would delete the old
			// file and leave nothing in its place. Prune-after-upload is only
			// safe if a failed upload stops the deploy.
			return nil, done.err
		}
	}
	return uploadedKeys, nil
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
