package dependencies

import (
	"regexp"
	"strconv"
	"strings"
)

// GoModMaxBytes bounds a go.mod read host-side.
const GoModMaxBytes = 4 << 20

// GoMod is what ParseGoMod reads of a go.mod.
type GoMod struct {
	// Module is the module path; "" when there is no module line.
	Module string
	// Requires is every required module path → its version, single-line or
	// in a require ( … ) block, // indirect included.
	Requires map[string]string
	// Replaced is every module path a replace directive replaces (its
	// left-hand path, with or without a version).
	Replaced map[string]bool
}

// ParseGoMod is the one small go.mod reader: the module path (module path or
// module ( path )), the requires and the replaced module paths. Every other
// directive's block is skipped whole.
func ParseGoMod(data []byte) GoMod {
	m := GoMod{Requires: map[string]string{}, Replaced: map[string]bool{}}
	// inOther is inside another directive's block (exclude, retract, …):
	// its lines are never read as directives.
	inRequire, inReplace, inModule, inOther := false, false, false, false
	for _, line := range strings.Split(string(data), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		// Module paths hold no parentheses: spacing them out makes
		// "require(" and "require ( )" ordinary fields.
		line = strings.NewReplacer("(", " ( ", ")", " ) ").Replace(line)
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch {
		case inModule:
			// module ( path ): the block holds the one module path.
			if fields[0] == ")" {
				inModule = false
			} else if m.Module == "" {
				m.Module = goModPath(fields[0])
			}
			continue
		case inOther:
			if fields[0] == ")" {
				inOther = false
			}
			continue
		case inRequire:
			if fields[0] == ")" {
				inRequire = false
			} else {
				m.addRequire(fields)
			}
			continue
		case inReplace:
			if fields[0] == ")" {
				inReplace = false
			} else {
				m.addReplace(fields)
			}
			continue
		}
		switch fields[0] {
		case "module":
			switch {
			case len(fields) >= 3 && fields[1] == "(" && fields[2] != ")":
				// module ( path ) on one line.
				m.Module = goModPath(fields[2])
				inModule = len(fields) < 4 || fields[3] != ")"
			case len(fields) >= 2 && fields[1] == "(":
				inModule = len(fields) < 3
			case len(fields) >= 2:
				m.Module = goModPath(fields[1])
			}
		case "require", "replace":
			block := &inRequire
			add := m.addRequire
			if fields[0] == "replace" {
				block, add = &inReplace, m.addReplace
			}
			switch {
			case len(fields) >= 3 && fields[1] == "(" && fields[2] == ")":
				// An empty block on one line: require ( ).
			case len(fields) >= 4 && fields[1] == "(" && fields[len(fields)-1] == ")":
				// A one-entry block on one line: require ( path version ).
				add(fields[2 : len(fields)-1])
			case len(fields) >= 2 && fields[1] == "(":
				*block = true
			case len(fields) >= 2:
				add(fields[1:])
			}
		default:
			// Another directive's block, unless it closes on the same line.
			inOther = len(fields) >= 2 && fields[1] == "(" && fields[len(fields)-1] != ")"
		}
	}
	return m
}

// addRequire records a require's path and version: path [version].
func (m GoMod) addRequire(fields []string) {
	version := ""
	if len(fields) >= 2 {
		version = goModPath(fields[1])
	}
	m.Requires[goModPath(fields[0])] = version
}

// addReplace records a replace's left-hand path: path [version] => target.
func (m GoMod) addReplace(fields []string) {
	if fields[0] != "=>" {
		m.Replaced[goModPath(fields[0])] = true
	}
}

// goModPath unquotes a quoted go.mod path.
func goModPath(field string) string {
	if unquoted, err := strconv.Unquote(field); err == nil {
		return unquoted
	}
	return field
}

// goPlainVersion is a plain Go version number: 1.27 or 1.27.1.
var goPlainVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+)?$`)

// GoBuildVersion is the Go version a go.mod builds with: its toolchain line's
// (toolchain go<version>) when present and plain, else its go line's when
// plain.
func GoBuildVersion(data []byte) (string, bool) {
	goLine, toolchain := ParseGoModVersions(data)
	if v, ok := strings.CutPrefix(toolchain, "go"); ok && goPlainVersion.MatchString(v) {
		return v, true
	}
	if goPlainVersion.MatchString(goLine) {
		return goLine, true
	}
	return "", false
}

// ParseGoModVersions is a go.mod's go and toolchain directives' values (""
// when absent), outside any block.
func ParseGoModVersions(data []byte) (goLine, toolchain string) {
	inBlock := false
	for _, line := range strings.Split(string(data), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		line = strings.NewReplacer("(", " ( ", ")", " ) ").Replace(line)
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if inBlock {
			if fields[0] == ")" {
				inBlock = false
			}
			continue
		}
		if len(fields) >= 2 && fields[1] == "(" {
			inBlock = fields[len(fields)-1] != ")"
			continue
		}
		switch {
		case fields[0] == "go" && len(fields) == 2 && goLine == "":
			goLine = fields[1]
		case fields[0] == "toolchain" && len(fields) == 2 && toolchain == "":
			toolchain = fields[1]
		}
	}
	return goLine, toolchain
}
