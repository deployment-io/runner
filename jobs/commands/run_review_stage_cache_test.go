package commands

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/types"
	commandUtils "github.com/deployment-io/deployment-runner/jobs/commands/utils"
)

// --- the Step's dependency cache spans the whole Step -------------------------
//
// The per-Step cache volume holds the vendored dependencies AND the compiler's
// own cache. RunAgentStep used to remove it when it returned, so the first
// review round ran with nothing vendored — a reviewer that built a repository
// with private modules failed on blocked github.com fetches — and every fix run
// compiled the project from scratch after the stage vendored again.

// The implement run hands the volume on ONLY when a review will actually use it,
// and only when the Step got that far. Anything else and the run that created it
// removes it, or a Step leaks a volume nothing collects.
func TestTheImplementRunKeepsItsCacheVolumeOnlyForAReviewThatFollows(t *testing.T) {
	for _, tc := range []struct {
		name          string
		runErr        error
		participation int64
		want          bool
	}{
		{"review is on and the run succeeded", nil, participationOn, true},
		{"review is advisory and the run succeeded", nil, participationAdvisory, true},
		{"review is off", nil, participationOff, false},
		{"the Job predates the review stage", nil, 0, false},
		{"the implement run failed", errors.New("agent step did not succeed"), participationOn, false},
		{"the implement run was stopped", types.ErrJobStoppedByUser, participationOn, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parameters := map[string]interface{}{}
			if tc.participation != 0 {
				jobs.SetParameterValue[int64](parameters, parameters_enums.ReviewParticipation, tc.participation)
			}
			if got := keepCacheVolumeForReview(tc.runErr, parameters); got != tc.want {
				t.Errorf("keepCacheVolumeForReview = %t, want %t", got, tc.want)
			}
		})
	}
}

// The stage takes over the volume the implement run left, under the Step's own
// name — and a fix run then builds against it WITHOUT vendoring again, which is
// the whole point of keeping it.
func TestTheStageAdoptsTheCacheVolumeTheImplementRunLeft(t *testing.T) {
	ctx := commandUtils.TaskJobContext{TaskID: "task-1", StepIndex: 2}
	var logs strings.Builder
	created := 0
	name, kept := adoptCacheVolume(ctx, func(string) bool { return true }, func(string) error {
		created++
		return nil
	}, &logs)

	if name != "agentbox-cache-task-1-2" {
		t.Errorf("volume = %q, want the Step's own name", name)
	}
	if !kept {
		t.Error("the stage did not recognise the cache the implement run left in place")
	}
	if created != 0 {
		t.Error("the stage created a volume over the one the implement run left")
	}

	var fixLogs strings.Builder
	stage := &reviewStage{vendored: kept, logsWriter: &fixLogs}
	if err := stage.ensureVendoredCache("agentbox:1.9.22", t.TempDir()); err != nil {
		t.Fatalf("ensureVendoredCache: %s", err)
	}
	if fixLogs.Len() != 0 {
		t.Errorf("the first fix run vendored again over the implement run's cache: %s", fixLogs.String())
	}

	// The FALLBACK: the volume is gone — an implement run from an older runner,
	// or one whose volume something else collected. It is created empty and the
	// first fix run vendors into it, exactly as before.
	logs.Reset()
	name, kept = adoptCacheVolume(ctx, func(string) bool { return false }, func(string) error {
		created++
		return nil
	}, &logs)
	if kept {
		t.Error("a missing volume was reported as the implement run's own")
	}
	if created != 1 {
		t.Errorf("the missing volume was created %d time(s), want once", created)
	}
	if !strings.Contains(logs.String(), "vendor again") {
		t.Errorf("the job log does not say the cache will have to be filled again:\n%s", logs.String())
	}
}

// The stage owns the volume from the moment it starts, so it removes it EXACTLY
// ONCE however the stage ends — and never while a later run in the same Step
// still needs what is on it.
func TestTheStageRemovesTheCacheVolumeExactlyOnceHoweverItEnds(t *testing.T) {
	const volume = "agentbox-cache-task-1-0"
	for _, tc := range []struct {
		name    string
		arrange func(t *testing.T, stage *reviewStage, repoDir string, removed *[]string)
		wantErr error
	}{
		{
			name: "the stage finished its loop",
			arrange: func(t *testing.T, stage *reviewStage, repoDir string, removed *[]string) {
				fixRuns := 0
				stage.runFix = func([]reviewFindingOutput) error {
					fixRuns++
					// A fix run BUILDS, so the cache must still be there while
					// it does.
					if len(*removed) != 0 {
						t.Errorf("the cache volume was removed before fix round %d ran", fixRuns)
					}
					writeFile(t, filepath.Join(repoDir, "handler.go"), fmt.Sprintf("fix round %d\n", fixRuns))
					return nil
				}
			},
		},
		{
			name: "the review round did not complete",
			arrange: func(_ *testing.T, stage *reviewStage, _ string, _ *[]string) {
				stage.runReview = func(int) (agentResult, error) {
					return agentResult{}, errors.New("the agentbox image could not be pulled")
				}
			},
		},
		{
			name: "the user stopped the stage",
			arrange: func(_ *testing.T, stage *reviewStage, _ string, _ *[]string) {
				stage.runReview = func(int) (agentResult, error) {
					return agentResult{}, fmt.Errorf("error waiting for the review run: %w", types.ErrJobStoppedByUser)
				}
			},
			wantErr: types.ErrJobStoppedByUser,
		},
		{
			name: "a fix round was rolled back",
			arrange: func(t *testing.T, stage *reviewStage, repoDir string, _ *[]string) {
				stage.runFix = func([]reviewFindingOutput) error {
					writeFile(t, filepath.Join(repoDir, "handler.go"), "half a fix\n")
					return fixRunOutcome(agentResult{Status: "failure", Error: "max turns reached"}, io.Discard)
				}
			},
		},
		{
			name: "a fix round changed nothing",
			arrange: func(_ *testing.T, stage *reviewStage, _ string, _ *[]string) {
				stage.runFix = func([]reviewFindingOutput) error { return nil }
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workDir := t.TempDir()
			repoDir := filepath.Join(workDir, "0-acme/api")
			writeFile(t, filepath.Join(repoDir, "handler.go"), "the implementation\n")
			stage := fixLoopStage(workDir, io.Discard)
			var removed []string
			stage.cacheVolume = volume
			stage.removeCache = func(name string) { removed = append(removed, name) }
			tc.arrange(t, stage, repoDir, &removed)

			_, err := stage.run()
			if tc.wantErr == nil && err != nil {
				t.Fatalf("run: %s", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("run returned %v, want %v", err, tc.wantErr)
			}
			if len(removed) != 1 || removed[0] != volume {
				t.Errorf("the stage removed %v, want the Step's cache volume exactly once", removed)
			}
		})
	}
}
