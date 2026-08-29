package commands

import (
	"strings"
	"testing"
)

// TestDescribeContainerExit pins what each death is allowed to claim.
//
// The distinction that matters is certainty. Docker's OOMKilled flag is a
// fact and is stated as one; a bare 137 is a strong suspicion and must be
// worded as one, because the kernel can kill a process INSIDE the container
// without flagging the container — which is exactly the case that went
// undiagnosed on 2026-08-28.
func TestDescribeContainerExit(t *testing.T) {
	cases := []struct {
		name       string
		oomKilled  bool
		exitCode   int
		wantEmpty  bool
		wantHas    []string
		wantNotHas []string
	}{
		{
			name: "ordinary success says nothing", exitCode: 0, wantEmpty: true,
		},
		{
			// A normal failure explains itself elsewhere; narrating it here
			// would bury the real error under noise.
			name: "ordinary failure says nothing", exitCode: 1, wantEmpty: true,
		},
		{
			name: "docker flagged an OOM", oomKilled: true, exitCode: 137,
			wantHas:    []string{"OOM-killed by the kernel", "AGENTBOX_MEMORY_BYTES"},
			wantNotHas: []string{"likeliest"}, // a flag is proof, not a guess
		},
		{
			name: "SIGKILL with no OOM flag", exitCode: 137,
			wantHas:    []string{"137", "SIGKILL", "likeliest cause", "without the container itself being flagged"},
			wantNotHas: []string{"was OOM-killed by the kernel"}, // must not assert what Docker did not
		},
		{
			name:     "SIGTERM",
			exitCode: 143,
			wantHas:  []string{"143", "SIGTERM"},
		},
		{
			name:     "some other signal is named numerically",
			exitCode: 139, // 128+11, SIGSEGV
			wantHas:  []string{"signal 11", "139"},
		},
		{
			// OOMKilled outranks the exit code: a container OOM-killed but
			// reporting a plain code must still be reported as an OOM.
			name: "oom flag wins over an unremarkable code", oomKilled: true, exitCode: 1,
			wantHas: []string{"OOM-killed"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := describeContainerExit(tc.oomKilled, tc.exitCode)
			if tc.wantEmpty {
				if got != "" {
					t.Fatalf("got %q, want silence — an exit that explains itself needs no narration", got)
				}
				return
			}
			if got == "" {
				t.Fatal("got silence, want an explanation")
			}
			for _, want := range tc.wantHas {
				if !strings.Contains(got, want) {
					t.Errorf("message %q is missing %q", got, want)
				}
			}
			for _, notWant := range tc.wantNotHas {
				if strings.Contains(got, notWant) {
					t.Errorf("message %q must not contain %q", got, notWant)
				}
			}
		})
	}
}

// A nil client or empty id must not panic on the path that reports a job's
// result — this is a diagnostic, and a diagnostic that can crash the reporter
// is worse than no diagnostic.
func TestReportContainerExitIsSafeWithNoClient(t *testing.T) {
	var sb strings.Builder
	reportContainerExit(nil, "abc", &sb)
	reportContainerExit(nil, "", &sb)
	if sb.String() != "" {
		t.Errorf("wrote %q with no daemon to ask", sb.String())
	}
}
