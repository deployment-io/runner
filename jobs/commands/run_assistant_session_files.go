package commands

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// sessionInputDirRel is agentbox's input dir (system prompt, user turns),
// relative to the session work dir.
const sessionInputDirRel = ".agentbox-input"

// sessionWritableDirsRel are the only directories, relative to the session
// work dir, that an assistant session's container may write: agentbox deletes
// each input message after reading it, writes its output, and keeps its scratch
// (TMPDIR, COREPACK_HOME) in the third. Everything else under /work is
// read-only to it — see agentboxSpawnSpec.readOnlyWorkDir.
//
// Because the container can write these, it can plant symbolic links in them,
// and the runner reads and writes there as root. Every host-side read and write
// the session makes under them goes through readNoFollow / atomicWrite, which
// refuse a link anywhere between the work dir and the file rather than follow
// it.
var sessionWritableDirsRel = []string{sessionInputDirRel, agentboxResultDirRel, agentboxTmpDirRel}

// errNotRegularFile is a session file that isn't a regular file reached
// without a symbolic link: the file itself, or a directory above it, is a
// link (or a FIFO, device, …). The session treats it as absent.
var errNotRegularFile = errors.New("not a regular file")

// pathBelow returns path's components below root, or an error when path is not
// strictly below root.
func pathBelow(root, path string) ([]string, error) {
	if root == "" {
		return nil, fmt.Errorf("no work dir to resolve %s against", path)
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("%s is not under %s", path, root)
	}
	return strings.Split(rel, string(filepath.Separator)), nil
}

// readNoFollow reads path, which lies under root, only when it and every
// directory between it and root are not symbolic links and it is a regular
// file; it is opened with O_NOFOLLOW (and non-blocking, so a FIFO can't hang
// the reader). Anything else is errNotRegularFile; a missing file or directory
// is fs.ErrNotExist, as from os.ReadFile.
func readNoFollow(root, path string) ([]byte, error) {
	parts, err := pathBelow(root, path)
	if err != nil {
		return nil, err
	}
	if err := checkDirsNoFollow(root, parts[:len(parts)-1], false); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errNotRegularFile
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, errNotRegularFile // swapped for a link since the Lstat
		}
		return nil, err
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil {
		return nil, err
	} else if !info.Mode().IsRegular() {
		return nil, errNotRegularFile
	}
	return io.ReadAll(f)
}

// checkDirsNoFollow Lstats each directory root/dirs[0], root/dirs[0]/dirs[1], …
// and refuses (errNotRegularFile) any that is a link or not a directory. With
// create, a missing one is made with Mkdir — never through a link; without, it
// is fs.ErrNotExist.
func checkDirsNoFollow(root string, dirs []string, create bool) error {
	cur := root
	for _, d := range dirs {
		cur = filepath.Join(cur, d)
		info, err := os.Lstat(cur)
		if create && errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(cur, 0755); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%s: %w", cur, errNotRegularFile)
		}
	}
	return nil
}

// atomicWrite writes dir/name (name may hold subdirectories) via temp+rename.
// dir lies under root, the session work dir; no directory between root and the
// file may be a symbolic link (a missing one is created, never through a link),
// and the temporary file is created exclusively and without following a link,
// so a file or link planted at its name fails the write instead of being
// overwritten or followed. The rename replaces a link at the final name rather
// than writing through it.
func atomicWrite(root, dir, name string, data []byte) error {
	dest := filepath.Join(dir, name)
	parts, err := pathBelow(root, dest)
	if err != nil {
		return err
	}
	if err := checkDirsNoFollow(root, parts[:len(parts)-1], true); err != nil {
		return err
	}
	tmp := dest + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0644)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, dest)
	}
	if err != nil {
		_ = os.Remove(tmp) // ours: O_EXCL created it
		return err
	}
	return nil
}

// sessionFiles reads the files agentbox writes under the session's output dir
// without following links (readNoFollow), and logs a refused one once per
// session rather than on every tick. Shared by the forwarders, which tick on
// their own goroutines, hence the lock.
type sessionFiles struct {
	root       string // the session work dir
	logsWriter io.Writer
	mu         sync.Mutex
	refused    map[string]bool
}

func newSessionFiles(root string, logsWriter io.Writer) *sessionFiles {
	return &sessionFiles{root: root, logsWriter: logsWriter, refused: map[string]bool{}}
}

// read is readNoFollow, logging "session: ignored <name>: not a regular file"
// the first time path is refused.
func (sf *sessionFiles) read(path string) ([]byte, error) {
	b, err := readNoFollow(sf.root, path)
	if errors.Is(err, errNotRegularFile) {
		sf.logRefused(path, "not a regular file")
	}
	return b, err
}

// checkDir reports whether dir, under root, and every directory above it are
// real directories; a link is logged once ("not a directory") and refused.
func (sf *sessionFiles) checkDir(dir string) bool {
	parts, err := pathBelow(sf.root, dir)
	if err != nil {
		return false
	}
	err = checkDirsNoFollow(sf.root, parts, false)
	if errors.Is(err, errNotRegularFile) {
		sf.logRefused(dir, "not a directory")
	}
	return err == nil
}

// logRefused logs path's base name once per session; it dedupes on the full
// path, so two refused files sharing a name each get their line.
func (sf *sessionFiles) logRefused(path, why string) {
	sf.mu.Lock()
	defer sf.mu.Unlock()
	if sf.refused[path] {
		return
	}
	sf.refused[path] = true
	io.WriteString(sf.logsWriter, fmt.Sprintf("session: ignored %s: %s\n", filepath.Base(path), why))
}

// readAgentResult is readAgentResult for a session: result.json is read only
// when it is a regular file reached without a link.
func (sf *sessionFiles) readAgentResult() (agentResult, error) {
	data, err := sf.read(filepath.Join(sf.root, agentboxResultDirRel, agentboxResultFile))
	if err != nil {
		return agentResult{}, err
	}
	return parseAgentResult(data)
}
