package commands

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const screenshotTestResult = `{"schema_version":1,"status":"completed","verdict":"pass","screenshot":{"width":1280,"height":720}}`

var testJPEG = []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F'}

// writeResultAndScreenshot writes result.json and, with write, screenshot.jpg
// as write leaves it.
func writeResultAndScreenshot(write func(path string) error) func(string) (verifyBrowserExit, error) {
	return func(ioDir string) (verifyBrowserExit, error) {
		if err := os.WriteFile(filepath.Join(ioDir, "result.json"), []byte(screenshotTestResult), 0o644); err != nil {
			return verifyBrowserExit{}, err
		}
		return verifyBrowserExit{}, write(filepath.Join(ioDir, "screenshot.jpg"))
	}
}

func writeJPEG(data []byte) func(string) error {
	return func(path string) error { return os.WriteFile(path, data, 0o644) }
}

// The screenshot is read only when the check asks for one.
func TestVerifyBrowser_ScreenshotOnlyWhenAsked(t *testing.T) {
	d := &fakeVerifyDocker{present: true, run: writeResultAndScreenshot(writeJPEG(testJPEG))}
	v := newTestVerifyBrowser(t, d)
	out, err := v.run(context.Background(), verifyRequest(), nil)
	if err != nil || out.Screenshot != nil || len(out.Result) == 0 {
		t.Fatalf("not asked: screenshot %v, result %q, err %v", out.Screenshot, out.Result, err)
	}
	req := verifyRequest()
	req.Screenshot = true
	var logs strings.Builder
	out, err = v.run(context.Background(), req, &logs)
	if err != nil || !bytes.Equal(out.Screenshot, testJPEG) {
		t.Fatalf("asked: screenshot %v, err %v", out.Screenshot, err)
	}
	if logs.String() != "" {
		t.Errorf("logs = %q, want none", logs.String())
	}
}

// No screenshot file (an older image, or screenshot_error) is nil without a
// log line.
func TestVerifyBrowser_NoScreenshotFile(t *testing.T) {
	d := &fakeVerifyDocker{present: true, run: writeResultAndScreenshot(func(string) error { return nil })}
	v := newTestVerifyBrowser(t, d)
	req := verifyRequest()
	req.Screenshot = true
	var logs strings.Builder
	out, err := v.run(context.Background(), req, &logs)
	if err != nil || out.Screenshot != nil || len(out.Result) == 0 {
		t.Fatalf("screenshot %v, result %q, err %v", out.Screenshot, out.Result, err)
	}
	if logs.String() != "" {
		t.Errorf("logs = %q, want none", logs.String())
	}
}

// A screenshot that is a symlink, a FIFO, over 1 MiB or not a JPEG is dropped
// with one line naming why; the check itself still has its result.
func TestVerifyBrowser_RefusedScreenshots(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret.jpg")
	if err := os.WriteFile(outside, testJPEG, 0o644); err != nil {
		t.Fatal(err)
	}
	big := append(append([]byte{}, testJPEG...), make([]byte, 1<<20)...)
	for _, tc := range []struct {
		name  string
		write func(string) error
		why   string
	}{
		{"symlink", func(p string) error { return os.Symlink(outside, p) }, "symbolic link"},
		{"fifo", func(p string) error { return syscall.Mkfifo(p, 0o644) }, "not a regular file"},
		{"directory", func(p string) error { return os.Mkdir(p, 0o755) }, "not a regular file"},
		{"too large", writeJPEG(big), "larger than 1048576 bytes"},
		{"not a jpeg", writeJPEG([]byte("\x89PNG\r\n\x1a\n")), "not a JPEG"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &fakeVerifyDocker{present: true, run: writeResultAndScreenshot(tc.write)}
			v := newTestVerifyBrowser(t, d)
			req := verifyRequest()
			req.Screenshot = true
			var logs strings.Builder
			done := make(chan struct{})
			go func() {
				defer close(done)
				out, err := v.run(context.Background(), req, &logs)
				if err != nil || out.Screenshot != nil || len(out.Result) == 0 {
					t.Errorf("screenshot %d bytes, result %q, err %v", len(out.Screenshot), out.Result, err)
				}
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("reading the screenshot blocked")
			}
			want := "QA: dropped a check's screenshot: "
			if got := logs.String(); !strings.HasPrefix(got, want) || !strings.Contains(got, tc.why) || strings.Count(got, "\n") != 1 {
				t.Errorf("logs = %q, want one line %q…%q", got, want, tc.why)
			}
		})
	}
}

// result.json is never read through a symlink, nor from a FIFO.
func TestVerifyBrowser_ResultNotReadThroughASymlinkOrFIFO(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "result.json")
	if err := os.WriteFile(outside, []byte(screenshotTestResult), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, write := range map[string]func(string) error{
		"symlink": func(p string) error { return os.Symlink(outside, p) },
		"fifo":    func(p string) error { return syscall.Mkfifo(p, 0o644) },
	} {
		t.Run(name, func(t *testing.T) {
			d := &fakeVerifyDocker{present: true, run: func(ioDir string) (verifyBrowserExit, error) {
				return verifyBrowserExit{}, write(filepath.Join(ioDir, "result.json"))
			}}
			v := newTestVerifyBrowser(t, d)
			done := make(chan struct{})
			go func() {
				defer close(done)
				out, err := v.run(context.Background(), verifyRequest(), nil)
				if err != nil || out.Result != nil {
					t.Errorf("result %q, err %v; want none", out.Result, err)
				}
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("reading result.json blocked")
			}
		})
	}
}
