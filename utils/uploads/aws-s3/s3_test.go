package aws_s3

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// These tests replace uploader.uploadFile, so the client is never dialed —
// but it must be non-nil, because every real upload now goes through the
// Uploader's single shared client. That sharing is the whole point: a client
// per file means a credentials cache per file, which rate-limits IMDS the
// moment uploads run concurrently.
func TestNewUploaderRequiresAClient(t *testing.T) {
	if _, err := NewUploader("region", "bucket", nil); err == nil {
		t.Error("NewUploader(nil client) must error — a nil client would have " +
			"each file build its own, which is the IMDS-throttling bug")
	}
}

// The uploaded-key set is what the caller prunes the bucket down to, so a file
// walked but not listed is a live file put on the expiry clock. Check that the
// set and the upload list agree, and that both cover nested directories.
func TestCollectFilesToUploadCoversEveryFile(t *testing.T) {
	root := t.TempDir()
	files := []string{
		"index.html",
		filepath.Join("assets", "main.abc123.js"),
		filepath.Join("assets", "chunks", "vendor.def456.js"),
	}
	for _, file := range files {
		path := filepath.Join(root, file)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// An empty directory has nothing to upload and must contribute no key.
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0755); err != nil {
		t.Fatal(err)
	}

	filesToUpload, uploadedKeys, err := collectFilesToUpload(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(filesToUpload) != len(files) {
		t.Fatalf("got %d files to upload, want %d", len(filesToUpload), len(files))
	}
	if len(uploadedKeys) != len(files) {
		t.Fatalf("got %d uploaded keys, want %d", len(uploadedKeys), len(files))
	}
	for _, file := range files {
		key := filepath.ToSlash(file)
		if !uploadedKeys[key] {
			t.Errorf("key %q missing from uploadedKeys", key)
		}
	}
	for _, file := range filesToUpload {
		if !uploadedKeys[file.objectKey] {
			t.Errorf("file %q queued for upload but not in uploadedKeys", file.path)
		}
	}
}

func TestCollectFilesToUploadMissingDirectory(t *testing.T) {
	if _, _, err := collectFilesToUpload(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("expected an error for a directory that does not exist")
	}
}

func testFiles(count int) []fileToUpload {
	files := make([]fileToUpload, count)
	for i := range files {
		files[i] = fileToUpload{
			path:      fmt.Sprintf("/build/file-%d", i),
			objectKey: fmt.Sprintf("assets/file-%d", i),
		}
	}
	return files
}

func TestUploadFilesLimitsConcurrencyAndUploadsEveryFileOnce(t *testing.T) {
	files := testFiles(uploadConcurrency * 3)
	uploader, err := NewUploader("region", "bucket", &s3.Client{})
	if err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{}, len(files))
	release := make(chan struct{})
	var inFlight int32
	var maxInFlight int32
	var mu sync.Mutex
	calls := make(map[string][]string)
	uploader.uploadFile = func(path, objectKey string, _ chan interface{}) <-chan uploadFileDoneDTO {
		current := atomic.AddInt32(&inFlight, 1)
		for {
			maximum := atomic.LoadInt32(&maxInFlight)
			if current <= maximum || atomic.CompareAndSwapInt32(&maxInFlight, maximum, current) {
				break
			}
		}
		mu.Lock()
		calls[path] = append(calls[path], objectKey)
		mu.Unlock()
		started <- struct{}{}

		done := make(chan uploadFileDoneDTO)
		go func() {
			<-release
			atomic.AddInt32(&inFlight, -1)
			done <- uploadFileDoneDTO{done: true, objectKey: objectKey}
		}()
		return done
	}

	finished := make(chan error, 1)
	go func() { finished <- uploader.uploadFiles(files, io.Discard) }()
	// Fill the pool before releasing any upload. An instant fake could make a
	// broken, effectively serial implementation report a meaningless max of 1.
	for i := 0; i < uploadConcurrency; i++ {
		<-started
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}

	if got := atomic.LoadInt32(&maxInFlight); got > uploadConcurrency {
		t.Fatalf("observed %d concurrent uploads; limit is %d", got, uploadConcurrency)
	} else if got != uploadConcurrency {
		t.Fatalf("pool never filled: observed max %d concurrent uploads, want %d", got, uploadConcurrency)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != len(files) {
		t.Fatalf("uploaded %d distinct paths, want %d", len(calls), len(files))
	}
	for _, file := range files {
		got := calls[file.path]
		if len(got) != 1 || got[0] != file.objectKey {
			t.Errorf("upload calls for %q = %q, want exactly [%q]", file.path, got, file.objectKey)
		}
	}
}

type notifyingWriter struct {
	wrote chan string
	bytes.Buffer
}

func (w *notifyingWriter) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	w.wrote <- string(p)
	return n, err
}

func (w *notifyingWriter) WriteString(s string) (int, error) {
	n, err := w.Buffer.WriteString(s)
	w.wrote <- s
	return n, err
}

func TestUploadFilesReturnsFirstErrorAndDrainsRemainingUploads(t *testing.T) {
	files := testFiles(uploadConcurrency + 5)
	uploader, err := NewUploader("region", "bucket", &s3.Client{})
	if err != nil {
		t.Fatal(err)
	}

	firstErr := errors.New("first upload failed")
	laterErr := errors.New("later upload failed")
	completions := make(map[string]chan uploadFileDoneDTO, len(files))
	started := make(chan string, len(files))
	var mu sync.Mutex
	uploader.uploadFile = func(_ string, key string, _ chan interface{}) <-chan uploadFileDoneDTO {
		done := make(chan uploadFileDoneDTO)
		mu.Lock()
		completions[key] = done
		mu.Unlock()
		started <- key
		return done
	}

	logs := &notifyingWriter{wrote: make(chan string, len(files))}
	finished := make(chan error, 1)
	go func() { finished <- uploader.uploadFiles(files, logs) }()

	firstWave := make([]string, 0, uploadConcurrency)
	for i := 0; i < uploadConcurrency; i++ {
		firstWave = append(firstWave, <-started)
	}
	mu.Lock()
	firstDone := completions[firstWave[0]]
	mu.Unlock()
	firstDone <- uploadFileDoneDTO{objectKey: firstWave[0], err: firstErr}
	<-logs.wrote // Ensure the first error has been received and recorded.

	mu.Lock()
	laterDone := completions[firstWave[1]]
	mu.Unlock()
	laterDone <- uploadFileDoneDTO{objectKey: firstWave[1], err: laterErr}

	// Complete every other invocation, including the rest of the first wave,
	// then each replacement as it starts. All fake done channels are
	// unbuffered, so uploadFiles cannot return unless it drains every one.
	for _, key := range firstWave[2:] {
		mu.Lock()
		done := completions[key]
		mu.Unlock()
		done <- uploadFileDoneDTO{done: true, objectKey: key}
	}
	for startedCount := uploadConcurrency; startedCount < len(files); startedCount++ {
		key := <-started
		mu.Lock()
		done := completions[key]
		mu.Unlock()
		done <- uploadFileDoneDTO{done: true, objectKey: key}
	}
	if got := <-finished; !errors.Is(got, firstErr) {
		t.Fatalf("uploadFiles returned %v, want first error %v", got, firstErr)
	}
}

func TestUploadFilesHandlesDegenerateInputs(t *testing.T) {
	for _, count := range []int{0, uploadConcurrency - 1} {
		t.Run(fmt.Sprintf("files=%d", count), func(t *testing.T) {
			uploader, err := NewUploader("region", "bucket", &s3.Client{})
			if err != nil {
				t.Fatal(err)
			}
			var calls int32
			uploader.uploadFile = func(_ string, key string, _ chan interface{}) <-chan uploadFileDoneDTO {
				atomic.AddInt32(&calls, 1)
				done := make(chan uploadFileDoneDTO, 1)
				done <- uploadFileDoneDTO{done: true, objectKey: key}
				return done
			}
			if err := uploader.uploadFiles(testFiles(count), io.Discard); err != nil {
				t.Fatal(err)
			}
			if got := atomic.LoadInt32(&calls); got != int32(count) {
				t.Fatalf("got %d uploads, want %d", got, count)
			}
		})
	}
}
