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

// buildTreeWithSymlinks makes a publish dir holding two regular files, a
// symlink to a file outside it, and a symlink to a directory with a file in it.
func buildTreeWithSymlinks(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "assets"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"index.html", filepath.Join("assets", "main.js")} {
		if err := os.WriteFile(filepath.Join(root, file), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "leak.txt")); err != nil {
		t.Fatal(err)
	}
	linkedTarget := filepath.Join(outside, "dir")
	if err := os.MkdirAll(linkedTarget, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linkedTarget, "inner.txt"), []byte("inner"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(linkedTarget, filepath.Join(root, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	return root
}

func assertOnlyRegularFilesCollected(t *testing.T, filesToUpload []fileToUpload, uploadedKeys map[string]bool) {
	t.Helper()
	want := map[string]bool{"index.html": true, "assets/main.js": true}
	if len(uploadedKeys) != len(want) {
		t.Errorf("uploadedKeys = %v, want exactly %v", uploadedKeys, want)
	}
	for key := range want {
		if !uploadedKeys[key] {
			t.Errorf("key %q missing from uploadedKeys", key)
		}
	}
	if len(filesToUpload) != len(want) {
		t.Errorf("got %d files to upload, want %d: %v", len(filesToUpload), len(want), filesToUpload)
	}
	for _, file := range filesToUpload {
		if !want[file.objectKey] {
			t.Errorf("unexpected file queued for upload: %q", file.objectKey)
		}
	}
	for key := range uploadedKeys {
		if key == "leak.txt" || key == "linked-dir" || strings.HasPrefix(key, "linked-dir/") {
			t.Errorf("symlinked entry %q must not be uploaded", key)
		}
	}
}

// os.Open follows symlinks, so a queued symlink would publish whatever it
// points to — /proc/self/environ, a runner config file. Only regular files
// may be collected.
func TestCollectFilesToUploadSkipsSymlinks(t *testing.T) {
	root := buildTreeWithSymlinks(t)
	var logs bytes.Buffer
	filesToUpload, uploadedKeys, err := collectFilesToUpload(root, &logs)
	if err != nil {
		t.Fatal(err)
	}
	assertOnlyRegularFilesCollected(t, filesToUpload, uploadedKeys)

	var skipped []string
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if strings.HasPrefix(line, "Skipping") {
			skipped = append(skipped, line)
		}
		if strings.Contains(line, root) {
			t.Errorf("log line leaks the absolute host path: %q", line)
		}
	}
	wantLines := map[string]bool{
		"Skipping leak.txt: symlink, not a regular file":   true,
		"Skipping linked-dir: symlink, not a regular file": true,
	}
	if len(skipped) != len(wantLines) {
		t.Fatalf("got skip lines %q, want one per symlink", skipped)
	}
	for _, line := range skipped {
		if !wantLines[line] {
			t.Errorf("unexpected skip line %q", line)
		}
	}
}

func TestCollectFilesToUploadSkipsSymlinksWithNilLogsWriter(t *testing.T) {
	root := buildTreeWithSymlinks(t)
	filesToUpload, uploadedKeys, err := collectFilesToUpload(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertOnlyRegularFilesCollected(t, filesToUpload, uploadedKeys)
}

// A symlinked publish directory is refused, not resolved: resolving it would
// publish whatever the link points to.
func TestCollectFilesToUploadRejectsSymlinkedRoot(t *testing.T) {
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "index.html"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "dist")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	// "dist/." and "dist/" make the kernel resolve the link, so they must be
	// refused too.
	for _, root := range []string{link, link + "/.", link + "/"} {
		filesToUpload, _, err := collectFilesToUpload(root, io.Discard)
		if !errors.Is(err, DirectoryErr) {
			t.Errorf("%q: err = %v, want one wrapping DirectoryErr", root, err)
		}
		if len(filesToUpload) != 0 {
			t.Errorf("%q: got %d files for a symlinked root, want none", root, len(filesToUpload))
		}
	}
}

// A real publish directory spelled "dist/." still yields relative keys.
func TestCollectFilesToUploadCleansRootPath(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	_, uploadedKeys, err := collectFilesToUpload(root+"/.", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(uploadedKeys) != 1 || !uploadedKeys["index.html"] {
		t.Errorf("got keys %v, want only index.html", uploadedKeys)
	}
}

// A read error mid-file must abort the multipart upload and fail the file,
// not upload an empty part and complete as if it succeeded.
func TestUploadByteStreamToS3AbortsOnReadError(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	emptyPart := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		var call string
		switch {
		case r.Method == http.MethodPost && query.Has("uploads"):
			call = "create"
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, `<InitiateMultipartUploadResult><Bucket>b</Bucket><Key>k</Key><UploadId>u1</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == http.MethodPut && query.Has("partNumber"):
			call = "part"
			body, _ := io.ReadAll(r.Body)
			if len(body) == 0 {
				mu.Lock()
				emptyPart = true
				mu.Unlock()
			}
			w.Header().Set("ETag", `"p"`)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete && query.Has("uploadId"):
			call = "abort"
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && query.Has("uploadId"):
			call = "complete"
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, `<CompleteMultipartUploadResult><Bucket>b</Bucket><Key>k</Key><ETag>"e"</ETag></CompleteMultipartUploadResult>`)
		default:
			call = r.Method + " " + r.URL.String()
			w.WriteHeader(http.StatusBadRequest)
		}
		mu.Lock()
		calls = append(calls, call)
		mu.Unlock()
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

	// Opened to sniff the content type, so it must exist and be non-empty.
	filePath := filepath.Join(t.TempDir(), "k.txt")
	if err := os.WriteFile(filePath, []byte("good"), 0644); err != nil {
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
	var result uploadFileDoneDTO
	select {
	case result = <-uploader.uploadByteStreamToS3(filePath, "k", stream, abort):
	case <-time.After(10 * time.Second):
		t.Fatal("upload did not finish")
	}

	if result.done {
		t.Error("done = true for a file whose read failed")
	}
	if !errors.Is(result.err, readErr) {
		t.Errorf("err = %v, want one wrapping the read error", result.err)
	}
	mu.Lock()
	defer mu.Unlock()
	called := map[string]bool{}
	for _, call := range calls {
		called[call] = true
	}
	if !called["abort"] {
		t.Errorf("AbortMultipartUpload not called; calls = %v", calls)
	}
	if called["complete"] {
		t.Errorf("CompleteMultipartUpload called after a read error; calls = %v", calls)
	}
	if emptyPart {
		t.Errorf("an UploadPart request had an empty body; calls = %v", calls)
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
