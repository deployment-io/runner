package aws_s3

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
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

	filesToUpload, uploadedKeys, err := collectFilesToUpload(root, io.Discard)
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
	if _, _, err := collectFilesToUpload(filepath.Join(t.TempDir(), "nope"), io.Discard); err == nil {
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

// An upload that fails before reading its file — as CreateMultipartUpload does when
// it is refused — must not leave the file's reader blocked handing over its first
// chunk: once uploadFiles returns, every reader has exited and closed its stream.
func TestUploadFilesReleasesReadersOfFailedUploads(t *testing.T) {
	dir := t.TempDir()
	files := make([]fileToUpload, 0, 3)
	for i := 0; i < 3; i++ {
		path := filepath.Join(dir, fmt.Sprintf("f%d.txt", i))
		if err := os.WriteFile(path, []byte("contents"), 0o644); err != nil {
			t.Fatal(err)
		}
		files = append(files, fileToUpload{path: path, objectKey: filepath.Base(path)})
	}
	uploader, err := NewUploader("region", "bucket", &s3.Client{})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var streams []<-chan fileByteStreamDTO
	uploader.uploadFile = func(path, key string, abort chan interface{}) <-chan uploadFileDoneDTO {
		stream := uploader.fileByteStreamGenerator(path, abort)
		mu.Lock()
		streams = append(streams, stream)
		mu.Unlock()
		done := make(chan uploadFileDoneDTO, 1)
		done <- uploadFileDoneDTO{objectKey: key, err: errors.New("CreateMultipartUpload refused")}
		return done
	}
	if err := uploader.uploadFiles(files, io.Discard); err == nil {
		t.Fatal("uploadFiles: want the upload error")
	}
	// Nothing reads the streams: a reader still blocked handing over its chunk would
	// get this receive and report open. Given a moment, a released reader has
	// already exited and closed its stream.
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(streams) != len(files) {
		t.Fatalf("%d readers started, want %d", len(streams), len(files))
	}
	for i, stream := range streams {
		select {
		case _, open := <-stream:
			if open {
				t.Errorf("reader %d was still blocked handing over its chunk after uploadFiles returned", i)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("reader %d still blocked after uploadFiles returned", i)
		}
	}
}

// buildPublishDirWithSymlinks makes a publish directory holding two regular
// files plus a symlink to a file outside it and a symlink to a directory
// outside it. It returns the publish directory and every temp-dir path
// involved, so callers can check no host path leaks into the logs.
func buildPublishDirWithSymlinks(t *testing.T) (string, []string) {
	t.Helper()
	root := t.TempDir()
	outside := t.TempDir()
	outsideDir := t.TempDir()
	for _, file := range []string{"index.html", filepath.Join("assets", "main.js")} {
		path := filepath.Join(root, file)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(outside, "secret.env")
	if err := os.WriteFile(secret, []byte("TOKEN=1"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outsideDir, "config.yaml"), []byte("k: v"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "leak.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(root, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	return root, []string{root, outside, outsideDir}
}

func assertOnlyRegularFiles(t *testing.T, filesToUpload []fileToUpload, uploadedKeys map[string]bool) {
	t.Helper()
	want := []string{"assets/main.js", "index.html"}
	var got []string
	for _, file := range filesToUpload {
		got = append(got, file.objectKey)
		if strings.HasPrefix(file.objectKey, "linked-dir/") {
			t.Errorf("walked into a symlinked directory: %q", file.objectKey)
		}
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("files to upload = %v, want %v", got, want)
	}
	if len(uploadedKeys) != len(want) {
		t.Errorf("uploadedKeys = %v, want exactly %v", uploadedKeys, want)
	}
	for _, key := range want {
		if !uploadedKeys[key] {
			t.Errorf("key %q missing from uploadedKeys", key)
		}
	}
}

// A symlink in the build is opened with os.Open, which follows it, so queuing
// one publishes the host file it points at. Only regular files are uploaded.
func TestCollectFilesToUploadSkipsSymlinks(t *testing.T) {
	root, tempDirs := buildPublishDirWithSymlinks(t)
	var logs bytes.Buffer
	filesToUpload, uploadedKeys, err := collectFilesToUpload(root, &logs)
	if err != nil {
		t.Fatal(err)
	}
	assertOnlyRegularFiles(t, filesToUpload, uploadedKeys)

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	wantLines := map[string]bool{
		"Skipping leak.txt: symlink, not a regular file":   false,
		"Skipping linked-dir: symlink, not a regular file": false,
	}
	if len(lines) != len(wantLines) {
		t.Fatalf("log lines = %q, want one Skipping line per symlink", lines)
	}
	for _, line := range lines {
		if _, ok := wantLines[line]; !ok {
			t.Errorf("unexpected log line %q", line)
		}
		wantLines[line] = true
		for _, dir := range tempDirs {
			if strings.Contains(line, dir) {
				t.Errorf("log line %q leaks host path %q", line, dir)
			}
		}
	}
	for line, seen := range wantLines {
		if !seen {
			t.Errorf("missing log line %q", line)
		}
	}
}

func TestCollectFilesToUploadSkipsSymlinksWithNilLogsWriter(t *testing.T) {
	root, _ := buildPublishDirWithSymlinks(t)
	filesToUpload, uploadedKeys, err := collectFilesToUpload(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertOnlyRegularFiles(t, filesToUpload, uploadedKeys)
}

// A symlinked publish root is refused rather than resolved, however it is
// spelled: Linux resolves `link/` and `link/.` through the link.
func TestCollectFilesToUploadRefusesSymlinkedRoot(t *testing.T) {
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "index.html"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	for _, spelling := range []string{link, link + "/", link + "/."} {
		filesToUpload, uploadedKeys, err := collectFilesToUpload(spelling, io.Discard)
		if !errors.Is(err, DirectoryErr) {
			t.Errorf("collectFilesToUpload(%q) err = %v, want DirectoryErr", spelling, err)
		}
		if len(filesToUpload) != 0 || len(uploadedKeys) != 0 {
			t.Errorf("collectFilesToUpload(%q) returned files %v / keys %v, want none",
				spelling, filesToUpload, uploadedKeys)
		}
	}
}

// A read error mid-file must fail the file and abort the multipart upload,
// not upload an empty part and complete the object as if it succeeded.
func TestUploadByteStreamToS3FailsOnReadError(t *testing.T) {
	var mu sync.Mutex
	var aborted, completed, emptyPart bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && query.Has("uploads"):
			w.Write([]byte(`<InitiateMultipartUploadResult><Bucket>b</Bucket><Key>k</Key><UploadId>u1</UploadId></InitiateMultipartUploadResult>`))
		case r.Method == http.MethodPut && query.Has("partNumber"):
			body, _ := io.ReadAll(r.Body)
			if len(body) == 0 {
				emptyPart = true
			}
			w.Header().Set("ETag", `"p"`)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete && query.Has("uploadId"):
			aborted = true
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && query.Has("uploadId"):
			completed = true
			w.Write([]byte(`<CompleteMultipartUploadResult><Bucket>b</Bucket><Key>k</Key><ETag>"e"</ETag></CompleteMultipartUploadResult>`))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()

	client := s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(server.URL),
		UsePathStyle: true,
		Credentials:  aws.AnonymousCredentials{},
	})
	uploader, err := NewUploader("us-east-1", "b", client)
	if err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(filePath, []byte("good content"), 0644); err != nil {
		t.Fatal(err)
	}

	readErr := errors.New("read failed")
	stream := make(chan fileByteStreamDTO)
	go func() {
		defer close(stream)
		stream <- fileByteStreamDTO{data: []byte("good")}
		stream <- fileByteStreamDTO{err: readErr}
	}()
	abort := make(chan interface{})
	defer close(abort)

	var done uploadFileDoneDTO
	select {
	case done = <-uploader.uploadByteStreamToS3(filePath, "k", stream, abort):
	case <-time.After(10 * time.Second):
		t.Fatal("upload did not finish")
	}

	if done.done {
		t.Error("done = true after a read error, want false")
	}
	if !errors.Is(done.err, readErr) {
		t.Errorf("err = %v, want it to wrap the read error", done.err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !aborted {
		t.Error("multipart upload was not aborted")
	}
	if completed {
		t.Error("multipart upload was completed after a read error")
	}
	if emptyPart {
		t.Error("an empty part was uploaded for the failed read")
	}
}
