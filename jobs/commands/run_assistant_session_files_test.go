package commands

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deployment-io/deployment-runner-kit/sessions"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

// An assistant session is read-only, and the kernel says so: /work is bound
// read-only and only the three directories agentbox must write are bound back
// read-write on top of it, each from its own host directory.
func TestTheSessionSpawnMountsTheWorkDirReadOnly(t *testing.T) {
	workDir := t.TempDir()
	spec := sessionSpawnSpec(sessionRun{imageRef: "agentbox:1", workDirHost: workDir, envVars: []string{"A=1"}}, 1<<30)
	if spec.imageRef != "agentbox:1" || spec.workDirHost != workDir || spec.memoryBytes != 1<<30 || len(spec.env) != 1 {
		t.Errorf("spec = %+v", spec)
	}
	mounts := agentboxMounts(spec)

	work := findMount(t, mounts, agentboxWorkDirInContainer)
	if !work.ReadOnly || work.Type != mount.TypeBind || work.Source != workDir {
		t.Errorf("/work = %+v, want a read-only bind from %s", work, workDir)
	}
	writable := map[string]string{
		"/work/.agentbox-input":  filepath.Join(workDir, ".agentbox-input"),
		"/work/.agentbox-output": filepath.Join(workDir, ".agentbox-output"),
		"/work/.agentbox-tmp":    filepath.Join(workDir, ".agentbox-tmp"),
	}
	for target, source := range writable {
		m := findMount(t, mounts, target)
		if m.ReadOnly || m.Type != mount.TypeBind || m.Source != source {
			t.Errorf("%s = %+v, want a read-write bind from %s", target, m, source)
		}
	}
	if len(mounts) != 1+len(writable) {
		t.Errorf("%d mounts, want /work and the three writable dirs only:\n%+v", len(mounts), mounts)
	}
}

// Every other spawn keeps a writable /work and gets none of the session's binds.
func TestOnlyTheSessionSpawnMountsTheWorkDirReadOnly(t *testing.T) {
	for _, spec := range []agentboxSpawnSpec{
		{workDirHost: "/w", cacheVolume: "c", mcpSocketHost: "/s"},
		{workDirHost: "/w", cmd: []string{"vendor"}, cacheVolume: "c"},
		{workDirHost: "/w", readOnlyRepoDirs: []string{"0-acme/api"}, readOnlyContextDir: "/ctx"},
	} {
		mounts := agentboxMounts(spec)
		if findMount(t, mounts, agentboxWorkDirInContainer).ReadOnly {
			t.Errorf("spec %+v: /work is read-only", spec)
		}
		for _, m := range mounts {
			if strings.HasPrefix(m.Target, "/work/.agentbox") {
				t.Errorf("spec %+v: unexpected session bind %+v", spec, m)
			}
		}
	}
}

// sessionWorkDir is a session work dir as prepareSessionDirs leaves it, and an
// outside dir holding a file the container would like the runner to read.
func sessionWorkDir(t *testing.T, outsideContent string) (workDir, outside, outsideFile string) {
	t.Helper()
	workDir = t.TempDir()
	if err := prepareSessionDirsForTest(workDir); err != nil {
		t.Fatal(err)
	}
	outside = t.TempDir()
	outsideFile = filepath.Join(outside, "secret.json")
	if err := os.WriteFile(outsideFile, []byte(outsideContent), 0644); err != nil {
		t.Fatal(err)
	}
	return workDir, outside, outsideFile
}

func prepareSessionDirsForTest(workDir string) error {
	for _, d := range []string{".agentbox-output/messages", ".agentbox-input/messages", ".agentbox-tmp", sessionUploadsDirRel} {
		if err := os.MkdirAll(filepath.Join(workDir, d), 0755); err != nil {
			return err
		}
	}
	return nil
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// A message file that is a link to a valid message outside the work dir is
// never read: skipped, marked seen, logged once.
func TestMessageForwarderIgnoresALinkedMessageFile(t *testing.T) {
	workDir, _, outsideFile := sessionWorkDir(t, `{"type":"chunk","text":"from outside"}`)
	dir := filepath.Join(workDir, ".agentbox-output", "messages")
	symlink(t, outsideFile, filepath.Join(dir, "0000000001.json"))
	logs := &bytes.Buffer{}
	mf := &messageForwarder{dir: dir, files: newSessionFiles(workDir, logs), logsWriter: logs, seen: map[string]bool{}}
	mf.tick()
	mf.tick()
	if !mf.seen["0000000001.json"] {
		t.Error("the linked file was not marked seen")
	}
	if want := "session: ignored 0000000001.json: not a regular file\n"; logs.String() != want {
		// "error forwarding" here would mean the target's content was read.
		t.Errorf("logs = %q, want exactly %q", logs.String(), want)
	}
}

// A regular message file is read and forwarded as today (here the forward
// fails — no server — so it stays unseen for a retry).
func TestMessageForwarderReadsARegularMessageFile(t *testing.T) {
	workDir, _, _ := sessionWorkDir(t, "")
	dir := filepath.Join(workDir, ".agentbox-output", "messages")
	writeFile(t, filepath.Join(dir, "0000000001.json"), `{"type":"chunk","text":"hi"}`)
	logs := &bytes.Buffer{}
	mf := &messageForwarder{dir: dir, files: newSessionFiles(workDir, logs), logsWriter: logs, seen: map[string]bool{}}
	mf.tick()
	if mf.seen["0000000001.json"] || !strings.Contains(logs.String(), "session: error forwarding messages") {
		t.Errorf("seen=%v logs=%q, want the file read and its forward attempted", mf.seen, logs.String())
	}
}

// The messages dir itself a link: nothing in it is read.
func TestMessageForwarderReadsNothingThroughALinkedDir(t *testing.T) {
	workDir, outside, _ := sessionWorkDir(t, `{"type":"chunk","text":"from outside"}`)
	writeFile(t, filepath.Join(outside, "0000000001.json"), `{"type":"chunk","text":"from outside"}`)
	dir := filepath.Join(workDir, ".agentbox-output", "messages")
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	symlink(t, outside, dir)
	logs := &bytes.Buffer{}
	mf := &messageForwarder{dir: dir, files: newSessionFiles(workDir, logs), logsWriter: logs, seen: map[string]bool{}}
	mf.tick()
	mf.tick()
	if len(mf.seen) != 0 || strings.Contains(logs.String(), "forwarding") {
		t.Errorf("read through a linked dir: seen=%v logs=%q", mf.seen, logs.String())
	}
	if strings.Count(logs.String(), "session: ignored messages") != 1 {
		t.Errorf("logs = %q, want one line for the linked dir", logs.String())
	}
}

// task-spec.json, repo-suggestion.json and result.json as links read as not
// written: never forwarded, never logged, one line each per session.
func TestSessionOutputFilesAsLinksReadAsNotWritten(t *testing.T) {
	workDir, outside, _ := sessionWorkDir(t, "")
	out := filepath.Join(workDir, ".agentbox-output")
	spec := filepath.Join(outside, "spec.json")
	writeFile(t, spec, `{"title":"from outside","goal":"g"}`)
	suggestion := filepath.Join(outside, "suggestion.json")
	writeFile(t, suggestion, oneRepoSuggestion)
	result := filepath.Join(outside, "result.json")
	writeFile(t, result, `{"status":"secret-status","error":"secret-error"}`)
	symlink(t, spec, filepath.Join(out, "task-spec.json"))
	symlink(t, suggestion, filepath.Join(out, "repo-suggestion.json"))
	symlink(t, result, filepath.Join(out, "result.json"))

	logs := &bytes.Buffer{}
	files := newSessionFiles(workDir, logs)
	sf := &specForwarder{path: filepath.Join(out, "task-spec.json"), files: files, logsWriter: logs}
	sink := &suggestionSink{}
	rf := &repoSuggestionForwarder{path: filepath.Join(out, "repo-suggestion.json"), files: files, logsWriter: logs, send: sink.send}
	for i := 0; i < 3; i++ {
		sf.tick()
		rf.tick()
	}
	logSessionOutcome(files, logs)
	msg := sessionAgentFailureMessage(files)

	if len(sink.sent) != 0 || sf.lastContent != "" || rf.lastContent != "" {
		t.Errorf("a linked file was forwarded: %+v", sink.sent)
	}
	if strings.Contains(msg, "secret") {
		t.Errorf("failure message read through the link: %q", msg)
	}
	want := "session: ignored task-spec.json: not a regular file\n" +
		"session: ignored repo-suggestion.json: not a regular file\n" +
		"session: ignored result.json: not a regular file\n"
	if logs.String() != want {
		t.Errorf("logs = %q, want %q", logs.String(), want)
	}
}

// Two refused files sharing a base name are each logged once: the log is
// deduplicated per file, not per name.
func TestRefusedFilesSharingANameAreEachLogged(t *testing.T) {
	workDir, outside, _ := sessionWorkDir(t, "")
	out := filepath.Join(workDir, ".agentbox-output")
	target := filepath.Join(outside, "spec.json")
	writeFile(t, target, `{"title":"from outside"}`)
	symlink(t, target, filepath.Join(out, "task-spec.json"))
	symlink(t, target, filepath.Join(out, "messages", "task-spec.json"))

	logs := &bytes.Buffer{}
	files := newSessionFiles(workDir, logs)
	for i := 0; i < 2; i++ {
		files.read(filepath.Join(out, "task-spec.json"))
		files.read(filepath.Join(out, "messages", "task-spec.json"))
	}
	want := strings.Repeat("session: ignored task-spec.json: not a regular file\n", 2)
	if logs.String() != want {
		t.Errorf("logs = %q, want %q", logs.String(), want)
	}
}

// The same files written as regular files read exactly as today.
func TestSessionOutputFilesAsRegularFilesRead(t *testing.T) {
	workDir, _, _ := sessionWorkDir(t, "")
	out := filepath.Join(workDir, ".agentbox-output")
	writeFile(t, filepath.Join(out, "repo-suggestion.json"), oneRepoSuggestion)
	writeFile(t, filepath.Join(out, "task-spec.json"), `{"title":"t"}`)
	writeFile(t, filepath.Join(out, "result.json"), `{"status":"failed","error":"credits exhausted"}`)
	logs := &bytes.Buffer{}
	files := newSessionFiles(workDir, logs)
	sink := &suggestionSink{}
	rf := &repoSuggestionForwarder{path: filepath.Join(out, "repo-suggestion.json"), files: files, logsWriter: logs, send: sink.send}
	rf.tick()
	if len(sink.sent) != 1 {
		t.Errorf("suggestion not forwarded: %+v", sink.sent)
	}
	logSessionOutcome(files, logs)
	if want := "session ended: status=failed\nsession produced a task spec\n"; logs.String() != want {
		t.Errorf("logs = %q, want %q", logs.String(), want)
	}
	if msg := sessionAgentFailureMessage(files); msg != "credits exhausted" {
		t.Errorf("failure message = %q", msg)
	}
}

// The input dir (or uploads) replaced by a link to an outside dir: the write
// fails and nothing lands outside.
func TestInputPumpRefusesToWriteThroughALinkedDir(t *testing.T) {
	for _, linked := range []string{".agentbox-input/messages", ".agentbox-input", sessionUploadsDirRel} {
		t.Run(linked, func(t *testing.T) {
			ip, base := newTestPump(t)
			outside := t.TempDir()
			if err := os.RemoveAll(filepath.Join(base, linked)); err != nil {
				t.Fatal(err)
			}
			symlink(t, outside, filepath.Join(base, linked))
			m := sessions.UserMessageDtoV1{ID: "m1", Ts: 1, Content: "hi",
				Attachments: []sessions.SessionAttachmentDtoV1{{Name: "a.txt", Path: "abc-1-a.txt", Content: "x"}}}
			if ip.deliver(m) {
				t.Fatal("delivered through a linked dir")
			}
			if entries, _ := os.ReadDir(outside); len(entries) != 0 {
				t.Errorf("wrote outside the work dir: %v", entries)
			}
		})
	}
}

// A file or link planted at the temporary name fails the write and leaves the
// planted target unchanged.
func TestAtomicWriteRefusesAPlantedTempFile(t *testing.T) {
	ip, _ := newTestPump(t)
	outsideFile := filepath.Join(t.TempDir(), "target")
	writeFile(t, outsideFile, "original")
	symlink(t, outsideFile, filepath.Join(ip.dir, "0000000001.json.tmp"))
	if ip.deliver(sessions.UserMessageDtoV1{ID: "m1", Ts: 1, Content: "hi"}) {
		t.Error("delivered over a link planted at the temporary name")
	}
	if b, _ := os.ReadFile(outsideFile); string(b) != "original" {
		t.Errorf("link target = %q, want unchanged", b)
	}

	planted := filepath.Join(ip.uploadsDir, "abc-1-a.txt.tmp")
	writeFile(t, planted, "planted")
	err := ip.writeAttachment(sessions.SessionAttachmentDtoV1{Name: "a.txt", Path: "abc-1-a.txt", Content: "x"})
	if err == nil {
		t.Error("wrote over a file planted at the temporary name")
	}
	if b, _ := os.ReadFile(planted); string(b) != "planted" {
		t.Errorf("planted file = %q, want unchanged", b)
	}
}

// An attachment path with subdirectories is written as today, the missing
// directories created; a link among them is refused.
func TestAtomicWriteCreatesSubdirectoriesButNotThroughALink(t *testing.T) {
	ip, base := newTestPump(t)
	a := sessions.SessionAttachmentDtoV1{Name: "x.txt", Path: "deep/er/abc-1-x.txt", Content: "content"}
	if err := ip.writeAttachment(a); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(base, sessionUploadsDirRel, "deep", "er", "abc-1-x.txt")); err != nil || string(b) != "content" {
		t.Errorf("got (%q, %v)", b, err)
	}

	outside := t.TempDir()
	symlink(t, outside, filepath.Join(ip.uploadsDir, "linked"))
	if err := ip.writeAttachment(sessions.SessionAttachmentDtoV1{Path: "linked/sub/abc-2-y.txt", Content: "y"}); err == nil {
		t.Error("wrote through a linked subdirectory")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("wrote outside the work dir: %v", entries)
	}
}

// With a Docker daemon: a container started with the session's spec can't
// write a repository and can write its scratch dir.
func TestTheSessionContainerCannotWriteTheRepositories(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Skipf("no Docker client: %s", err)
	}
	defer cli.Close()
	if _, err := cli.Ping(ctx, client.PingOptions{NegotiateAPIVersion: true}); err != nil {
		t.Skipf("no Docker daemon: %s", err)
	}
	const image = "busybox:1.36"
	if reader, err := cli.ImagePull(ctx, "docker.io/library/"+image, client.ImagePullOptions{}); err != nil {
		t.Skipf("can't pull %s: %s", image, err)
	} else {
		_, _ = io.Copy(io.Discard, reader)
		reader.Close()
	}

	workDir, err := os.MkdirTemp("/tmp", "session-ro-") // a path the daemon can bind
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(workDir)
	if err := prepareSessionDirsForTest(workDir); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(workDir, "0-acme", "api")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	// World-writable, so only the read-only bind can stop UID 1000.
	for _, d := range []string{workDir, filepath.Join(workDir, "0-acme"), repo, filepath.Join(workDir, agentboxTmpDirRel)} {
		if err := os.Chmod(d, 0777); err != nil {
			t.Fatal(err)
		}
	}

	spec := sessionSpawnSpec(sessionRun{imageRef: image, workDirHost: workDir}, 0)
	spec.cmd = []string{"sh", "-c", "touch /work/0-acme/api/x && exit 1; touch /work/.agentbox-tmp/ok || exit 2"}
	id, err := createAgentboxContainer(ctx, cli, spec)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = removeContainer(context.Background(), cli, id) }()
	if _, err := cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		t.Fatal(err)
	}
	wait := cli.ContainerWait(ctx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case err := <-wait.Error:
		t.Fatal(err)
	case res := <-wait.Result:
		if res.StatusCode != 0 {
			t.Errorf("exit %d (1: the repository was writable; 2: the scratch dir wasn't)", res.StatusCode)
		}
	}
	if _, err := os.Lstat(filepath.Join(repo, "x")); err == nil {
		t.Error("the container wrote into the repository")
	}
	if _, err := os.Lstat(filepath.Join(workDir, agentboxTmpDirRel, "ok")); err != nil {
		t.Errorf("the container couldn't write its scratch dir: %s", err)
	}
}
