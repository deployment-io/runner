package agenttools

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

func treeFS(files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, content := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(content)}
	}
	return fsys
}

const prismaPostgres = `datasource db {
  provider  = "postgresql"
  url       = env("DATABASE_URL")
  directUrl = env("DIRECT_URL")
}

generator client {
  provider = "prisma-client-js"
}
`

func TestDetectPreviewDatabaseStrongSignals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		files  map[string]string
		engine string
		vars   []string
	}{
		{"prisma postgres", map[string]string{"prisma/schema.prisma": prismaPostgres}, "postgres", []string{"DATABASE_URL", "DIRECT_URL"}},
		{"prisma postgres alias", map[string]string{"schema.prisma": "datasource db {\n provider = \"postgres\"\n url = env(\"PG_URL\")\n}\n"}, "postgres", []string{"PG_URL"}},
		{"prisma mysql", map[string]string{"schema.prisma": "datasource db {\n provider = \"mysql\"\n url = env(\"DATABASE_URL\")\n}\n"}, "mysql", []string{"DATABASE_URL"}},
		{"prisma mongodb", map[string]string{"schema.prisma": "datasource db {\n provider = \"mongodb\"\n url = env(\"MONGODB_URI\")\n}\n"}, "mongodb", []string{"MONGODB_URI"}},
		{"rails postgresql", map[string]string{"config/database.yml": "default: &default\n  adapter: postgresql\n  pool: 5\n"}, "postgres", []string{"DATABASE_URL"}},
		{"rails postgis", map[string]string{"config/database.yml": "default:\n  adapter: postgis\n"}, "postgres", []string{"DATABASE_URL"}},
		{"rails mysql2", map[string]string{"config/database.yml": "default:\n  adapter: mysql2\n"}, "mysql", []string{"DATABASE_URL"}},
		{"rails trilogy", map[string]string{"config/database.yml": "default:\n  adapter: trilogy\n"}, "mysql", []string{"DATABASE_URL"}},
		{"django postgresql", map[string]string{
			"app/settings.py": "DATABASES = {'default': {'ENGINE': 'django.db.backends.postgresql', 'HOST': os.environ['DB_HOST'], 'NAME': os.getenv('DB_NAME')}}\n",
		}, "postgres", []string{"DB_HOST", "DB_NAME"}},
		{"django mysql", map[string]string{
			"settings.py": "DATABASES = {\"default\": {\"ENGINE\": \"django.db.backends.mysql\"}}\nURL = os.environ.get(\"DATABASE_URL\")\n",
		}, "mysql", []string{"DATABASE_URL"}},
		{"compose postgres", map[string]string{
			"docker-compose.yml": "services:\n  db:\n    image: postgres:16\n  api:\n    build: .\n    environment:\n      - DATABASE_URL=postgres://x\n",
		}, "postgres", []string{"DATABASE_URL"}},
		{"compose mysql", map[string]string{
			"compose.yaml": "services:\n  db:\n    image: mysql:8.0\n  api:\n    environment:\n      MYSQL_URL: mysql://x\n",
		}, "mysql", []string{"MYSQL_URL"}},
		{"compose mongo", map[string]string{
			"docker-compose.dev.yml": "services:\n  mongo:\n    image: \"mongo:7.0\"\n",
			"src/index.ts":           "const uri = process.env.MONGODB_URI;\n",
		}, "mongodb", []string{"MONGODB_URI"}},
		{"typeorm", map[string]string{
			"src/data-source.ts": "import { DataSource } from 'typeorm'\nexport default new DataSource({ type: 'postgres', url: process.env.DATABASE_URL })\n",
		}, "postgres", []string{"DATABASE_URL"}},
		{"sequelize", map[string]string{
			"db.js": "const { Sequelize } = require('sequelize')\nnew Sequelize(process.env[\"DB_URL\"], { dialect: 'mysql' })\n",
		}, "mysql", []string{"DB_URL"}},
		{"knex", map[string]string{
			"knexfile.js": "module.exports = { client: 'pg', connection: process.env.DATABASE_URL }\n",
		}, "postgres", []string{"DATABASE_URL"}},
		// The knexfile's strong signal decides over dependencies for both engines.
		{"knexfile over mixed dependencies", map[string]string{
			"knexfile.ts":  "export default { client: 'pg', connection: process.env.DATABASE_URL }\n",
			"package.json": `{"dependencies": {"pg": "8", "mysql2": "3"}}`,
		}, "postgres", []string{"DATABASE_URL"}},
		{"typeorm mongodb", map[string]string{
			"ormconfig.json": `{"type": "mongodb", "url": "x"}`,
			"main.go":        `uri := os.Getenv("MONGO_URL")`,
		}, "mongodb", []string{"MONGO_URL"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := DetectPreviewDatabase(treeFS(tc.files), nil)
			if got.Engine != tc.engine || !reflect.DeepEqual(got.Variables, tc.vars) {
				t.Errorf("got %+v; want %s %v", got, tc.engine, tc.vars)
			}
			if len(got.Evidence) == 0 {
				t.Errorf("no evidence: %+v", got)
			}
		})
	}
}

func TestDetectPreviewDatabaseWeakSignalsOnly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		files  map[string]string
		engine string
	}{
		{"package.json pg", map[string]string{"package.json": `{"dependencies": {"pg": "^8"}}`}, "postgres"},
		{"package.json mongoose", map[string]string{"package.json": `{"devDependencies": {"mongoose": "^8"}}`}, "mongodb"},
		{"go.mod pgx", map[string]string{"go.mod": "module x\nrequire github.com/jackc/pgx/v5 v5.0.0\n"}, "postgres"},
		{"go.mod mysql", map[string]string{"go.mod": "require github.com/go-sql-driver/mysql v1.7.0\n"}, "mysql"},
		{"go.mod mongo", map[string]string{"go.mod": "require go.mongodb.org/mongo-driver v1.17.0\n"}, "mongodb"},
		{"requirements psycopg2-binary", map[string]string{"requirements.txt": "Django==5.0\npsycopg2-binary>=2.9\n"}, "postgres"},
		{"requirements PyMySQL", map[string]string{"requirements-prod.txt": "PyMySQL==1.1\n"}, "mysql"},
		{"pyproject motor", map[string]string{"pyproject.toml": "[project]\ndependencies = [\n  \"motor>=3\",\n]\n"}, "mongodb"},
		{"pyproject inline psycopg", map[string]string{"pyproject.toml": "[project]\ndescription = \"motor control\"\ndependencies = [\"fastapi>=0.110\", \"psycopg[binary]>=3\"]\n"}, "postgres"},
		{"pyproject inline asyncpg", map[string]string{"pyproject.toml": "[project]\ndependencies = ['asyncpg', 'uvicorn']\n"}, "postgres"},
		{"Gemfile pg", map[string]string{"Gemfile": "gem 'rails'\ngem \"pg\", \"~> 1.5\"\n"}, "postgres"},
		{"Gemfile mongoid", map[string]string{"Gemfile": "gem 'mongoid'\n"}, "mongodb"},
		{"pom postgres", map[string]string{"pom.xml": "<groupId>org.postgresql</groupId>"}, "postgres"},
		{"gradle mysql", map[string]string{"build.gradle.kts": `implementation("com.mysql:mysql-connector-j:8.3.0")`}, "mysql"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{".env.example": "DATABASE_URL=\nPORT=3000\n"}
			for k, v := range tc.files {
				files[k] = v
			}
			got := DetectPreviewDatabase(treeFS(files), nil)
			if got.Engine != tc.engine || !reflect.DeepEqual(got.Variables, []string{"DATABASE_URL"}) {
				t.Errorf("got %+v; want %s", got, tc.engine)
			}
		})
	}
}

func TestDetectPreviewDatabaseConflictsAndOtherEngines(t *testing.T) {
	read := map[string]string{"src/db.ts": "process.env.DATABASE_URL"}
	for _, tc := range []struct {
		name   string
		files  map[string]string
		engine string
		reason string
	}{
		{"strong conflict", map[string]string{
			"schema.prisma":      "datasource db {\n provider = \"postgresql\"\n}\n",
			"docker-compose.yml": "services:\n  db:\n    image: mysql:8.4\n",
		}, "", "found signs of both mysql and postgres"},
		{"weak conflict without strong", map[string]string{
			"package.json": `{"dependencies": {"pg": "1", "mongodb": "1"}}`,
		}, "", "found signs of both mongodb and postgres"},
		{"strong wins over a weak conflict", map[string]string{
			"schema.prisma": "datasource db {\n provider = \"postgresql\"\n}\n",
			"package.json":  `{"dependencies": {"mysql2": "1"}}`,
		}, "postgres", ""},
		{"sqlite", map[string]string{"schema.prisma": "datasource db {\n provider = \"sqlite\"\n}\n"}, "",
			"found signs of sqlite, which preview databases don't support"},
		{"mariadb", map[string]string{"docker-compose.yml": "services:\n  db:\n    image: mariadb:11\n"}, "",
			"found signs of mariadb, which preview databases don't support"},
		{"cockroachdb vs postgres", map[string]string{
			"docker-compose.yml": "services:\n  db:\n    image: cockroachdb/cockroach:v23\n",
			"schema.prisma":      "datasource db {\n provider = \"cockroachdb\"\n}\n",
		}, "", "found signs of cockroachdb, which preview databases don't support"},
		{"a cache says nothing", map[string]string{
			"docker-compose.yml": "services:\n  cache:\n    image: redis:7\n  db:\n    image: postgres:16\n",
		}, "postgres", ""},
		{"nothing", map[string]string{"README.md": "hello"}, "", "no database detected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{}
			for k, v := range read {
				files[k] = v
			}
			for k, v := range tc.files {
				files[k] = v
			}
			got := DetectPreviewDatabase(treeFS(files), nil)
			if got.Engine != tc.engine || got.Reason != tc.reason {
				t.Errorf("got %+v; want engine %q reason %q", got, tc.engine, tc.reason)
			}
		})
	}
}

func TestDetectPreviewDatabaseComposeVersion(t *testing.T) {
	for image, want := range map[string]string{
		"postgres:15-alpine": "15",
		"postgres:13.4":      "13",
		"postgres:12":        "16",
		"postgres":           "16",
		"public.ecr.aws/docker/library/postgres:17": "17",
		"mysql:8.0.36":        "8.0",
		"mysql:8":             "8.4",
		"mysql:5.7":           "8.4",
		"mongo:6":             "6.0",
		"mongo:6.0.14":        "6.0",
		"mongo:4.4":           "7.0",
		"postgres@sha256:abc": "16",
		"docker.io/library/postgres:14.2-bookworm":   "14",
		"registry.example.com:5000/postgres:15-slim": "15",
	} {
		t.Run(image, func(t *testing.T) {
			got := DetectPreviewDatabase(treeFS(map[string]string{
				"docker-compose.yml": "services:\n  db:\n    image: " + image + "\n",
				"main.go":            `os.Getenv("DATABASE_URL")`,
			}), nil)
			if got.Version != want {
				t.Errorf("version %q; want %q (%+v)", got.Version, want, got)
			}
		})
	}
}

func TestDetectPreviewDatabaseVariables(t *testing.T) {
	strong := map[string]string{"docker-compose.yml": "services:\n  db:\n    image: postgres:16\n    environment:\n      POSTGRES_PASSWORD: x\n"}
	for _, tc := range []struct {
		name       string
		files      map[string]string
		production []string
		vars       []string
		reason     string
		notInProd  []string
	}{
		{name: "rails url from ENV", files: map[string]string{
			"config/database.yml": "production:\n  adapter: postgresql\n  url: <%= ENV['PRIMARY_DB_URL'] %>\n",
		}, vars: []string{"PRIMARY_DB_URL"}, notInProd: []string{"PRIMARY_DB_URL"}},
		{name: "rails url from ENV.fetch", files: map[string]string{
			"config/database.yml": "production:\n  adapter: postgresql\n  url: <%= ENV.fetch(\"POSTGRES_URL\") %>\n",
		}, production: []string{"POSTGRES_URL"}, vars: []string{"POSTGRES_URL"}},
		{name: "rails url set in the file", files: map[string]string{
			"config/database.yml": "production:\n  adapter: postgresql\n  url: postgres://app:secret@shared-db/app\n",
			"app.rb":              `ENV["DATABASE_URL"]`,
		}, reason: "config/database.yml sets the database address itself, not from a variable"},
		{name: "rails url from credentials", files: map[string]string{
			"config/database.yml": "production:\n  adapter: postgresql\n  url: <%= Rails.application.credentials.database_url %>\n",
		}, reason: "config/database.yml sets the database address itself, not from a variable"},
		{name: "rails empty url", files: map[string]string{
			"config/database.yml": "production:\n  adapter: postgresql\n  url:\n",
		}, vars: []string{"DATABASE_URL"}, notInProd: []string{"DATABASE_URL"}},
		{name: "prisma url set in the file", files: map[string]string{
			"prisma/schema.prisma": "datasource db {\n  provider = \"postgresql\"\n  url = \"postgresql://app:secret@shared-db/app\"\n}\n",
			"src/a.ts":             "process.env.DATABASE_URL",
		}, reason: "prisma/schema.prisma sets the database address itself, not from a variable"},
		{name: "a single read URL variable", files: map[string]string{
			"src/a.js": "const a = process.env.DATABASE_URL; const b = process.env.API_URL",
		}, production: []string{"DATABASE_URL"}, vars: []string{"DATABASE_URL"}},
		{name: "several narrowed by production", files: map[string]string{
			"src/a.js": "process.env.DATABASE_URL; process.env.POSTGRES_URL",
		}, production: []string{"POSTGRES_URL", "SECRET"}, vars: []string{"POSTGRES_URL"}},
		{name: "several left", files: map[string]string{
			"src/a.js": "process.env.DATABASE_URL; process.env.POSTGRES_URL",
		}, production: []string{"DATABASE_URL", "POSTGRES_URL"}, reason: "several database URL variables: DATABASE_URL, POSTGRES_URL"},
		{name: "several, none in production", files: map[string]string{
			"src/a.js": "process.env.DATABASE_URL; process.env.POSTGRES_URL",
		}, reason: "several database URL variables: DATABASE_URL, POSTGRES_URL"},
		{name: "split variables", files: map[string]string{
			"main.go":      `os.Getenv("DB_HOST"); os.Getenv("DB_PORT"); os.Getenv("DB_PASSWORD"); os.Getenv("PORT")`,
			".env.example": "DB_USER=\nDB_NAME=\n",
		}, production: []string{"DB_HOST", "DB_NAME", "DB_PASSWORD", "DB_PORT", "DB_USER"},
			vars: []string{"DB_HOST", "DB_NAME", "DB_PASSWORD", "DB_PORT", "DB_USER"}},
		{name: "libpq variables", files: map[string]string{
			"App.java": `System.getenv("PGHOST"); System.getenv("PGDATABASE")`,
		}, production: []string{"PGHOST"}, vars: []string{"PGDATABASE", "PGHOST"}, notInProd: []string{"PGDATABASE"}},
		{name: "ruby ENV reads", files: map[string]string{
			"app.rb": `ENV["DATABASE_URL"]`,
		}, vars: []string{"DATABASE_URL"}, notInProd: []string{"DATABASE_URL"}},
		{name: "nothing", files: map[string]string{
			"src/a.js": "process.env.API_URL",
		}, reason: "couldn't tell which variable holds the database address"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{}
			for k, v := range strong {
				files[k] = v
			}
			for k, v := range tc.files {
				files[k] = v
			}
			got := DetectPreviewDatabase(treeFS(files), tc.production)
			if !reflect.DeepEqual(got.Variables, tc.vars) || got.Reason != tc.reason || !reflect.DeepEqual(got.NotInProduction, tc.notInProd) {
				t.Errorf("got %+v; want vars %v reason %q not in production %v", got, tc.vars, tc.reason, tc.notInProd)
			}
			if tc.reason != "" && got.Engine != "" {
				t.Errorf("an engine without variables: %+v", got)
			}
			// The database container's own variables are never the service's.
			for _, v := range got.Variables {
				if v == "POSTGRES_PASSWORD" {
					t.Errorf("read the database service's init variable: %v", got.Variables)
				}
			}
		})
	}
}

func TestDetectPreviewDatabaseEvidence(t *testing.T) {
	got := DetectPreviewDatabase(treeFS(map[string]string{
		"prisma/schema.prisma": prismaPostgres,
		"package.json":         `{"dependencies": {"pg": "1"}}`,
		"src/index.ts":         "process.env.DATABASE_URL",
	}), []string{"DATABASE_URL", "DIRECT_URL"})
	want := PreviewDatabaseDetection{
		Engine: "postgres", Version: "16", Variables: []string{"DATABASE_URL", "DIRECT_URL"},
		Evidence: []string{"prisma/schema.prisma"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
	got = DetectPreviewDatabase(treeFS(map[string]string{
		"go.mod":         "require github.com/lib/pq v1\n",
		"cmd/api/db.go":  `os.Getenv("DATABASE_URL")`,
		"internal/x.go":  `os.Getenv("DATABASE_URL")`,
		"internal/y.go":  `os.Getenv("OTHER")`,
		"docs/readme.md": "DATABASE_URL",
	}), nil)
	if !reflect.DeepEqual(got.Evidence, []string{"cmd/api/db.go", "go.mod", "internal/x.go"}) {
		t.Errorf("evidence %v", got.Evidence)
	}
}

func TestDetectPreviewDatabaseSkipsDirectoriesAndLargeFiles(t *testing.T) {
	for _, dir := range []string{"node_modules/pg", "vendor/x", ".git", "dist", "build", ".cache", "a/node_modules"} {
		t.Run(dir, func(t *testing.T) {
			got := DetectPreviewDatabase(treeFS(map[string]string{
				dir + "/schema.prisma": prismaPostgres,
				dir + "/package.json":  `{"dependencies": {"pg": "1"}}`,
			}), nil)
			if got.Engine != "" || got.Reason != "no database detected" {
				t.Errorf("read %s: %+v", dir, got)
			}
		})
	}
	big := prismaPostgres + strings.Repeat(" ", previewDatabaseDetectMaxFileBytes)
	if got := DetectPreviewDatabase(treeFS(map[string]string{"schema.prisma": big}), nil); got.Engine != "" {
		t.Errorf("read a file over 1 MB: %+v", got)
	}
}

func TestDetectPreviewDatabaseStopsAtTheFileCap(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < previewDatabaseDetectMaxFiles; i++ {
		files[fmt.Sprintf("a/f%05d.txt", i)] = ""
	}
	// Walked after the first 5,000 files (lexical order).
	files["z/schema.prisma"] = prismaPostgres
	if got := DetectPreviewDatabase(treeFS(files), nil); got.Engine != "" {
		t.Errorf("read past the file cap: %+v", got)
	}
	delete(files, "a/f00000.txt")
	if got := DetectPreviewDatabase(treeFS(files), nil); got.Engine != "postgres" {
		t.Errorf("the 5,000th file wasn't read: %+v", got)
	}
}

func TestParseComposeServices(t *testing.T) {
	got := parseComposeServices(`version: "3.9"
services:
  # the database
  db:
    image: "postgres:16-alpine"
    environment:
      POSTGRES_USER: app
      POSTGRES_PASSWORD: secret
    ports:
      - "5432:5432"
  api:
    build:
      context: .
    environment:
      - DATABASE_URL=postgres://app:secret@db/app
      - "REDIS_URL=redis://cache"
      - PORT
    depends_on:
      - db
volumes:
  data:
`)
	want := []composeService{
		{image: "postgres:16-alpine", env: []string{"POSTGRES_USER", "POSTGRES_PASSWORD"}},
		{env: []string{"DATABASE_URL", "REDIS_URL", "PORT"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}
