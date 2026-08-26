package aws_s3

import (
	"os"
	"path/filepath"
	"testing"
)

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
