package agenttools

// preview_database.go decides whether a backend preview gets its own throwaway
// database, and which: the person's setting for the service, else detection from
// the Task's code (preview_database_detect.go), unless Preview configuration already
// points the detected variables at a database of the team's own. The database itself
// is deployed by the commands package (Deploy), beside the API preview.

import (
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/deployment-io/deployment-runner-kit/task_previews"
)

// Where a preview database's engine and variables came from.
const (
	PreviewDatabaseSourceDetected = "detected"
	PreviewDatabaseSourceSetting  = "setting"
)

// previewDatabaseSettingNone is the note for a "none" setting.
const previewDatabaseSettingNone = "the service's setting says no preview database"

// PreviewDatabasePlan is the database a backend preview gets.
type PreviewDatabasePlan struct {
	Engine  string
	Version string
	Image   string
	Port    int64
	// Variables are the names of the API's variables pointed at the database.
	Variables []string
	// Evidence are the files detection found it in (none for a setting).
	Evidence []string
	Source   string
	// NotInProduction are Variables production doesn't set (the Task may add them).
	NotInProduction []string
}

// PreviewDatabaseDeploy is the database a deploy runs beside the API preview.
type PreviewDatabaseDeploy struct {
	// PreviewID is the database preview Deployment's id.
	PreviewID string
	// ServiceKey is its service name in the Task: "<API service key>-db".
	ServiceKey string
	PreviewDatabasePlan
	// PreDeployCommand is the production service's pre-deploy command when it
	// runs against this database (previewDatabaseMigrations); "" when it doesn't.
	PreDeployCommand string
	// Migrations says whether it runs, for the result and the Step log.
	Migrations previewDatabaseMigrationPlan
}

// previewDatabaseServiceKey is the database preview's service name in the Task.
func previewDatabaseServiceKey(apiServiceKey string) string {
	return apiServiceKey + "-db"
}

// resolvePreviewDatabase decides the preview database of service: nil and a note
// saying why when there is none. detect runs detection over the Task's code; it is
// called only when the setting leaves the choice to it. previewVariables are
// Preview configuration's variables (only their names are read).
func resolvePreviewDatabase(service task_previews.WebServicePreviewSettingsV1, previewVariables map[string]string,
	detect func() PreviewDatabaseDetection) (*PreviewDatabasePlan, string) {
	if setting := service.PreviewDatabase; setting != nil {
		switch setting.Mode {
		case task_previews.PreviewDatabaseModeNone:
			return nil, previewDatabaseSettingNone
		case task_previews.PreviewDatabaseModeUse:
			return previewDatabaseFromSetting(*setting)
		}
	}
	d := detect()
	if d.Engine == "" {
		reason := d.Reason
		if reason == "" {
			reason = previewDatabaseNoneDetected
		}
		return nil, reason
	}
	var set []string
	for _, name := range d.Variables {
		if _, ok := previewVariables[name]; ok {
			set = append(set, name)
		}
	}
	if len(set) > 0 {
		return nil, fmt.Sprintf("Preview configuration sets %s, so the preview uses that", strings.Join(set, ", "))
	}
	engine, _ := task_previews.LookupPreviewDatabaseEngine(d.Engine)
	image, err := engine.Image(d.Version)
	if err != nil {
		return nil, err.Error()
	}
	return &PreviewDatabasePlan{
		Engine:          d.Engine,
		Version:         versionOrDefault(engine, d.Version),
		Image:           image,
		Port:            engine.Port,
		Variables:       d.Variables,
		Evidence:        d.Evidence,
		Source:          PreviewDatabaseSourceDetected,
		NotInProduction: d.NotInProduction,
	}, ""
}

// previewDatabaseFromSetting is the database a "use" setting names. app-server
// validates the setting; one that still isn't usable gives no database.
func previewDatabaseFromSetting(setting task_previews.PreviewDatabaseV1) (*PreviewDatabasePlan, string) {
	engine, ok := task_previews.LookupPreviewDatabaseEngine(setting.Engine)
	if !ok {
		return nil, fmt.Sprintf("the service's preview database setting names %q, which preview databases don't support", setting.Engine)
	}
	image, err := engine.Image(setting.Version)
	if err != nil {
		return nil, "the service's preview database setting: " + err.Error()
	}
	var variables []string
	seen := map[string]bool{}
	for _, name := range setting.Variables {
		name = strings.TrimSpace(name)
		if name != "" && !seen[name] {
			seen[name] = true
			variables = append(variables, name)
		}
	}
	if len(variables) == 0 {
		return nil, "the service's preview database setting names no variable"
	}
	return &PreviewDatabasePlan{
		Engine:    engine.Engine,
		Version:   versionOrDefault(engine, setting.Version),
		Image:     image,
		Port:      engine.Port,
		Variables: variables,
		Source:    PreviewDatabaseSourceSetting,
	}, ""
}

func versionOrDefault(e task_previews.PreviewDatabaseEngineV1, version string) string {
	if version == "" {
		return e.DefaultVersion
	}
	return version
}

// detectPreviewDatabaseIn is detection over fsys, the service directory.
func detectPreviewDatabaseIn(fsys fs.FS, productionVariableNames []string) func() PreviewDatabaseDetection {
	return func() PreviewDatabaseDetection {
		return DetectPreviewDatabase(fsys, productionVariableNames)
	}
}

// previewDatabaseResult is the result's `database`. Never a password or a URL.
type previewDatabaseResult struct {
	Engine     string   `json:"engine"`
	Version    string   `json:"version"`
	Image      string   `json:"image"`
	Variables  []string `json:"variables"`
	Evidence   []string `json:"evidence"`
	Source     string   `json:"source"`
	Migrations string   `json:"migrations"`
}

// previewDatabaseMigrationPlan is whether the pre-deploy command runs against the
// preview database, as the result and the Step log say it.
type previewDatabaseMigrationPlan struct {
	// command is the command shown, with inline values redacted; "" when none runs.
	command string
	// skipped says why the command isn't run, or that there is none.
	skipped string
}

// summary is the result's `migrations` once the deploy succeeded.
func (m previewDatabaseMigrationPlan) summary() string {
	if m.command == "" {
		return m.skipped
	}
	return fmt.Sprintf("ran `%s`", m.command)
}

// failedSummary is the result's `migrations` when the deploy failed: whether the
// command ran isn't known here, its output is in the log tail.
func (m previewDatabaseMigrationPlan) failedSummary() string {
	if m.command == "" {
		return m.skipped
	}
	return fmt.Sprintf("the deploy failed before it finished; the pre-deploy command was `%s` (its output, if it ran, is in the log tail)", m.command)
}

// previewDatabaseMigrations decides whether preDeployCommand runs against the
// preview database: never when the command sets a database variable or names a
// database URL itself, since it would then reach a database the preview doesn't
// own. It returns the command to run ("" for none).
func previewDatabaseMigrations(preDeployCommand string, variables []string) (string, previewDatabaseMigrationPlan) {
	command := strings.TrimSpace(preDeployCommand)
	if command == "" {
		return "", previewDatabaseMigrationPlan{skipped: "no pre-deploy command"}
	}
	if reason := preDeployCommandOwnDatabase(command, variables); reason != "" {
		return "", previewDatabaseMigrationPlan{skipped: "didn't run the pre-deploy command: " + reason +
			", which could point it at a database the preview doesn't own"}
	}
	return preDeployCommand, previewDatabaseMigrationPlan{command: redactPreDeployCommand(command)}
}

var (
	// shellAssignment is NAME= at the start of a command word (VAR=… cmd,
	// export VAR=…, env VAR=…).
	shellAssignment = regexp.MustCompile(`(^|[\s;&|(])([A-Za-z_][A-Za-z0-9_]*)=`)
	// databaseURLLiteral is a database address written into the command.
	databaseURLLiteral = regexp.MustCompile(`(?i)\b(postgres|postgresql|mysql|mysql2|mongodb|mongodb\+srv)://`)
	// databaseAddressFlag is a flag naming a database's address, and its value.
	databaseAddressFlag = regexp.MustCompile(`(?i)(?:^|\s)(--host|--hostname|--server|--uri|--url|--db-url|--database-url|--dsn|--connection|--connection-string|--conn-string)(?:=|\s+)("[^"]*"|'[^']*'|\S+)`)
	// databaseClientHost is a database client's -h <host>.
	databaseClientHost = regexp.MustCompile(`(?i)(?:^|[\s;&|(/])(psql|mysql|mariadb|mongosh|mongo|pg_restore|pg_dump|mysqldump|mongorestore|mongoimport|sqlcmd)\b[^;&|]*?\s-h\s*("[^"]*"|'[^']*'|\S+)`)
	// conninfoAddress is a libpq/ADO-style connection string's host or server.
	conninfoAddress = regexp.MustCompile(`(?i)(?:^|[\s"';])(host|hostaddr|server|data source)\s*=\s*[^\s"';]`)
	// ownVariableReference is "$NAME" or "${NAME}", alone.
	ownVariableReference = regexp.MustCompile(`^\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?$`)

	// assignmentValue is an inline NAME=value's value; urlCredentials a URL's user:password@.
	assignmentValue = regexp.MustCompile(`(^|[\s;&|(])([A-Za-z_][A-Za-z0-9_]*)=("[^"]*"|'[^']*'|\S*)`)
	urlCredentials  = regexp.MustCompile(`://[^/\s@]+@`)
	// secretFlag is a flag whose name says it holds a secret, and its value.
	secretFlag = regexp.MustCompile(`(?i)((?:^|\s)--?[A-Za-z0-9_-]*(?:password|passwd|pwd|pass|secret|token|key|auth|credential)[A-Za-z0-9_-]*)(=|\s+)("[^"]*"|'[^']*'|\S+)`)
	// mysqlPassword is mysql's -p<password>.
	mysqlPassword = regexp.MustCompile(`(\s-p)[^\s"']+`)
	// secretHeader is a header holding a credential (Authorization, X-Api-Key…).
	secretHeader = regexp.MustCompile(`(?i)\b((?:proxy-)?authorization|[A-Za-z-]*(?:token|key|secret|auth)[A-Za-z-]*)(\s*:\s*)[^"'\n]+`)
	// secretParameter is key=value whose key says it holds a secret (a query
	// parameter, a conninfo field).
	secretParameter = regexp.MustCompile(`(?i)([A-Za-z0-9_.-]*(?:password|passwd|pwd|secret|token|apikey|api_key|access_key)[A-Za-z0-9_.-]*=)[^&\s"';]+`)
	// keyLikeWord is a long word of letters and digits with no path or dots:
	// what an API key or token looks like.
	keyLikeWord = regexp.MustCompile(`\b[A-Za-z0-9_+=-]{24,}\b`)
	hasLetter   = regexp.MustCompile(`[A-Za-z]`)
	hasDigit    = regexp.MustCompile(`[0-9]`)
)

// preDeployCommandOwnDatabase says how command picks a database of its own: it
// sets one of variables or a database-looking variable, or names a database URL.
// "" when it doesn't.
func preDeployCommandOwnDatabase(command string, variables []string) string {
	ours := map[string]bool{}
	for _, name := range variables {
		ours[name] = true
	}
	var set []string
	seen := map[string]bool{}
	for _, m := range shellAssignment.FindAllStringSubmatch(command, -1) {
		name := m[2]
		kind, _ := task_previews.ClassifyDatabaseVariable(name)
		if (ours[name] || kind != task_previews.NotDatabaseVariable) && !seen[name] {
			seen[name] = true
			set = append(set, name)
		}
	}
	if len(set) > 0 {
		return "it sets " + strings.Join(set, ", ") + " itself"
	}
	if databaseURLLiteral.MatchString(command) {
		return "it names a database URL itself"
	}
	for _, m := range databaseAddressFlag.FindAllStringSubmatch(command, -1) {
		if !isOwnVariableReference(m[2], ours) {
			return "it names a database address itself (" + m[1] + ")"
		}
	}
	for _, m := range databaseClientHost.FindAllStringSubmatch(command, -1) {
		if !isOwnVariableReference(m[2], ours) {
			return "it names a database host itself (" + m[1] + " -h)"
		}
	}
	if conninfoAddress.MatchString(command) {
		return "it names a database host itself"
	}
	return ""
}

// isOwnVariableReference reports whether value is "$NAME" or "${NAME}" for one of
// the preview database's variables: the address the preview sets.
func isOwnVariableReference(value string, ours map[string]bool) bool {
	m := ownVariableReference.FindStringSubmatch(strings.Trim(value, `"'`))
	return m != nil && ours[m[1]]
}

// redactPreDeployCommand is command as the result and the Step log show it: what
// can be a secret is replaced — an inline NAME=value's value, a URL's credentials,
// the value of a flag, header or parameter named like a secret, mysql's
// -p<password>, and any long word of letters and digits (a key or token).
func redactPreDeployCommand(command string) string {
	command = assignmentValue.ReplaceAllString(command, "${1}${2}=***")
	command = urlCredentials.ReplaceAllString(command, "://***@")
	command = secretHeader.ReplaceAllString(command, "${1}${2}***")
	command = secretFlag.ReplaceAllString(command, "${1}${2}***")
	command = mysqlPassword.ReplaceAllString(command, "${1}***")
	command = secretParameter.ReplaceAllString(command, "${1}***")
	return keyLikeWord.ReplaceAllStringFunc(command, func(word string) string {
		if hasLetter.MatchString(word) && hasDigit.MatchString(word) {
			return "***"
		}
		return word
	})
}

// previewDatabaseEvidenceMax is how many evidence files the result and the Step
// log list; the rest are counted. previewDatabaseEvidencePathMax caps each path's
// bytes and previewDatabaseLogLineMax every Step log line's: the Step log reads
// lines of up to 64 KiB.
const (
	previewDatabaseEvidenceMax     = 20
	previewDatabaseEvidencePathMax = 200
	previewDatabaseLogLineMax      = 4096
)

// previewDatabaseEvidence is evidence sorted, at most previewDatabaseEvidenceMax
// of it with long paths shortened, and how many more there are.
func previewDatabaseEvidence(evidence []string) ([]string, int) {
	sorted := append([]string{}, evidence...)
	sort.Strings(sorted)
	more := 0
	if len(sorted) > previewDatabaseEvidenceMax {
		sorted, more = sorted[:previewDatabaseEvidenceMax], len(sorted)-previewDatabaseEvidenceMax
	}
	for i, p := range sorted {
		if len(p) > previewDatabaseEvidencePathMax {
			// Keep the end: the file name says the most.
			tail := p[len(p)-previewDatabaseEvidencePathMax:]
			for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
				tail = tail[1:]
			}
			sorted[i] = "…" + tail
		}
	}
	return sorted, more
}

// writePreviewDatabaseLogLine writes line to the Step log, cut to
// previewDatabaseLogLineMax bytes.
func writePreviewDatabaseLogLine(w io.Writer, line string) {
	if len(line) > previewDatabaseLogLineMax {
		cut := previewDatabaseLogLineMax
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		line = line[:cut] + "…"
	}
	io.WriteString(w, line+"\n")
}

func newPreviewDatabaseResult(plan PreviewDatabasePlan, migrations string) *previewDatabaseResult {
	evidence, more := previewDatabaseEvidence(plan.Evidence)
	if more > 0 {
		evidence = append(evidence, fmt.Sprintf("(and %d more files)", more))
	}
	return &previewDatabaseResult{
		Engine:     plan.Engine,
		Version:    plan.Version,
		Image:      plan.Image,
		Variables:  append([]string{}, plan.Variables...),
		Evidence:   evidence,
		Source:     plan.Source,
		Migrations: migrations,
	}
}

// logPreviewDatabasePlan writes the preview database, one line each, to the Step log.
func logPreviewDatabasePlan(w io.Writer, plan PreviewDatabasePlan) {
	if w == nil {
		return
	}
	source := "detected from the Task's code"
	if plan.Source == PreviewDatabaseSourceSetting {
		source = "from the service's setting"
	}
	lines := []string{
		fmt.Sprintf("Preview database: %s %s (%s), %s", plan.Engine, plan.Version, plan.Image, source),
		fmt.Sprintf("Preview database variables: %s", strings.Join(plan.Variables, ", ")),
	}
	if evidence, more := previewDatabaseEvidence(plan.Evidence); len(evidence) > 0 {
		line := "Preview database evidence: " + strings.Join(evidence, ", ")
		if more > 0 {
			line += fmt.Sprintf(" (and %d more files)", more)
		}
		lines = append(lines, line)
	}
	for _, name := range plan.NotInProduction {
		lines = append(lines, fmt.Sprintf("Preview database: %s is read in the code but not set in production; the preview sets it", name))
	}
	for _, line := range lines {
		writePreviewDatabaseLogLine(w, line)
	}
}

// logPreviewDatabaseMigrations writes whether the pre-deploy command ran.
func logPreviewDatabaseMigrations(w io.Writer, migrations string) {
	if w != nil {
		writePreviewDatabaseLogLine(w, "Preview database migrations: "+migrations)
	}
}

// logPreviewDatabaseNote writes why the preview has no database of its own.
func logPreviewDatabaseNote(w io.Writer, note string) {
	if w != nil {
		writePreviewDatabaseLogLine(w, "Preview database: none — "+note)
	}
}
