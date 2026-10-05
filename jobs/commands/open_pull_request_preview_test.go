package commands

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/task_previews"
)

type stubPreviewLister struct {
	previews []task_previews.TaskPreviewV1
	err      error
}

func (s stubPreviewLister) ListTaskPreviews(string, string) ([]task_previews.TaskPreviewV1, error) {
	return s.previews, s.err
}

const previewFootnote = "A preview shows the code as of the agent's last deploy to it, which can be earlier than this pull request's final commit. Previews are removed about 72 hours after the Task's last preview deploy, or when the Task is deleted."

func previewOpener(lister stubPreviewLister) *taskOpenPR {
	opr := reviewTestOpener(completedReview(false))
	opr.taskPreviews = lister
	return opr
}

func TestPreviewSection_OnePreview(t *testing.T) {
	_, body := previewOpener(stubPreviewLister{previews: []task_previews.TaskPreviewV1{
		{ServiceName: "acme-web-dist", ServiceType: task_previews.ServiceTypeStaticSite, URL: "https://d1abc.cloudfront.net"},
	}}).buildPRTitleAndBody()
	want := "**Preview**\n\n" +
		"The agent deployed this Task's change to a preview while working on it:\n\n" +
		"- acme-web-dist: https://d1abc.cloudfront.net\n\n" +
		previewFootnote
	if !strings.Contains(body, want) {
		t.Fatalf("body lacks the Preview part.\nwant:\n%s\n\ngot:\n%s", want, body)
	}
	// Directly after the tally line, before the summary.
	tally := reviewTally(completedReview(false))
	if !strings.HasPrefix(body, tally+"\n\n"+want+"\n\n") {
		t.Fatalf("Preview part is not directly after the tally:\n%s", body)
	}
}

func TestPreviewSection_SixPreviews(t *testing.T) {
	var previews []task_previews.TaskPreviewV1
	for i := 1; i <= 6; i++ {
		previews = append(previews, task_previews.TaskPreviewV1{
			ServiceName: fmt.Sprintf("svc-%d", i),
			URL:         fmt.Sprintf("https://d%d.cloudfront.net", i),
		})
	}
	got := formatPreviewSection(previews)
	want := "**Preview**\n\n" +
		"The agent deployed this Task's change to a preview while working on it:\n\n" +
		"- svc-1: https://d1.cloudfront.net\n" +
		"- svc-2: https://d2.cloudfront.net\n" +
		"- svc-3: https://d3.cloudfront.net\n" +
		"- svc-4: https://d4.cloudfront.net\n" +
		"- svc-5: https://d5.cloudfront.net\n" +
		"- and 1 more on the Task page\n\n" +
		previewFootnote + "\n"
	if got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestPreviewSection_LongServiceName(t *testing.T) {
	name := strings.Repeat("a", 150)
	got := formatPreviewSection([]task_previews.TaskPreviewV1{{ServiceName: name, URL: "https://d1.cloudfront.net"}})
	if !strings.Contains(got, "- "+strings.Repeat("a", 100)+": https://d1.cloudfront.net\n") {
		t.Fatalf("name not cut to 100 characters:\n%s", got)
	}
	if strings.Contains(got, strings.Repeat("a", 101)) {
		t.Fatalf("name longer than 100 characters kept:\n%s", got)
	}
}

func TestPreviewSection_AbsentWithoutPreviews(t *testing.T) {
	for name, lister := range map[string]stubPreviewLister{
		"no previews":  {},
		"lister error": {err: errors.New("rpc: can't find method TaskPreviews.ListV1")},
	} {
		t.Run(name, func(t *testing.T) {
			_, body := previewOpener(lister).buildPRTitleAndBody()
			if strings.Contains(body, "**Preview**") || strings.Contains(body, "preview while working") {
				t.Fatalf("Preview part should be absent:\n%s", body)
			}
		})
	}
}
