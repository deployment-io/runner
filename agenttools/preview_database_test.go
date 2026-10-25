package agenttools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/deployment-io/deployment-runner-kit/task_previews"
)

func detected(engine, version string, variables ...string) func() PreviewDatabaseDetection {
	return func() PreviewDatabaseDetection {
		return PreviewDatabaseDetection{Engine: engine, Version: version, Variables: variables, Evidence: []string{"prisma/schema.prisma"}}
	}
}

func TestResolvePreviewDatabase(t *testing.T) {
	notCalled := func() PreviewDatabaseDetection {
		t.Error("detection ran although the setting decides")
		return PreviewDatabaseDetection{}
	}
	use := &task_previews.PreviewDatabaseV1{Mode: task_previews.PreviewDatabaseModeUse, Engine: "mysql", Version: "8.0", Variables: []string{"DATABASE_URL"}}
	for _, tc := range []struct {
		name     string
		setting  *task_previews.PreviewDatabaseV1
		preview  map[string]string
		detect   func() PreviewDatabaseDetection
		want     *PreviewDatabasePlan
		wantNote string
	}{
		{name: "none setting", setting: &task_previews.PreviewDatabaseV1{Mode: task_previews.PreviewDatabaseModeNone}, detect: notCalled,
			wantNote: "the service's setting says no preview database"},
		{name: "use setting", setting: use, detect: notCalled, want: &PreviewDatabasePlan{
			Engine: "mysql", Version: "8.0", Image: "public.ecr.aws/docker/library/mysql:8.0", Port: 3306,
			Variables: []string{"DATABASE_URL"}, Source: "setting",
		}},
		{name: "use setting wins over Preview configuration", setting: use, preview: map[string]string{"DATABASE_URL": "postgres://team"}, detect: notCalled,
			want: &PreviewDatabasePlan{
				Engine: "mysql", Version: "8.0", Image: "public.ecr.aws/docker/library/mysql:8.0", Port: 3306,
				Variables: []string{"DATABASE_URL"}, Source: "setting",
			}},
		{name: "use setting with the default version", setting: &task_previews.PreviewDatabaseV1{Mode: "use", Engine: "mongodb", Variables: []string{"MONGO_URL"}},
			detect: notCalled, want: &PreviewDatabasePlan{
				Engine: "mongodb", Version: "7.0", Image: "public.ecr.aws/docker/library/mongo:7.0", Port: 27017,
				Variables: []string{"MONGO_URL"}, Source: "setting",
			}},
		{name: "use setting with an unsupported engine", setting: &task_previews.PreviewDatabaseV1{Mode: "use", Engine: "redis", Variables: []string{"X"}},
			detect: notCalled, wantNote: `the service's preview database setting names "redis", which preview databases don't support`},
		{name: "use setting with an unsupported version", setting: &task_previews.PreviewDatabaseV1{Mode: "use", Engine: "postgres", Version: "9", Variables: []string{"X"}},
			detect: notCalled, wantNote: "the service's preview database setting: postgres 9 is not a supported preview database version (supported: 13, 14, 15, 16, 17)"},
		{name: "automatic setting detects", setting: &task_previews.PreviewDatabaseV1{Mode: task_previews.PreviewDatabaseModeAutomatic},
			detect: detected("postgres", "15", "DATABASE_URL"), want: &PreviewDatabasePlan{
				Engine: "postgres", Version: "15", Image: "public.ecr.aws/docker/library/postgres:15", Port: 5432,
				Variables: []string{"DATABASE_URL"}, Evidence: []string{"prisma/schema.prisma"}, Source: "detected",
			}},
		{name: "nil setting detects", detect: detected("postgres", "16", "DATABASE_URL"), want: &PreviewDatabasePlan{
			Engine: "postgres", Version: "16", Image: "public.ecr.aws/docker/library/postgres:16", Port: 5432,
			Variables: []string{"DATABASE_URL"}, Evidence: []string{"prisma/schema.prisma"}, Source: "detected",
		}},
		{name: "Preview configuration overrides detection", preview: map[string]string{"DATABASE_URL": "postgres://team", "OTHER": "x"},
			detect: detected("postgres", "16", "DATABASE_URL", "DIRECT_URL"), wantNote: "Preview configuration sets DATABASE_URL, so the preview uses that"},
		{name: "detection found nothing", detect: func() PreviewDatabaseDetection {
			return PreviewDatabaseDetection{Reason: "found signs of both mysql and postgres"}
		},
			wantNote: "found signs of both mysql and postgres"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := task_previews.WebServicePreviewSettingsV1{PreviewDatabase: tc.setting}
			got, note := resolvePreviewDatabase(service, tc.preview, tc.detect)
			if !reflect.DeepEqual(got, tc.want) || note != tc.wantNote {
				t.Errorf("got %+v, %q\nwant %+v, %q", got, note, tc.want, tc.wantNote)
			}
		})
	}
}

// databaseDeps is webDeps over a repository whose service uses Prisma with Postgres,
// with a database store.
func databaseDeps(t *testing.T, service task_previews.WebServicePreviewSettingsV1, preview map[string]string, logs *bytes.Buffer,
	deploy func(context.Context, WebServicePreviewDeployInput) (WebServicePreviewDeployOutput, error)) (DeployWebServicePreviewDeps, *fakeWebStore) {
	t.Helper()
	deps := webDeps(t, &fakeWebStore{}, &fakeWebCloudFront{}, deploy)
	writeTree(t, filepath.Join(deps.WorkDirHost, "1-acme", "api"), map[string]string{
		"services/api/prisma/schema.prisma": prismaPostgres,
		"services/api/src/db.ts":            "process.env.DATABASE_URL",
	})
	databaseStore := &fakeWebStore{}
	deps.DatabaseStore = databaseStore
	deps.LogsWriter = logs
	deps.Settings = func(string) (task_previews.WebServicePreviewSettingsReplyV1, error) {
		reply := readyReply(service)
		reply.Variables = preview
		return reply, nil
	}
	return deps, databaseStore
}

func runDatabasePreview(t *testing.T, deps DeployWebServicePreviewDeps) deployWebServicePreviewResult {
	t.Helper()
	builds := newPreviewBuildsPending("deploying", webPreviewDeployingMessage)
	defer builds.stop()
	out, err := handleDeployWebServicePreview(context.Background(), deps, builds, json.RawMessage(`{"repository":"1-acme/api"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"preview:preview", "postgresql://", "mysql://", "mongodb://", "password"} {
		if strings.Contains(out, secret) {
			t.Errorf("the result carries %q: %s", secret, out)
		}
	}
	var res deployWebServicePreviewResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	return res
}

func TestDeployWebServicePreviewWithDatabase(t *testing.T) {
	service := webService("api", "us-east-1", "services/api")
	service.PreDeployCommand = "npx prisma migrate deploy"
	service.VariableNames = []string{"DATABASE_URL"}
	var logs bytes.Buffer
	var got WebServicePreviewDeployInput
	deps, databaseStore := databaseDeps(t, service, map[string]string{"API_KEY": "k"}, &logs,
		func(_ context.Context, in WebServicePreviewDeployInput) (WebServicePreviewDeployOutput, error) {
			got = in
			return WebServicePreviewDeployOutput{LoadBalancerDNS: "alb-x.elb.amazonaws.com"}, nil
		})
	res := runDatabasePreview(t, deps)
	want := &previewDatabaseResult{
		Engine: "postgres", Version: "16", Image: "public.ecr.aws/docker/library/postgres:16",
		Variables: []string{"DATABASE_URL", "DIRECT_URL"}, Evidence: []string{"prisma/schema.prisma"},
		Source: "detected", Migrations: "ran `npx prisma migrate deploy`",
	}
	if !reflect.DeepEqual(res.Database, want) || res.DatabaseNote != "" {
		t.Errorf("database = %+v, note %q\nwant %+v", res.Database, res.DatabaseNote, want)
	}
	if got.Database == nil || got.Database.PreviewID != "64f0c0ffee0123456789abcd" || got.Database.ServiceKey != got.ServiceKey+"-db" ||
		got.Database.Engine != "postgres" || got.Database.Port != 5432 {
		t.Errorf("deploy input database = %+v", got.Database)
	}
	if len(databaseStore.ensured) != 1 || databaseStore.ensured[0] != got.ServiceKey+"-db" {
		t.Errorf("database store ensured %v", databaseStore.ensured)
	}
	for _, line := range []string{
		"Preview database: postgres 16 (public.ecr.aws/docker/library/postgres:16), detected from the Task's code\n",
		"Preview database variables: DATABASE_URL, DIRECT_URL\n",
		"Preview database evidence: prisma/schema.prisma\n",
		"Preview database: DIRECT_URL is read in the code but not set in production; the preview sets it\n",
		"Preview database migrations: ran `npx prisma migrate deploy`\n",
	} {
		if !strings.Contains(logs.String(), line) {
			t.Errorf("Step log lacks %q:\n%s", line, logs.String())
		}
	}
	if strings.Contains(res.LogTail, "Preview database") {
		t.Errorf("the database lines reached the agent's log tail: %q", res.LogTail)
	}
}

func TestDeployWebServicePreviewWithDatabaseNoPreDeployCommand(t *testing.T) {
	var logs bytes.Buffer
	deps, _ := databaseDeps(t, webService("api", "us-east-1", "services/api"), nil, &logs,
		func(context.Context, WebServicePreviewDeployInput) (WebServicePreviewDeployOutput, error) {
			return WebServicePreviewDeployOutput{LoadBalancerDNS: "alb-x.elb.amazonaws.com"}, nil
		})
	res := runDatabasePreview(t, deps)
	if res.Database == nil || res.Database.Migrations != "no pre-deploy command" {
		t.Errorf("database = %+v", res.Database)
	}
	if !strings.Contains(logs.String(), "Preview database migrations: no pre-deploy command\n") {
		t.Errorf("Step log:\n%s", logs.String())
	}
}

func TestDeployWebServicePreviewWithoutDatabase(t *testing.T) {
	for _, tc := range []struct {
		name    string
		service func(*task_previews.WebServicePreviewSettingsV1)
		preview map[string]string
		note    string
	}{
		{name: "Preview configuration sets the variable", preview: map[string]string{"DATABASE_URL": "postgres://team-db"},
			note: "Preview configuration sets DATABASE_URL, so the preview uses that"},
		{name: "none setting", service: func(s *task_previews.WebServicePreviewSettingsV1) {
			s.PreviewDatabase = &task_previews.PreviewDatabaseV1{Mode: "none"}
		}, note: "the service's setting says no preview database"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := webService("api", "us-east-1", "services/api")
			service.PreDeployCommand = "./migrate"
			if tc.service != nil {
				tc.service(&service)
			}
			var logs bytes.Buffer
			var got WebServicePreviewDeployInput
			deps, databaseStore := databaseDeps(t, service, tc.preview, &logs,
				func(_ context.Context, in WebServicePreviewDeployInput) (WebServicePreviewDeployOutput, error) {
					got = in
					return WebServicePreviewDeployOutput{LoadBalancerDNS: "alb-x.elb.amazonaws.com"}, nil
				})
			res := runDatabasePreview(t, deps)
			if res.Database != nil || res.DatabaseNote != tc.note {
				t.Errorf("database %+v, note %q; want note %q", res.Database, res.DatabaseNote, tc.note)
			}
			if got.Database != nil || len(databaseStore.ensured) != 0 {
				t.Errorf("a database was deployed: %+v, ensured %v", got.Database, databaseStore.ensured)
			}
			if !strings.Contains(logs.String(), "Preview database: none — "+tc.note+"\n") {
				t.Errorf("Step log:\n%s", logs.String())
			}
			if strings.Contains(logs.String(), "postgres://team-db") {
				t.Error("a Preview configuration value was logged")
			}
		})
	}
	// A repository with no database at all.
	var got WebServicePreviewDeployInput
	deps := webDeps(t, &fakeWebStore{}, &fakeWebCloudFront{}, func(_ context.Context, in WebServicePreviewDeployInput) (WebServicePreviewDeployOutput, error) {
		got = in
		return WebServicePreviewDeployOutput{LoadBalancerDNS: "alb-x.elb.amazonaws.com"}, nil
	})
	deps.DatabaseStore = &fakeWebStore{}
	res := runDatabasePreview(t, deps)
	if res.Database != nil || res.DatabaseNote != "no database detected" || got.Database != nil {
		t.Errorf("database %+v, note %q, input %+v", res.Database, res.DatabaseNote, got.Database)
	}
}

func TestPreviewDatabaseMigrations(t *testing.T) {
	variables := []string{"DB_URL"}
	for _, tc := range []struct {
		command, wantRun, wantSummary string
	}{
		{"", "", "no pre-deploy command"},
		{"npx prisma migrate deploy", "npx prisma migrate deploy", "ran `npx prisma migrate deploy`"},
		{"RAILS_ENV=production SECRET_KEY_BASE=abc123 bin/rails db:migrate", "RAILS_ENV=production SECRET_KEY_BASE=abc123 bin/rails db:migrate",
			"ran `RAILS_ENV=*** SECRET_KEY_BASE=*** bin/rails db:migrate`"},
		{"curl -s https://user:t0ken@hooks.example.com && ./migrate", "curl -s https://user:t0ken@hooks.example.com && ./migrate",
			"ran `curl -s https://***@hooks.example.com && ./migrate`"},
		{"DATABASE_URL=postgres://prod:pw@prod-db/app ./migrate", "",
			"didn't run the pre-deploy command: it sets DATABASE_URL itself, which could point it at a database the preview doesn't own"},
		{"export DB_URL=$PROD_DB; ./migrate", "",
			"didn't run the pre-deploy command: it sets DB_URL itself, which could point it at a database the preview doesn't own"},
		{"env PGHOST=prod ./migrate", "",
			"didn't run the pre-deploy command: it sets PGHOST itself, which could point it at a database the preview doesn't own"},
		{"./migrate --url mysql://root:pw@prod-db/app", "",
			"didn't run the pre-deploy command: it names a database URL itself, which could point it at a database the preview doesn't own"},
		{"psql -h shared-db -U app -d shared -f migrations.sql", "",
			"didn't run the pre-deploy command: it names a database host itself (psql -h), which could point it at a database the preview doesn't own"},
		{"./migrate --host shared-db.internal", "",
			"didn't run the pre-deploy command: it names a database address itself (--host), which could point it at a database the preview doesn't own"},
		{`psql "host=shared-db dbname=app" -f schema.sql`, "",
			"didn't run the pre-deploy command: it names a database host itself, which could point it at a database the preview doesn't own"},
		// The preview's own variable is the preview's address.
		{`npx prisma db execute --url "$DB_URL" --file x.sql`, `npx prisma db execute --url "$DB_URL" --file x.sql`,
			"ran `npx prisma db execute --url \"$DB_URL\" --file x.sql`"},
		{"./migrate --password hunter2 && curl -H 'Authorization: Bearer abc' https://x/?token=zzz", "./migrate --password hunter2 && curl -H 'Authorization: Bearer abc' https://x/?token=zzz",
			"ran `./migrate --password *** && curl -H 'Authorization: ***' https://x/?token=***`"},
		{"mysql -upreview -pS3cret < schema.sql && ./notify ghp_a1B2c3D4e5F6g7H8i9J0k1L2m3", "mysql -upreview -pS3cret < schema.sql && ./notify ghp_a1B2c3D4e5F6g7H8i9J0k1L2m3",
			"ran `mysql -upreview -p*** < schema.sql && ./notify ***`"},
		{"bin/rails db:migrate", "bin/rails db:migrate", "ran `bin/rails db:migrate`"},
		{"python manage.py migrate --noinput", "python manage.py migrate --noinput", "ran `python manage.py migrate --noinput`"},
	} {
		run, plan := previewDatabaseMigrations(tc.command, variables)
		if run != tc.wantRun || plan.summary() != tc.wantSummary {
			t.Errorf("%q: run %q, summary %q; want %q, %q", tc.command, run, plan.summary(), tc.wantRun, tc.wantSummary)
		}
	}
}

func TestDeployWebServicePreviewWithDatabaseSkipsOwnDatabaseCommand(t *testing.T) {
	service := webService("api", "us-east-1", "services/api")
	service.PreDeployCommand = "DATABASE_URL=postgres://prod:pw@prod-db/app npx prisma migrate deploy"
	var logs bytes.Buffer
	var got WebServicePreviewDeployInput
	deps, _ := databaseDeps(t, service, nil, &logs,
		func(_ context.Context, in WebServicePreviewDeployInput) (WebServicePreviewDeployOutput, error) {
			got = in
			return WebServicePreviewDeployOutput{LoadBalancerDNS: "alb-x.elb.amazonaws.com"}, nil
		})
	res := runDatabasePreview(t, deps)
	if got.Database == nil || got.Database.PreDeployCommand != "" {
		t.Fatalf("deploy input database = %+v; want no pre-deploy command", got.Database)
	}
	if res.Database == nil || !strings.HasPrefix(res.Database.Migrations, "didn't run the pre-deploy command: it sets DATABASE_URL itself") {
		t.Errorf("database = %+v", res.Database)
	}
	if strings.Contains(logs.String(), "prod:pw") {
		t.Errorf("the command's credentials reached the Step log:\n%s", logs.String())
	}
}

// A failed deploy's error carries the database and the log tail (the pre-deploy
// command's output), never a password or a URL.
func TestDeployWebServicePreviewWithDatabaseFailure(t *testing.T) {
	service := webService("api", "us-east-1", "services/api")
	service.PreDeployCommand = "npx prisma migrate deploy"
	var logs bytes.Buffer
	deps, _ := databaseDeps(t, service, nil, &logs,
		func(_ context.Context, in WebServicePreviewDeployInput) (WebServicePreviewDeployOutput, error) {
			io.WriteString(in.Logs, "Error: P3009 migrate found failed migrations\n")
			return WebServicePreviewDeployOutput{}, WebPreviewDeployFailed(errors.New("The pre-deploy command failed (exit code 1). Nothing was deployed."))
		})
	builds := newPreviewBuildsPending("deploying", webPreviewDeployingMessage)
	defer builds.stop()
	_, err := handleDeployWebServicePreview(context.Background(), deps, builds, json.RawMessage(`{"repository":"1-acme/api"}`))
	if err == nil {
		t.Fatal("the deploy didn't fail")
	}
	msg := err.Error()
	for _, want := range []string{"exit code 1", `"engine":"postgres"`, `"variables":["DATABASE_URL","DIRECT_URL"]`,
		"npx prisma migrate deploy", "P3009 migrate found failed migrations"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error lacks %q: %s", want, msg)
		}
	}
	for _, secret := range []string{"preview:preview", "postgresql://"} {
		if strings.Contains(msg, secret) {
			t.Errorf("error carries %q: %s", secret, msg)
		}
	}
	if !strings.Contains(logs.String(), "Preview database migrations: the deploy failed before it finished") {
		t.Errorf("Step log:\n%s", logs.String())
	}
}

func TestPreviewDatabaseEvidenceIsCapped(t *testing.T) {
	var evidence []string
	for i := 0; i < 5000; i++ {
		evidence = append(evidence, fmt.Sprintf("src/very/long/path/to/module%04d.ts", i))
	}
	plan := PreviewDatabasePlan{Engine: "postgres", Version: "16", Image: "i", Variables: []string{"DATABASE_URL"}, Evidence: evidence}
	var logs bytes.Buffer
	logPreviewDatabasePlan(&logs, plan)
	for _, line := range strings.Split(logs.String(), "\n") {
		if len(line) > 4096 {
			t.Errorf("a %d-byte Step log line", len(line))
		}
	}
	if !strings.Contains(logs.String(), "(and 4980 more files)") {
		t.Errorf("Step log:\n%s", logs.String())
	}
	// Twenty paths of several KiB each still make short lines.
	evidence = nil
	for i := 0; i < 30; i++ {
		evidence = append(evidence, strings.Repeat(fmt.Sprintf("dir%02d/", i), 600)+"db.ts")
	}
	logs.Reset()
	logPreviewDatabasePlan(&logs, PreviewDatabasePlan{Engine: "postgres", Variables: []string{strings.Repeat("A", 70000) + "_DATABASE_URL"}, Evidence: evidence})
	logPreviewDatabaseNote(&logs, strings.Repeat("x", 70000))
	logPreviewDatabaseMigrations(&logs, strings.Repeat("y", 70000))
	for _, line := range strings.Split(logs.String(), "\n") {
		if len(line) > previewDatabaseLogLineMax+len("…") {
			t.Errorf("a %d-byte Step log line", len(line))
		}
	}
	if !strings.Contains(logs.String(), "/db.ts") {
		t.Errorf("shortened paths lose their file name:\n%.300s", logs.String())
	}
	res := newPreviewDatabaseResult(plan, "no pre-deploy command")
	if len(res.Evidence) != previewDatabaseEvidenceMax+1 || res.Evidence[previewDatabaseEvidenceMax] != "(and 4980 more files)" {
		t.Errorf("result evidence = %d entries, last %q", len(res.Evidence), res.Evidence[len(res.Evidence)-1])
	}
}
