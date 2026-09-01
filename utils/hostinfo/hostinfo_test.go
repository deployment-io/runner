package hostinfo

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadMemTotalBytes covers the parse that container sizing is built
// on. A misread here would silently mis-size every container the runner
// spawns by orders of magnitude, so malformed input must return 0 (which
// makes the caller apply the 8 GB fallback) rather than a wrong number.
func TestReadMemTotalBytes(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    int64
	}{
		{
			name:    "typical meminfo",
			content: "MemTotal:       16116496 kB\nMemFree:         1234 kB\n",
			want:    16116496 * 1024,
		},
		{
			name:    "MemTotal not first line",
			content: "SwapTotal:      0 kB\nMemTotal:       8046736 kB\n",
			want:    8046736 * 1024,
		},
		{
			name: "missing MemTotal",
			// A procfs without the line at all must not be guessed at.
			content: "MemFree:         1234 kB\n",
			want:    0,
		},
		{
			name: "unexpected unit",
			// Refusing an unknown unit is the whole point: silently
			// treating an mB value as kB would be a 1000x error.
			content: "MemTotal:       16116496 mB\n",
			want:    0,
		},
		{
			name:    "no unit suffix",
			content: "MemTotal:       16116496\n",
			want:    0,
		},
		{
			name:    "non-numeric value",
			content: "MemTotal:       lots kB\n",
			want:    0,
		},
		{
			name:    "zero value",
			content: "MemTotal:       0 kB\n",
			want:    0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "meminfo")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("write meminfo: %v", err)
			}
			if got := readMemTotalBytes(path); got != tc.want {
				t.Errorf("readMemTotalBytes() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestReadMemTotalBytes_MissingFile(t *testing.T) {
	if got := readMemTotalBytes(filepath.Join(t.TempDir(), "does-not-exist")); got != 0 {
		t.Errorf("readMemTotalBytes(missing) = %d, want 0 so the caller falls back", got)
	}
}

// TestMemoryBytesAlwaysPositive guards the fallback path: callers divide
// by and clamp against this value, so it must never be zero even on a
// machine with no readable procfs (every dev Mac, for instance).
func TestMemoryBytesAlwaysPositive(t *testing.T) {
	if got := MemoryBytes(); got <= 0 {
		t.Errorf("MemoryBytes() = %d, want a positive value", got)
	}
}

func TestCPUCoresAlwaysPositive(t *testing.T) {
	if got := CPUCores(); got < 1 {
		t.Errorf("CPUCores() = %d, want at least 1", got)
	}
}
