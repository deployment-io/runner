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

const (
	previewSentence        = "Previews are temporary: they are removed once this Task's pull requests are merged or closed, or after they sit idle (about 72 hours).\n"
	backendPreviewSentence = "Previews are temporary: they are removed once this Task's pull requests are merged or closed, or after they sit idle (about 72 hours; backend previews about 4).\n"
)

func previewOpener(lister stubPreviewLister) *taskOpenPR {
	opr := reviewTestOpener(completedReview(false))
	opr.taskPreviews = lister
	return opr
}

func TestPreviewSection_OnePreview(t *testing.T) {
	_, body := previewOpener(stubPreviewLister{previews: []task_previews.TaskPreviewV1{
		{ServiceName: "acme-web-dist", ServiceType: task_previews.ServiceTypeStaticSite, URL: "https://d1abc.cloudfront.net"},
	}}).buildPRTitleAndBody()
	want := "**Previews**\n\n" +
		"- acme-web-dist: https://d1abc.cloudfront.net\n\n" +
		previewSentence
	// After the summary, directly before the trailer.
	if !strings.Contains(body, "Added the login endpoint.\n\n"+want+"\n---\nGenerated-By: deployment.io Tasks\n") {
		t.Fatalf("the Previews part is not between the summary and the trailer.\nwant:\n%s\n\ngot:\n%s", want, body)
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
	want := "**Previews**\n\n" +
		"- svc-1: https://d1.cloudfront.net\n" +
		"- svc-2: https://d2.cloudfront.net\n" +
		"- svc-3: https://d3.cloudfront.net\n" +
		"- svc-4: https://d4.cloudfront.net\n" +
		"- svc-5: https://d5.cloudfront.net\n" +
		"- and 1 more under Resources → Previews in the dashboard\n\n" +
		previewSentence
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
			if strings.Contains(body, "**Previews**") || strings.Contains(body, "Previews are temporary") {
				t.Fatalf("Previews part should be absent:\n%s", body)
			}
		})
	}
}

// A web or private service preview is removed after 4 idle hours; the one
// sentence says so. Static previews alone don't.
func TestPreviewSection_BackendSentence(t *testing.T) {
	for _, tc := range []struct {
		name        string
		serviceType string
		want        string
	}{
		{"web service", task_previews.ServiceTypeWebService, backendPreviewSentence},
		{"private service", task_previews.ServiceTypePrivateService, backendPreviewSentence},
		{"static site only", task_previews.ServiceTypeStaticSite, previewSentence},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := formatPreviewSection([]task_previews.TaskPreviewV1{
				{ServiceName: "web", ServiceType: task_previews.ServiceTypeStaticSite, URL: "https://d1.cloudfront.net"},
				{ServiceName: "svc", ServiceType: tc.serviceType, URL: "https://d2.cloudfront.net"},
			})
			want := "**Previews**\n\n- web: https://d1.cloudfront.net\n- svc: https://d2.cloudfront.net\n\n" + tc.want
			if got != want {
				t.Fatalf("got:\n%q\nwant:\n%q", got, want)
			}
		})
	}
}
