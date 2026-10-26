package agenttools

// preview_database_detect.go detects, from a backend preview's service directory,
// which database the service needs and which variables hold its address, so
// deploy_web_service_preview can run a throwaway database beside it. Detection is
// deterministic and static: it reads files (never runs anything) and decides by
// the rules below. It is a pure function over an fs.FS.

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/deployment-io/deployment-runner-kit/task_previews"
)

const (
	// previewDatabaseDetectMaxFileBytes: a larger file isn't read.
	previewDatabaseDetectMaxFileBytes = 1 << 20
	// previewDatabaseDetectMaxFiles: detection stops after this many files.
	previewDatabaseDetectMaxFiles = 5000
)

// previewDatabaseSkippedDirs are never walked (nor is any hidden directory).
var previewDatabaseSkippedDirs = map[string]bool{
	"node_modules": true, "vendor": true, ".git": true, "dist": true, "build": true,
}

// PreviewDatabaseDetection is what detection found. Engine "" means no preview
// database, and Reason says why.
type PreviewDatabaseDetection struct {
	Engine  string
	Version string
	// Variables hold the database's address: one URL variable or split ones.
	Variables []string
	// Evidence are the files (relative to the service directory, sorted) the
	// engine and the variables were found in.
	Evidence []string
	// NotInProduction are the Variables the production deployment doesn't set.
	NotInProduction []string
	Reason          string
}

// The reasons detection finds no database.
const (
	previewDatabaseNoneDetected  = "no database detected"
	previewDatabaseNoVariable    = "couldn't tell which variable holds the database address"
	previewDatabaseOtherEngineOf = "found signs of %s, which preview databases don't support"
	previewDatabaseBothEngines   = "found signs of both %s and %s"
	previewDatabaseSeveralURLs   = "several database URL variables: %s"
	previewDatabaseFixedURL      = "%s sets the database address itself, not from a variable"
)

// engineSignals collects, per engine, the files that signal it.
type engineSignals map[string]map[string]bool

func (s engineSignals) add(engine, file string) {
	if s[engine] == nil {
		s[engine] = map[string]bool{}
	}
	s[engine][file] = true
}

// databaseVariables collects variable names and the files they were found in.
type databaseVariables struct {
	order []string
	files map[string]map[string]bool
}

func (v *databaseVariables) add(name, file string) {
	if v.files == nil {
		v.files = map[string]map[string]bool{}
	}
	if v.files[name] == nil {
		v.files[name] = map[string]bool{}
		v.order = append(v.order, name)
	}
	v.files[name][file] = true
}

// previewDatabaseScan is everything one walk of the tree collects.
type previewDatabaseScan struct {
	strong, weak engineSignals
	// composeTags are the compose image tags per engine, in walk order.
	composeTags map[string][]string
	// authoritative are the variables the database configuration names (Prisma's
	// url/directUrl, database.yml's url); railsAdapters are the files whose
	// database.yml has a postgres or mysql adapter and no url.
	authoritative databaseVariables
	railsNoURL    []string
	// fixedURL are the files whose database configuration sets a url that isn't
	// read from a variable: it would win over any variable the preview sets.
	fixedURL []string
	// read are the variables the code reads.
	read databaseVariables
}

// DetectPreviewDatabase detects the database the service in fsys needs.
// productionVariableNames are the production deployment's variable names: they
// choose between several URL variables, and a chosen variable not among them is
// reported in NotInProduction.
func DetectPreviewDatabase(fsys fs.FS, productionVariableNames []string) PreviewDatabaseDetection {
	scan := previewDatabaseScan{strong: engineSignals{}, weak: engineSignals{}, composeTags: map[string][]string{}}
	files := 0
	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() && p != "." {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p != "." && (previewDatabaseSkippedDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return fs.SkipDir
			}
			return nil
		}
		if files >= previewDatabaseDetectMaxFiles {
			return fs.SkipAll
		}
		files++
		// Regular files only: a symlink could point out of the tree.
		if !d.Type().IsRegular() {
			return nil
		}
		if !previewDatabaseFileOfInterest(p) {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > previewDatabaseDetectMaxFileBytes {
			return nil
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil || len(data) > previewDatabaseDetectMaxFileBytes {
			return nil
		}
		scan.file(p, string(data))
		return nil
	})
	return scan.decide(productionVariableNames)
}

// sourceExtensions are the files whose environment reads are collected.
var sourceExtensions = map[string]bool{
	".js": true, ".jsx": true, ".mjs": true, ".cjs": true, ".ts": true, ".tsx": true, ".mts": true, ".cts": true,
	".go": true, ".py": true, ".rb": true, ".java": true, ".kt": true, ".kts": true, ".scala": true, ".groovy": true,
	".prisma": true, ".erb": true, ".yml": true, ".yaml": true, ".php": true, ".ex": true, ".exs": true, ".rs": true, ".cs": true,
}

func previewDatabaseFileOfInterest(p string) bool {
	base := path.Base(p)
	switch {
	case base == "package.json", base == "go.mod", base == "Gemfile", base == "Pipfile", base == "pyproject.toml",
		base == "pom.xml", base == "build.gradle", base == "build.gradle.kts", base == ".env.example", base == "ormconfig.json":
		return true
	case strings.HasPrefix(base, "requirements") && strings.HasSuffix(base, ".txt"):
		return true
	case isComposeFile(base):
		return true
	}
	return sourceExtensions[path.Ext(base)]
}

func isComposeFile(base string) bool {
	if !strings.HasSuffix(base, ".yml") && !strings.HasSuffix(base, ".yaml") {
		return false
	}
	return strings.HasPrefix(base, "docker-compose") || strings.HasPrefix(base, "compose")
}

// file collects the signals and variables of one file.
func (s *previewDatabaseScan) file(p, content string) {
	base := path.Base(p)
	ext := path.Ext(base)
	switch {
	case base == "package.json":
		s.packageJSON(p, content)
	case base == "go.mod":
		s.substrings(p, content, goModDrivers)
	case strings.HasPrefix(base, "requirements") && ext == ".txt", base == "pyproject.toml", base == "Pipfile":
		s.pythonDependencies(p, content)
	case base == "Gemfile":
		for _, m := range gemfileGem.FindAllStringSubmatch(content, -1) {
			if engine, ok := gemDrivers[m[1]]; ok {
				s.weak.add(engine, p)
			}
		}
	case base == "pom.xml", base == "build.gradle", base == "build.gradle.kts":
		s.substrings(p, content, jvmDrivers)
	case base == ".env.example":
		for _, m := range dotenvKey.FindAllStringSubmatch(content, -1) {
			s.read.add(m[1], p)
		}
	case isComposeFile(base):
		s.compose(p, content)
		return
	}
	if ext == ".prisma" {
		s.prisma(p, content)
	}
	if p == "config/database.yml" || strings.HasSuffix(p, "/config/database.yml") {
		s.railsDatabaseYml(p, content)
	}
	if ext == ".py" {
		for _, m := range djangoEngine.FindAllStringSubmatch(content, -1) {
			switch {
			case strings.Contains(m[1], "postgresql") || strings.Contains(m[1], "postgis"):
				s.strong.add(task_previews.PreviewDatabaseEnginePostgres, p)
			case strings.Contains(m[1], "mysql"):
				s.strong.add(task_previews.PreviewDatabaseEngineMySQL, p)
			case strings.Contains(m[1], "sqlite"):
				s.strong.add("sqlite", p)
			case strings.Contains(m[1], "oracle"):
				s.strong.add("oracle", p)
			default:
				// Any other backend (CockroachDB, SQL Server, a custom one) is an
				// engine preview databases don't support: it must not fall through
				// to a driver dependency's weak signal.
				s.strong.add(m[1], p)
			}
		}
	}
	if isJavaScript(ext) || base == "ormconfig.json" {
		s.javaScriptORMs(p, base, content)
	}
	if sourceExtensions[ext] {
		for _, re := range envReads {
			for _, m := range re.FindAllStringSubmatch(content, -1) {
				s.read.add(m[1], p)
			}
		}
	}
}

func isJavaScript(ext string) bool {
	switch ext {
	case ".js", ".jsx", ".mjs", ".cjs", ".ts", ".tsx", ".mts", ".cts":
		return true
	}
	return false
}

// --- dependencies (weak signals) ----------------------------------------------

var packageJSONDrivers = map[string]string{
	"pg": task_previews.PreviewDatabaseEnginePostgres, "postgres": task_previews.PreviewDatabaseEnginePostgres,
	"mysql2": task_previews.PreviewDatabaseEngineMySQL, "mysql": task_previews.PreviewDatabaseEngineMySQL,
	"mongodb": task_previews.PreviewDatabaseEngineMongoDB, "mongoose": task_previews.PreviewDatabaseEngineMongoDB,
}

func (s *previewDatabaseScan) packageJSON(p, content string) {
	var pkg map[string]json.RawMessage
	if json.Unmarshal([]byte(content), &pkg) != nil {
		return
	}
	for _, section := range []string{"dependencies", "devDependencies", "peerDependencies", "optionalDependencies"} {
		var deps map[string]any
		if json.Unmarshal(pkg[section], &deps) != nil {
			continue
		}
		for name := range deps {
			if engine, ok := packageJSONDrivers[name]; ok {
				s.weak.add(engine, p)
			}
		}
	}
}

type substringDriver struct{ substring, engine string }

var goModDrivers = []substringDriver{
	{"github.com/lib/pq", task_previews.PreviewDatabaseEnginePostgres},
	{"github.com/jackc/pgx", task_previews.PreviewDatabaseEnginePostgres},
	{"gorm.io/driver/postgres", task_previews.PreviewDatabaseEnginePostgres},
	{"github.com/go-sql-driver/mysql", task_previews.PreviewDatabaseEngineMySQL},
	{"gorm.io/driver/mysql", task_previews.PreviewDatabaseEngineMySQL},
	{"go.mongodb.org/mongo-driver", task_previews.PreviewDatabaseEngineMongoDB},
}

var jvmDrivers = []substringDriver{
	{"org.postgresql", task_previews.PreviewDatabaseEnginePostgres},
	{"mysql-connector", task_previews.PreviewDatabaseEngineMySQL},
	{"mongodb-driver", task_previews.PreviewDatabaseEngineMongoDB},
}

func (s *previewDatabaseScan) substrings(p, content string, drivers []substringDriver) {
	for _, d := range drivers {
		if strings.Contains(content, d.substring) {
			s.weak.add(d.engine, p)
		}
	}
}

var pythonDrivers = map[string]string{
	"psycopg": task_previews.PreviewDatabaseEnginePostgres, "psycopg2": task_previews.PreviewDatabaseEnginePostgres,
	"psycopg2-binary": task_previews.PreviewDatabaseEnginePostgres, "asyncpg": task_previews.PreviewDatabaseEnginePostgres,
	"mysqlclient": task_previews.PreviewDatabaseEngineMySQL, "pymysql": task_previews.PreviewDatabaseEngineMySQL,
	"pymongo": task_previews.PreviewDatabaseEngineMongoDB, "motor": task_previews.PreviewDatabaseEngineMongoDB,
}

// pythonRequirement is a requirement's name at the start of a line, optionally
// quoted (pyproject.toml's lists, Pipfile's and Poetry's keys).
var pythonRequirement = regexp.MustCompile(`(?m)^\s*["']?([A-Za-z0-9_.-]+)`)

// quotedPythonRequirement is a quoted requirement anywhere on a line, as in
// pyproject.toml's inline arrays (dependencies = ["psycopg[binary]>=3"]): its
// name, its extras, then a version specifier, a marker or the closing quote.
var quotedPythonRequirement = regexp.MustCompile(`["']([A-Za-z0-9_.-]+)(?:\[[^\]"']*\])?\s*(?:[<>=!~;@]|["'])`)

func (s *previewDatabaseScan) pythonDependencies(p, content string) {
	matches := append(pythonRequirement.FindAllStringSubmatch(content, -1), quotedPythonRequirement.FindAllStringSubmatch(content, -1)...)
	for _, m := range matches {
		if engine, ok := pythonDrivers[strings.ToLower(m[1])]; ok {
			s.weak.add(engine, p)
		}
	}
}

var gemfileGem = regexp.MustCompile(`(?m)^\s*gem\s+['"]([A-Za-z0-9_-]+)['"]`)

var gemDrivers = map[string]string{
	"pg": task_previews.PreviewDatabaseEnginePostgres, "mysql2": task_previews.PreviewDatabaseEngineMySQL,
	"trilogy": task_previews.PreviewDatabaseEngineMySQL, "mongoid": task_previews.PreviewDatabaseEngineMongoDB,
}

// --- configuration (strong signals) -------------------------------------------

var (
	prismaDatasource = regexp.MustCompile(`(?s)datasource\s+\w+\s*\{(.*?)\}`)
	prismaProvider   = regexp.MustCompile(`(?m)^\s*provider\s*=\s*"([^"]+)"`)
	prismaURL        = regexp.MustCompile(`(?m)^\s*(url|directUrl)\s*=\s*env\(\s*"([^"]+)"\s*\)`)
	prismaAnyURL     = regexp.MustCompile(`(?m)^\s*(?:url|directUrl)\s*=`)
)

// prismaProviders maps a Prisma provider to its engine; any other is another engine.
var prismaProviders = map[string]string{
	"postgresql": task_previews.PreviewDatabaseEnginePostgres, "postgres": task_previews.PreviewDatabaseEnginePostgres,
	"mysql": task_previews.PreviewDatabaseEngineMySQL, "mongodb": task_previews.PreviewDatabaseEngineMongoDB,
}

func (s *previewDatabaseScan) prisma(p, content string) {
	for _, block := range prismaDatasource.FindAllStringSubmatch(content, -1) {
		if m := prismaProvider.FindStringSubmatch(block[1]); m != nil {
			engine, ok := prismaProviders[m[1]]
			if !ok {
				engine = m[1]
			}
			s.strong.add(engine, p)
		}
		if len(prismaAnyURL.FindAllString(block[1], -1)) > len(prismaURL.FindAllString(block[1], -1)) {
			s.fixedURL = append(s.fixedURL, p)
		}
		// url before directUrl, as Prisma reads them.
		var direct []string
		for _, m := range prismaURL.FindAllStringSubmatch(block[1], -1) {
			if m[1] == "url" {
				s.authoritative.add(m[2], p)
			} else {
				direct = append(direct, m[2])
			}
		}
		for _, name := range direct {
			s.authoritative.add(name, p)
		}
	}
}

var (
	// [ \t]*, never \s*: \s matches a newline, so an empty `url:` would take
	// the next line as its value.
	railsAdapter = regexp.MustCompile(`(?m)^[ \t]*adapter:[ \t]*["']?([A-Za-z0-9_]+)`)
	railsURL     = regexp.MustCompile(`(?m)^[ \t]*url:[ \t]*(.*)$`)
	railsEnvRead = regexp.MustCompile(`ENV(?:\[\s*|\.fetch\(\s*)["']([A-Za-z_][A-Za-z0-9_]*)["']`)
)

func railsAdapterEngine(adapter string) string {
	switch adapter {
	case "postgresql", "postgis":
		return task_previews.PreviewDatabaseEnginePostgres
	case "mysql2", "trilogy", "mysql":
		return task_previews.PreviewDatabaseEngineMySQL
	}
	return adapter
}

func (s *previewDatabaseScan) railsDatabaseYml(p, content string) {
	supported := false
	for _, m := range railsAdapter.FindAllStringSubmatch(content, -1) {
		engine := railsAdapterEngine(m[1])
		s.strong.add(engine, p)
		if engine == task_previews.PreviewDatabaseEnginePostgres || engine == task_previews.PreviewDatabaseEngineMySQL {
			supported = true
		}
	}
	explicitURL := false
	for _, m := range railsURL.FindAllStringSubmatch(content, -1) {
		value := strings.TrimSpace(m[1])
		if value == "" || value == `""` || value == "''" || value == "~" || value == "null" {
			continue
		}
		explicitURL = true
		reads := railsEnvRead.FindAllStringSubmatch(value, -1)
		if len(reads) == 0 {
			s.fixedURL = append(s.fixedURL, p)
		}
		for _, read := range reads {
			s.authoritative.add(read[1], p)
		}
	}
	if supported && !explicitURL {
		s.railsNoURL = append(s.railsNoURL, p)
	}
}

// djangoEngine is a Django DATABASES entry's "ENGINE": "django.db.backends.<x>".
var djangoEngine = regexp.MustCompile(`["']ENGINE["']\s*:\s*["']([^"']+)["']`)

// javaScriptORM is TypeORM's `type:`, Sequelize's `dialect:` or Knex's `client:`
// with a database value; ormMarkers are what the file must mention for each key,
// so an unrelated `type: "mysql"` isn't read as one.
var javaScriptORM = regexp.MustCompile(`["']?\b(type|dialect|client)["']?\s*:\s*["']([a-z0-9-]+)["']`)

var ormMarkers = map[string][]string{
	"type":    {"typeorm", "TypeORM", "DataSource", "TypeOrmModule"},
	"dialect": {"sequelize", "Sequelize"},
	"client":  {"knex", "Knex"},
}

var javaScriptORMEngines = map[string]string{
	"postgres": task_previews.PreviewDatabaseEnginePostgres, "postgresql": task_previews.PreviewDatabaseEnginePostgres,
	"pg":    task_previews.PreviewDatabaseEnginePostgres,
	"mysql": task_previews.PreviewDatabaseEngineMySQL, "mysql2": task_previews.PreviewDatabaseEngineMySQL,
	"mongodb": task_previews.PreviewDatabaseEngineMongoDB,
	// Other engines: a signal for none.
	"mariadb": "mariadb", "sqlite": "sqlite", "sqlite3": "sqlite", "better-sqlite3": "sqlite",
	"mssql": "mssql", "oracle": "oracle", "oracledb": "oracle", "cockroachdb": "cockroachdb",
}

func (s *previewDatabaseScan) javaScriptORMs(p, base, content string) {
	for _, m := range javaScriptORM.FindAllStringSubmatch(content, -1) {
		engine, ok := javaScriptORMEngines[m[2]]
		if !ok {
			continue
		}
		// ormconfig and knexfile are the ORM's own configuration by name.
		marked := (strings.HasPrefix(base, "ormconfig.") && m[1] == "type") || (strings.HasPrefix(base, "knexfile.") && m[1] == "client")
		for _, marker := range ormMarkers[m[1]] {
			if strings.Contains(content, marker) {
				marked = true
				break
			}
		}
		if marked {
			s.strong.add(engine, p)
		}
	}
}

// composeImageEngines maps an image's name (its last path segment) to its
// engine. A cache (Redis, Memcached…) is not the service's database and says
// nothing either way.
var composeImageEngines = map[string]string{
	"postgres": task_previews.PreviewDatabaseEnginePostgres, "postgis": task_previews.PreviewDatabaseEnginePostgres,
	"mysql": task_previews.PreviewDatabaseEngineMySQL, "mongo": task_previews.PreviewDatabaseEngineMongoDB,
	"mariadb": "mariadb", "cockroach": "cockroachdb", "mssql": "mssql",
}

var (
	composeLine     = regexp.MustCompile(`^(\s*)(-\s*)?["']?([A-Za-z0-9_.-]+)["']?\s*(?::\s*(.*)|=(.*))?$`)
	composeListItem = regexp.MustCompile(`^\s*-\s*["']?([A-Za-z_][A-Za-z0-9_]*)`)
)

// composeService is one compose service: its image and its environment's keys.
type composeService struct {
	image string
	env   []string
}

func (s *previewDatabaseScan) compose(p, content string) {
	for _, svc := range parseComposeServices(content) {
		name, tag := splitImage(svc.image)
		engine, isDatabase := composeImageEngines[name]
		if svc.image != "" && isDatabase {
			s.strong.add(engine, p)
			s.composeTags[engine] = append(s.composeTags[engine], tag)
			// The database's own init variables aren't the service's.
			continue
		}
		for _, key := range svc.env {
			s.read.add(key, p)
		}
	}
}

// parseComposeServices reads the services of a compose file with the little YAML
// it needs: each service's image and its environment's keys (list or map form).
func parseComposeServices(content string) []composeService {
	var services []composeService
	inServices, serviceIndent, envIndent := false, -1, -1
	var current *composeService
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimRight(raw, "\r ")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if indent == 0 {
			inServices = strings.HasPrefix(trimmed, "services:")
			serviceIndent, envIndent, current = -1, -1, nil
			continue
		}
		if !inServices {
			continue
		}
		if serviceIndent < 0 {
			serviceIndent = indent
		}
		if indent <= serviceIndent {
			envIndent = -1
			if indent == serviceIndent && strings.HasSuffix(trimmed, ":") {
				services = append(services, composeService{})
				current = &services[len(services)-1]
			}
			continue
		}
		if current == nil {
			continue
		}
		if envIndent >= 0 && indent > envIndent {
			if m := composeListItem.FindStringSubmatch(line); m != nil {
				current.env = append(current.env, m[1])
			} else if m := composeLine.FindStringSubmatch(line); m != nil && m[2] == "" {
				current.env = append(current.env, m[3])
			}
			continue
		}
		envIndent = -1
		m := composeLine.FindStringSubmatch(line)
		if m == nil || m[2] != "" {
			continue
		}
		switch m[3] {
		case "image":
			current.image = strings.Trim(strings.TrimSpace(m[4]), `"'`)
		case "environment":
			if strings.TrimSpace(m[4]) == "" {
				envIndent = indent
			}
		}
	}
	return services
}

// splitImage is an image reference's name (last path segment) and tag.
func splitImage(image string) (name, tag string) {
	ref := image
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	name = ref
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		name = ref[i+1:]
	}
	if i := strings.Index(name, ":"); i >= 0 {
		name, tag = name[:i], name[i+1:]
	}
	return strings.ToLower(name), tag
}

// --- environment reads --------------------------------------------------------

const envName = `([A-Za-z_][A-Za-z0-9_]*)`

var envReads = []*regexp.Regexp{
	regexp.MustCompile(`process\.env\.` + envName),
	regexp.MustCompile(`process\.env\[\s*["'` + "`" + `]` + envName + `["'` + "`" + `]\s*\]`),
	regexp.MustCompile(`os\.Getenv\(\s*"` + envName + `"`),
	regexp.MustCompile(`os\.environ\[\s*["']` + envName + `["']`),
	regexp.MustCompile(`os\.environ\.get\(\s*["']` + envName + `["']`),
	regexp.MustCompile(`os\.getenv\(\s*["']` + envName + `["']`),
	regexp.MustCompile(`ENV\[\s*["']` + envName + `["']`),
	regexp.MustCompile(`ENV\.fetch\(\s*["']` + envName + `["']`),
	regexp.MustCompile(`System\.getenv\(\s*"` + envName + `"`),
	regexp.MustCompile(`\benv\(\s*["']` + envName + `["']`),
}

var dotenvKey = regexp.MustCompile(`(?m)^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=`)

// --- the decision -------------------------------------------------------------

// decide applies the rules to what the walk collected.
func (s *previewDatabaseScan) decide(productionVariableNames []string) PreviewDatabaseDetection {
	engine, evidence, reason := s.engine()
	if engine == "" {
		return PreviewDatabaseDetection{Reason: reason}
	}
	variables, variableFiles, reason := s.variables(productionVariableNames)
	if len(variables) == 0 {
		return PreviewDatabaseDetection{Reason: reason}
	}
	for f := range variableFiles {
		evidence[f] = true
	}
	production := map[string]bool{}
	for _, name := range productionVariableNames {
		production[name] = true
	}
	var notInProduction []string
	for _, name := range variables {
		if !production[name] {
			notInProduction = append(notInProduction, name)
		}
	}
	return PreviewDatabaseDetection{
		Engine:          engine,
		Version:         s.version(engine),
		Variables:       variables,
		Evidence:        sortedKeys(evidence),
		NotInProduction: notInProduction,
	}
}

// engine is the one all strong signals agree on or, with none, the one all weak
// signals agree on — when it's an allowlisted engine.
func (s *previewDatabaseScan) engine() (string, map[string]bool, string) {
	signals := s.strong
	if len(signals) == 0 {
		signals = s.weak
	}
	if len(signals) == 0 {
		return "", nil, previewDatabaseNoneDetected
	}
	engines := make([]string, 0, len(signals))
	for engine := range signals {
		engines = append(engines, engine)
	}
	sort.Strings(engines)
	if len(engines) > 1 {
		return "", nil, fmt.Sprintf(previewDatabaseBothEngines, engines[0], engines[1])
	}
	if _, ok := task_previews.LookupPreviewDatabaseEngine(engines[0]); !ok {
		return "", nil, fmt.Sprintf(previewDatabaseOtherEngineOf, engines[0])
	}
	evidence := map[string]bool{}
	for f := range signals[engines[0]] {
		evidence[f] = true
	}
	return engines[0], evidence, ""
}

// version is the first compose image tag for engine whose version is allowlisted,
// else the engine's default.
func (s *previewDatabaseScan) version(engine string) string {
	e, _ := task_previews.LookupPreviewDatabaseEngine(engine)
	for _, tag := range s.composeTags[engine] {
		if v, ok := allowlistedVersionOfTag(e, tag); ok {
			return v
		}
	}
	return e.DefaultVersion
}

// allowlistedVersionOfTag is the allowlisted version an image tag runs: the version
// the tag is or starts with ("16", "16.4", "16-alpine", "8.0.36"), or the only
// allowlisted version of the tag's major ("7" → "7.0").
func allowlistedVersionOfTag(e task_previews.PreviewDatabaseEngineV1, tag string) (string, bool) {
	for _, v := range e.Versions {
		if tag == v || strings.HasPrefix(tag, v+".") || strings.HasPrefix(tag, v+"-") {
			return v, true
		}
	}
	major := tag
	if i := strings.IndexAny(major, ".-"); i >= 0 {
		major = major[:i]
	}
	if major == "" {
		return "", false
	}
	var matches []string
	for _, v := range e.Versions {
		if strings.SplitN(v, ".", 2)[0] == major {
			matches = append(matches, v)
		}
	}
	if len(matches) == 1 {
		return matches[0], true
	}
	return "", false
}

// variables are the variables that hold the database's address and the files
// they were found in, or why there are none.
func (s *previewDatabaseScan) variables(productionVariableNames []string) ([]string, map[string]bool, string) {
	files := map[string]bool{}
	if len(s.fixedURL) > 0 {
		return nil, nil, fmt.Sprintf(previewDatabaseFixedURL, s.fixedURL[0])
	}
	if len(s.authoritative.order) > 0 {
		for _, name := range s.authoritative.order {
			for f := range s.authoritative.files[name] {
				files[f] = true
			}
		}
		return append([]string(nil), s.authoritative.order...), files, ""
	}
	if len(s.railsNoURL) > 0 {
		for _, f := range s.railsNoURL {
			files[f] = true
		}
		return []string{"DATABASE_URL"}, files, ""
	}
	var urls, split []string
	for _, name := range s.read.order {
		switch kind, _ := task_previews.ClassifyDatabaseVariable(name); kind {
		case task_previews.DatabaseURLVariable:
			urls = append(urls, name)
		case task_previews.DatabaseSplitVariable:
			split = append(split, name)
		}
	}
	sort.Strings(urls)
	sort.Strings(split)
	chosen := split
	switch {
	case len(urls) == 1:
		chosen = urls
	case len(urls) > 1:
		production := map[string]bool{}
		for _, name := range productionVariableNames {
			production[name] = true
		}
		var kept []string
		for _, name := range urls {
			if production[name] {
				kept = append(kept, name)
			}
		}
		if len(kept) != 1 {
			if len(kept) > 1 {
				urls = kept
			}
			return nil, nil, fmt.Sprintf(previewDatabaseSeveralURLs, strings.Join(urls, ", "))
		}
		chosen = kept
	}
	if len(chosen) == 0 {
		return nil, nil, previewDatabaseNoVariable
	}
	for _, name := range chosen {
		for f := range s.read.files[name] {
			files[f] = true
		}
	}
	return chosen, files, ""
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
