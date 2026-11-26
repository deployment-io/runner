package dependencies

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

// ReadBoundedRegularFile reads path when it is a regular file of at most max
// bytes: never through a symlink, never a FIFO or device (opened non-blocking,
// so a FIFO can't hang it). A missing file is fs.ErrNotExist; anything else
// that isn't such a file is another error.
func ReadBoundedRegularFile(path string, max int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file (%s)", info.Mode().Type())
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// Re-checked on the opened file: the path may have been swapped since Lstat.
	if info, err := f.Stat(); err != nil {
		return nil, err
	} else if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file (%s)", info.Mode().Type())
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("larger than %d bytes", max)
	}
	return data, nil
}
