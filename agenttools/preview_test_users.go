package agenttools

// preview_test_users.go holds the test users of a Task's backend previews: one per
// role a person set on the service's Previews tab, with an email and a password the
// runner generates. The preview seed command creates them (it gets them in its
// task's environment); a later sign-in step reads them. They stay runner-side:
// never in a log, a tool result, the agent's environment or the pull request.

import (
	"crypto/rand"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/deployment-io/deployment-runner-kit/task_previews"
)

// previewTestUserPasswordLength is the length of a generated password, drawn from
// previewTestUserPasswordAlphabet.
const previewTestUserPasswordLength = 24

const previewTestUserPasswordAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// PreviewTestUser is the test user of one role of a preview.
type PreviewTestUser struct {
	Role     string
	Email    string
	Password string
}

// PreviewTestUsers are the test users of the previews deployed by the deploys that
// share it, by preview id: each role of a preview gets its password the first time
// it's asked for, and keeps it for every later deploy of that preview. A new
// holder (a later job) generates new passwords; the emails stay the same.
type PreviewTestUsers struct {
	mu        sync.Mutex
	byPreview map[string]map[string]PreviewTestUser
}

// NewPreviewTestUsers is an empty holder.
func NewPreviewTestUsers() *PreviewTestUsers {
	return &PreviewTestUsers{byPreview: map[string]map[string]PreviewTestUser{}}
}

// For returns the test user of each of roles of the preview previewID, in roles'
// order, generating those it has none of yet.
func (u *PreviewTestUsers) For(previewID string, roles []task_previews.PreviewRoleV1) ([]PreviewTestUser, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	users := u.byPreview[previewID]
	if users == nil {
		users = map[string]PreviewTestUser{}
		u.byPreview[previewID] = users
	}
	out := make([]PreviewTestUser, 0, len(roles))
	for _, role := range roles {
		user, ok := users[role.Name]
		if !ok {
			password, err := previewTestUserPassword()
			if err != nil {
				return nil, fmt.Errorf("generate a password for the preview's test users: %w", err)
			}
			user = PreviewTestUser{Role: role.Name, Email: previewTestUserEmail(previewID, role.Name), Password: password}
			users[role.Name] = user
		}
		out = append(out, user)
	}
	return out, nil
}

// Users returns the test users the holder has for previewID, sorted by role; nil
// when it has none.
func (u *PreviewTestUsers) Users(previewID string) []PreviewTestUser {
	u.mu.Lock()
	defer u.mu.Unlock()
	users := u.byPreview[previewID]
	if len(users) == 0 {
		return nil
	}
	out := make([]PreviewTestUser, 0, len(users))
	for _, role := range sortedKeys(users) {
		out = append(out, users[role])
	}
	return out
}

// previewTestUserEmail is "qa-<role>-<the preview id's last 8 characters>@example.com".
func previewTestUserEmail(previewID, role string) string {
	suffix := previewID
	if len(suffix) > 8 {
		suffix = suffix[len(suffix)-8:]
	}
	return fmt.Sprintf("qa-%s-%s@example.com", strings.ToLower(role), suffix)
}

// previewTestUserPassword is previewTestUserPasswordLength characters of
// previewTestUserPasswordAlphabet from crypto/rand, each equally likely.
func previewTestUserPassword() (string, error) {
	// The largest multiple of the alphabet's length a byte can hold: bytes from it
	// up are dropped so no character is likelier than another.
	limit := byte(256 - 256%len(previewTestUserPasswordAlphabet))
	password := make([]byte, 0, previewTestUserPasswordLength)
	buf := make([]byte, 2*previewTestUserPasswordLength)
	for len(password) < previewTestUserPasswordLength {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, b := range buf {
			if b < limit && len(password) < previewTestUserPasswordLength {
				password = append(password, previewTestUserPasswordAlphabet[int(b)%len(previewTestUserPasswordAlphabet)])
			}
		}
	}
	return string(password), nil
}

// PreviewRoleEnvKey is the <KEY> of a role's QA_ROLE_<KEY>_EMAIL and
// QA_ROLE_<KEY>_PASSWORD: its name in upper case, every character other than A–Z
// and 0–9 turned into "_".
func PreviewRoleEnvKey(role string) string {
	key := []byte(strings.ToUpper(role))
	for i, c := range key {
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			key[i] = '_'
		}
	}
	return string(key)
}

// WebServicePreviewSeed is the preview seed command a backend preview's deploy runs
// right after its preview migrate command, and the test users it creates.
type WebServicePreviewSeed struct {
	Command string
	// Users are the test users of the service's roles, in their saved order.
	Users []PreviewTestUser
}

// PreviewSeedOutcome is how a deploy's preview seed command ended.
type PreviewSeedOutcome int

const (
	// PreviewSeedNotRun: the deploy had no seed command to run.
	PreviewSeedNotRun PreviewSeedOutcome = iota
	PreviewSeedSucceeded
	// PreviewSeedFailed: it exited non-zero, couldn't start or timed out; the
	// deploy went on.
	PreviewSeedFailed
)

// planPreviewSeed is the seed the API preview previewID's deploy runs: nil when
// service has no preview seed command. Its test users come from deps.TestUsers, a
// holder of this deploy's own when there's none.
func planPreviewSeed(deps DeployWebServicePreviewDeps, previewID string, service task_previews.WebServicePreviewSettingsV1) (*WebServicePreviewSeed, error) {
	if strings.TrimSpace(service.PreviewSeedCommand) == "" {
		return nil, nil
	}
	holder := deps.TestUsers
	if holder == nil {
		holder = NewPreviewTestUsers()
	}
	users, err := holder.For(previewID, service.PreviewRoles)
	if err != nil {
		return nil, err
	}
	return &WebServicePreviewSeed{Command: service.PreviewSeedCommand, Users: users}, nil
}

// previewTestUsersSummary is the "Preview test users" line and the tool result's
// test_users after a deploy that succeeded: the roles and how the seed ended, never
// an email or a password. "" when the service sets neither a seed command nor roles.
func previewTestUsersSummary(service task_previews.WebServicePreviewSettingsV1, outcome PreviewSeedOutcome) string {
	hasCommand := strings.TrimSpace(service.PreviewSeedCommand) != ""
	switch {
	case !hasCommand && len(service.PreviewRoles) == 0:
		return ""
	case !hasCommand:
		return "roles are set but there's no preview seed command, so there are no test users"
	case outcome != PreviewSeedSucceeded:
		return "the preview seed command failed, so there are no test users"
	case len(service.PreviewRoles) == 0:
		return "ran the preview seed command (no roles)"
	}
	names := make([]string, 0, len(service.PreviewRoles))
	for _, role := range service.PreviewRoles {
		names = append(names, role.Name)
	}
	return "seeded " + strings.Join(names, ", ")
}

// logPreviewTestUsers writes the summary line, when there is one.
func logPreviewTestUsers(w io.Writer, summary string) {
	if w != nil && summary != "" {
		writePreviewDatabaseLogLine(w, "Preview test users: "+summary)
	}
}
