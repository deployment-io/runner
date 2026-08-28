package reclaim

import (
	"bytes"
	"strings"
	"testing"
)

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		in   uint64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{2048, "2.0 KiB"},
		{10 << 30, "10.0 GiB"},
		{1<<30 + 1<<29, "1.5 GiB"},
	}
	for _, test := range tests {
		if got := humanBytes(test.in); got != test.want {
			t.Errorf("humanBytes(%d) = %q, want %q", test.in, got, test.want)
		}
	}
}

func TestLowWaterBytes(t *testing.T) {
	tests := []struct {
		name  string
		env   string
		want  uint64
		unset bool
	}{
		{name: "default when unset", unset: true, want: defaultLowWaterBytes},
		{name: "override", env: "1073741824", want: 1 << 30},
		{name: "garbage falls back rather than failing a job", env: "plenty", want: defaultLowWaterBytes},
		{name: "zero falls back", env: "0", want: defaultLowWaterBytes},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if !test.unset {
				t.Setenv(lowWaterBytesEnvVar, test.env)
			}
			if got := lowWaterBytes(); got != test.want {
				t.Errorf("lowWaterBytes() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestBuildCacheKeepBytes(t *testing.T) {
	if got := buildCacheKeepBytes(); got != defaultBuildCacheKeepBytes {
		t.Errorf("default keep = %d, want %d", got, defaultBuildCacheKeepBytes)
	}
	t.Setenv(buildCacheKeepBytesEnvVar, "0")
	if got := buildCacheKeepBytes(); got != 0 {
		t.Errorf("an explicit zero budget must be honored, got %d", got)
	}
	t.Setenv(buildCacheKeepBytesEnvVar, "-1")
	if got := buildCacheKeepBytes(); got != defaultBuildCacheKeepBytes {
		t.Errorf("a negative budget must fall back, got %d", got)
	}
}

// A runner should say how much room it has before a job dies, and say it
// loudly once it is nearly out.
func TestLogFreeDiskWarnsBelowTheLowWaterMark(t *testing.T) {
	if _, ok := freeDiskBytes(t.TempDir()); !ok {
		t.Skip("no statfs on this platform")
	}
	t.Setenv(lowWaterBytesEnvVar, "18446744073709551615") // everything is below this
	var logs bytes.Buffer
	LogFreeDisk("before build", &logs)
	if !strings.Contains(logs.String(), "free on") {
		t.Errorf("expected a free-space line, got %q", logs.String())
	}
	if !strings.Contains(logs.String(), "WARNING") {
		t.Errorf("expected a low-water warning, got %q", logs.String())
	}
}

func TestReclaimingLogsBeforeAndAfter(t *testing.T) {
	if _, ok := freeDiskBytes(diskPath); !ok {
		t.Skip("no statfs on this platform")
	}
	t.Setenv(lowWaterBytesEnvVar, "1") // nothing is below this
	var logs bytes.Buffer
	ran := false
	reclaiming(&logs, "local image tags", func() { ran = true })
	if !ran {
		t.Fatal("the reclaim body must run")
	}
	if got := strings.Count(logs.String(), "free on"); got != 2 {
		t.Errorf("expected free space logged before and after, got %d lines: %q", got, logs.String())
	}
	if strings.Contains(logs.String(), "WARNING") {
		t.Errorf("unexpected low-water warning: %q", logs.String())
	}
}
