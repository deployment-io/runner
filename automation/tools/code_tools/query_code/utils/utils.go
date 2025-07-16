package utils

import "fmt"

func GetIdentifier(moduleName, packagePath, name, importPath string) string {

	// packagePath may be nested, e.g. pkg/subpkg
	if importPath != "" {
		// Use full importPath for external/cross-package references (may be nested)
		return fmt.Sprintf("%s#%s", importPath, name)
	}
	// Use moduleName and possibly-nested packagePath for local references
	return fmt.Sprintf("%s/%s#%s", moduleName, packagePath, name)
}
