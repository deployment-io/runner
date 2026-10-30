package commands

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ecr"
)

// The local image may only be dropped once it is durable in ECR. A failed
// push must leave it exactly where it is — a rollback re-pulls from ECR, and
// there is nothing to re-pull if the push never landed.
func TestPushAndReclaim(t *testing.T) {
	const (
		localTag = "6a8fc4300aaee89eaf15b035-6a8fe8be1bcfd79ebf25c046:abc1234"
		ecrTag   = "1234.dkr.ecr.us-east-1.amazonaws.com/ecr-org-deployment:abc1234"
	)
	pushFailed := errors.New("no space left on device")

	tests := []struct {
		name       string
		pushErr    error
		wantErr    error
		wantRemove []string
	}{
		{
			name:       "a pushed image gives up both commit-scoped local tags",
			wantRemove: []string{ecrTag, localTag},
		},
		{
			name:    "a failed push keeps the image",
			pushErr: pushFailed,
			wantErr: pushFailed,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var removed []string
			pushed := 0
			command := &UploadDockerImageToEcr{
				pushImage: func(_ *ecr.Client, ref string, _ io.Writer) error {
					pushed++
					if ref != ecrTag {
						t.Errorf("pushed %q, want the ECR ref", ref)
					}
					return test.pushErr
				},
				reclaimLocalImages: func(refs []string, _ io.Writer) {
					removed = append(removed, refs...)
				},
			}

			err := command.pushAndReclaim(nil, localTag, ecrTag, &bytes.Buffer{})

			if !errors.Is(err, test.wantErr) {
				t.Fatalf("err = %v, want %v", err, test.wantErr)
			}
			if pushed != 1 {
				t.Errorf("push called %d times, want exactly one", pushed)
			}
			if len(removed) == 0 && len(test.wantRemove) == 0 {
				return
			}
			if !reflect.DeepEqual(removed, test.wantRemove) {
				t.Errorf("removed %v, want %v", removed, test.wantRemove)
			}
		})
	}
}

// The command the job runner hands out must have its seams wired to the
// production implementations — a nil field would panic mid-deploy.
func TestNewUploadDockerImageToEcrIsFullyWired(t *testing.T) {
	command := newUploadDockerImageToEcr()
	if command.pushImage == nil || command.reclaimLocalImages == nil {
		t.Fatal("both seams must default to the production implementations")
	}
}

func TestEcrRegistryPassword(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("AWS:pass:word"))
	if got, err := ecrRegistryPassword(encoded); err != nil || got != "pass:word" {
		t.Errorf("got %q, %v; want the password", got, err)
	}
}

// A malformed token's error carries no part of the token: it is a live credential.
func TestEcrRegistryPasswordMalformedTokenNotInError(t *testing.T) {
	decoded := "liveRegistrySecretNoColon"
	encoded := base64.StdEncoding.EncodeToString([]byte(decoded))
	_, err := ecrRegistryPassword(encoded)
	if err == nil {
		t.Fatal("no error for a token without a colon")
	}
	msg := err.Error()
	if !strings.Contains(msg, "unexpected format") {
		t.Errorf("error %q, want it to say unexpected format", msg)
	}
	for _, secret := range []string{decoded, "liveRegistrySecret", encoded} {
		if strings.Contains(msg, secret) {
			t.Errorf("error %q contains the token %q", msg, secret)
		}
	}
}
