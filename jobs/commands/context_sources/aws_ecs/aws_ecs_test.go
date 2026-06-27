package aws_ecs

import "testing"

func TestImageRepoName(t *testing.T) {
	cases := map[string]string{
		"123.dkr.ecr.us-east-1.amazonaws.com/team/billing-api:v3": "billing-api",
		"123.dkr.ecr.us-east-1.amazonaws.com/billing-api:latest":  "billing-api",
		"ghcr.io/acme/web@sha256:abc123":                          "web",
		"billing-api:latest":                                      "billing-api",
		"billing-api":                                             "billing-api",
		"nginx":                                                   "nginx",
		"registry:5000/app:1.2.3":                                 "app", // port on host is not the last segment
		"":                                                        "",
	}
	for in, want := range cases {
		if got := imageRepoName(in); got != want {
			t.Errorf("imageRepoName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRecoverRepo(t *testing.T) {
	repo, by := recoverRepo("123.dkr.ecr.us-east-1.amazonaws.com/billing-api:v3")
	if repo != "billing-api" || by != "image-name" {
		t.Errorf("recoverRepo = (%q, %q), want (billing-api, image-name)", repo, by)
	}
	if repo, by := recoverRepo(""); repo != "" || by != "" {
		t.Errorf("recoverRepo(\"\") = (%q, %q), want empty", repo, by)
	}
}

func TestNameFromArn(t *testing.T) {
	cases := map[string]string{
		"arn:aws:ecs:us-east-1:123456789:cluster/prod":         "prod",
		"arn:aws:ecs:us-east-1:123456789:task-definition/web:7": "web:7", // last segment after the final slash
		"prod": "prod",
	}
	for in, want := range cases {
		if got := nameFromArn(in); got != want {
			t.Errorf("nameFromArn(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestChunk(t *testing.T) {
	got := chunk([]string{"a", "b", "c", "d", "e"}, 2)
	if len(got) != 3 || len(got[0]) != 2 || len(got[2]) != 1 {
		t.Fatalf("chunk into 2s = %v, want [[a b] [c d] [e]]", got)
	}
	if chunk(nil, 10) != nil {
		t.Errorf("chunk(nil) should be nil")
	}
}
