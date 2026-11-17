package agenttools

// qa_screenshots.go keeps one QA round's screenshots: verify_preview_rendered
// writes each check's screenshot here under the check's id, and after the QA
// stage the runner uploads the ones QA cited (jobs/commands). The bytes live on
// disk, in a directory the store owns; the agent never sees them.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

const (
	// QAScreenshotMaxBytes caps a screenshot, as kit's qa_models.MaxEvidenceBytes
	// does on deployment-server.
	QAScreenshotMaxBytes = 1 << 20
	// qaScreenshotMaxSide bounds a screenshot's width and height.
	qaScreenshotMaxSide = 10000
)

// qaCheckIDPattern is a check id deployment-server accepts.
var qaCheckIDPattern = regexp.MustCompile(`^[a-z0-9_-]{1,16}$`)

// QAScreenshots is one QA round's screenshots, by check id. Safe for
// concurrent tool calls.
type QAScreenshots struct {
	dir string

	mu    sync.Mutex
	shots map[string]qaScreenshot
}

// qaScreenshot is one stored screenshot: its file and pixel size.
type qaScreenshot struct {
	path          string
	width, height int
}

// NewQAScreenshots creates the store in dir (0700), which it owns: anything
// already there is removed.
func NewQAScreenshots(dir string) (*QAScreenshots, error) {
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &QAScreenshots{dir: dir, shots: map[string]qaScreenshot{}}, nil
}

// Dir is the store's directory.
func (s *QAScreenshots) Dir() string { return s.dir }

// put stores a check's screenshot (a file, 0600), replacing any it had.
func (s *QAScreenshots) put(checkID string, image []byte, width, height int) error {
	if !qaCheckIDPattern.MatchString(checkID) {
		return fmt.Errorf("invalid check id %q", checkID)
	}
	if len(image) == 0 || len(image) > QAScreenshotMaxBytes {
		return errors.New("the screenshot is empty or too large")
	}
	path := filepath.Join(s.dir, checkID+".jpg")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shots[checkID] = qaScreenshot{path: path, width: width, height: height}
	return nil
}

// Has reports whether the store holds a screenshot for checkID.
func (s *QAScreenshots) Has(checkID string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.shots[checkID]
	return ok
}

// Get reads checkID's screenshot and its pixel size.
func (s *QAScreenshots) Get(checkID string) (image []byte, width, height int, err error) {
	if s == nil {
		return nil, 0, 0, errors.New("no screenshots")
	}
	s.mu.Lock()
	shot, ok := s.shots[checkID]
	s.mu.Unlock()
	if !ok {
		return nil, 0, 0, fmt.Errorf("no screenshot for %s", checkID)
	}
	image, err = os.ReadFile(shot.path)
	if err != nil {
		return nil, 0, 0, err
	}
	return image, shot.width, shot.height, nil
}

// validQAScreenshotSize reports whether a screenshot's width and height are
// ones deployment-server accepts.
func validQAScreenshotSize(width, height int) bool {
	return width >= 1 && width <= qaScreenshotMaxSide && height >= 1 && height <= qaScreenshotMaxSide
}
