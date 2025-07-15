package golang

import (
	"fmt"
	"github.com/deployment-io/deployment-runner/automation/tools/code_tools/query_code/types"
	"github.com/deployment-io/deployment-runner/automation/tools/code_tools/query_code/utils"
	treeSitter "github.com/tree-sitter/go-tree-sitter"
	treeSitterGo "github.com/tree-sitter/tree-sitter-go/bindings/go"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func parseGoModFile(path string) string {
	content, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}

	// Extract the module name
	moduleRegex := regexp.MustCompile(`module\s+(?P<name>[^\s]+)`)
	moduleMatch := moduleRegex.FindStringSubmatch(string(content))
	moduleName := moduleMatch[1] // The module name
	return moduleName
}

func GetModuleName(dir string) (string, error) {
	var moduleName string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if filepath.Ext(path) == ".mod" {
			moduleName = parseGoModFile(path)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return moduleName, nil
}

// getPackagePath calculates the (possibly nested) package path for a Go file.
// Params:
//   - moduleRoot: the absolute path to the module root directory (where go.mod resides)
//   - filePath: absolute or relative path to the Go file
//
// Returns: packagePath, e.g. "subpkg/nested/pkg"
func getPackagePath(moduleRoot, filePath string) (string, error) {
	absModuleRoot, err := filepath.Abs(moduleRoot)
	if err != nil {
		return "", fmt.Errorf("failed to resolve module root: %w", err)
	}
	absFilePath, err := filepath.Abs(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to resolve file path: %w", err)
	}
	dir := filepath.Dir(absFilePath)
	rel, err := filepath.Rel(absModuleRoot, dir)
	if err != nil {
		return "", fmt.Errorf("failed to get relative path: %w", err)
	}
	// Convert Windows path separator to "/"
	packagePath := filepath.ToSlash(rel)
	// If file is in module root, rel will be ".", so packagePath should be ""
	if packagePath == "." {
		packagePath = ""
	}
	return packagePath, nil
}

// AddNodes reads a Go file and extracts syntax nodes based on the provided Tree-sitter query
func AddNodes(dir, filePath string, queryContent string, graph *types.CodeGraph, moduleName string) error {
	// Read the file content
	content, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("error reading file: %s", err)
	}

	// Create a new parser and set the language to Go
	parser := treeSitter.NewParser()
	err = parser.SetLanguage(treeSitter.NewLanguage(treeSitterGo.Language()))
	if err != nil {
		return fmt.Errorf("error setting language: %s", err)
	}
	// Parse the content and get the syntax tree
	tree := parser.Parse(content, nil)
	rootNode := tree.RootNode()

	//packagePath, err := getPackageName(rootNode, content, filepath)
	//if err != nil {
	//	return err
	//}

	packagePath, err := getPackagePath(dir, filePath)
	if err != nil {
		return err
	}

	// Load the Tree-sitter query from the provided query content
	query, queryErr := treeSitter.NewQuery(treeSitter.NewLanguage(treeSitterGo.Language()), queryContent)
	if queryErr != nil {
		return queryErr
	}

	// Execute the query on the parsed syntax tree to extract matches
	queryCursor := treeSitter.NewQueryCursor()
	queryCaptures := queryCursor.Captures(query, rootNode, content)

	// Iterate over the captures and print them
	for {
		match, captureIndex := queryCaptures.Next()
		if match == nil {
			break
		}
		capture := match.Captures[captureIndex]
		node := capture.Node
		nodeKind := node.Kind()

		//fmt.Printf("Node kind is: %s\n", nodeKind)

		// Handle function declarations
		if nodeKind == "function_declaration" {
			funcNameNode := node.ChildByFieldName("name")
			if funcNameNode != nil {
				funcName := string(content[funcNameNode.StartByte():funcNameNode.EndByte()])
				funcIdentifier := utils.GetIdentifier(moduleName, packagePath, funcName, "")
				graph.AddNode(funcIdentifier, "function", filePath,
					fmt.Sprintf("%d:%d", node.StartPosition().Row+1, node.StartPosition().Column+1),
					fmt.Sprintf("%d:%d", node.EndPosition().Row+1, node.EndPosition().Column+1))
			}
		}

		// Handle method declarations
		if nodeKind == "method_declaration" {
			var methodName string
			funcNameNode := node.ChildByFieldName("name")
			methodName = string(content[funcNameNode.StartByte():funcNameNode.EndByte()])
			receiverNode := node.ChildByFieldName("receiver")
			if receiverNode != nil {
				parameterDeclarationNode := receiverNode.NamedChild(0)
				if parameterDeclarationNode != nil {
					parameterTypeNode := parameterDeclarationNode.ChildByFieldName("type")
					rT := string(content[parameterTypeNode.StartByte():parameterTypeNode.EndByte()])
					if rT[0] == '*' {
						rT = rT[1:]
					}
				}
			}

			if len(methodName) > 0 {
				methodIdentifier := utils.GetIdentifier(moduleName, packagePath, methodName, "")
				graph.AddNode(methodIdentifier, "method", filePath, fmt.Sprintf("%d:%d",
					node.StartPosition().Row+1, node.StartPosition().Column+1),
					fmt.Sprintf("%d:%d", node.EndPosition().Row+1, node.EndPosition().Column+1))
			}
		}
		if nodeKind == "type_declaration" {
			var typeName string
			var isStruct bool

			// Look for type_spec within the declaration
			for i := 0; i < int(capture.Node.ChildCount()); i++ {
				child := capture.Node.Child(uint(i))
				if child != nil && child.Kind() == "type_spec" {
					// Within type_spec, look for the name and check if it's a struct
					for j := 0; j < int(child.ChildCount()); j++ {
						grandchild := child.Child(uint(j))
						if grandchild == nil {
							continue
						}

						if grandchild.Kind() == "type_identifier" {
							typeName = string(content[grandchild.StartByte():grandchild.EndByte()])
						}

						if grandchild.Kind() == "struct_type" {
							isStruct = true
						}
					}
				}
			}

			// Print the struct name if we found both a name and confirmed it's a struct
			if len(typeName) > 0 && isStruct {
				structIdentifier := utils.GetIdentifier(moduleName, packagePath, typeName, "")
				graph.AddNode(structIdentifier, "struct", filePath,
					fmt.Sprintf("%d:%d", node.StartPosition().Row+1, node.StartPosition().Column+1),
					fmt.Sprintf("%d:%d", node.EndPosition().Row+1, node.EndPosition().Column+1))
			}
		}

	}
	return nil
}

func processImportSpec(spec *treeSitter.Node, content []byte, imports map[string]string) {
	pathNode := spec.ChildByFieldName("path")
	aliasNode := spec.ChildByFieldName("name")
	if pathNode != nil {
		rawPath := string(content[pathNode.StartByte():pathNode.EndByte()])
		cleanPath := strings.Trim(rawPath, `"`)
		var alias string
		if aliasNode != nil {
			alias = string(content[aliasNode.StartByte():aliasNode.EndByte()])
		} else {
			aliasParts := strings.Split(cleanPath, "/")
			alias = aliasParts[len(aliasParts)-1]
		}
		imports[alias] = cleanPath
	}
}

func extractImports(rootNode *treeSitter.Node, content []byte) map[string]string {
	imports := make(map[string]string)

	for i := 0; i < int(rootNode.ChildCount()); i++ {
		child := rootNode.Child(uint(i))
		if child.Kind() == "import_declaration" {
			for j := 0; j < int(child.NamedChildCount()); j++ {
				declChild := child.NamedChild(uint(j))
				switch declChild.Kind() {
				case "import_spec":
					processImportSpec(declChild, content, imports)
				case "import_spec_list":
					for k := 0; k < int(declChild.NamedChildCount()); k++ {
						spec := declChild.NamedChild(uint(k))
						if spec.Kind() == "import_spec" {
							processImportSpec(spec, content, imports)
						}
					}
				}
			}
		}
	}
	return imports
}

// In your function that extracts relationships from function bodies:
func extractFunctionRelationships(
	node *treeSitter.Node, content []byte, graph *types.CodeGraph, parentFuncIdentifier,
	moduleName, packagePath string, imports map[string]string,
) {
	// selector_expression identifies something like "otherPkg.Func"
	if node.Kind() == "selector_expression" {
		fieldNode := node.ChildByFieldName("field")
		operandNode := node.ChildByFieldName("operand")
		if fieldNode != nil && operandNode != nil {
			packageAlias := string(content[operandNode.StartByte():operandNode.EndByte()])
			fieldPart := string(content[fieldNode.StartByte():fieldNode.EndByte()])
			importPath := imports[packageAlias]
			typeIdentifier := utils.GetIdentifier(moduleName, packageAlias, fieldPart, importPath) // Use importPath if cross-package

			if _, exists := graph.NameToNodesMap[typeIdentifier]; exists {
				graph.AddEdge(parentFuncIdentifier, typeIdentifier, "uses")
				//fmt.Printf("Added edge from %s to %s (selector type reference)\n",
				//	parentFuncIdentifier, typeIdentifier)
			}
		}
	}

	// For function call expressions, use imports if found
	if node.Kind() == "call_expression" {
		funcCallNode := node.ChildByFieldName("function")
		if funcCallNode != nil {
			calledName := string(content[funcCallNode.StartByte():funcCallNode.EndByte()])
			// Detect if this is a selector like package.Func or just Func
			segments := strings.Split(calledName, ".")
			var calledFuncIdentifier string
			if len(segments) == 2 {
				pkgAlias := segments[0]
				funcName := segments[1]
				importPath := imports[pkgAlias]
				calledFuncIdentifier = utils.GetIdentifier(moduleName, pkgAlias, funcName, importPath)
			} else {
				calledFuncIdentifier = utils.GetIdentifier(moduleName, packagePath, calledName, "")
			}
			graph.AddEdge(parentFuncIdentifier, calledFuncIdentifier, "calls")
		}
	}

	// ...existing handling, but pass imports recursively
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(uint(i))
		extractFunctionRelationships(child, content, graph, parentFuncIdentifier, moduleName, packagePath, imports)
	}
}

func extractStructRelationships(node *treeSitter.Node, content []byte, graph *types.CodeGraph, moduleName, packageName string) {
	// Example 1: Parse Go embedded structs (inheritance)
	if node.Kind() == "type_spec" {
		// Check if the node contains an embedded struct
		structNode := node.ChildByFieldName("type")
		if structNode != nil && structNode.Kind() == "struct_type" {
			// Check embedded fields
			for i := 0; i < int(structNode.NamedChildCount()); i++ {
				fieldNode := structNode.NamedChild(uint(i))

				// Extract the struct's name
				structNameNode := node.ChildByFieldName("name")

				// Detect embedded struct (inheritance) explicitly

				for i := 0; i < int(fieldNode.NamedChildCount()); i++ {
					individualFieldNode := fieldNode.NamedChild(uint(i))
					//individualFieldName := string(content[individualFieldNode.StartByte():individualFieldNode.EndByte()])
					//fmt.Printf("Detected inherited struct: %s\n", individualFieldName)
					nameIdentifierNode := individualFieldNode.ChildByFieldName("name")
					typeIdentifierNode := individualFieldNode.ChildByFieldName("type")
					if typeIdentifierNode != nil {
						if structNameNode != nil {
							if nameIdentifierNode == nil {
								typeName := string(content[structNameNode.StartByte():structNameNode.EndByte()])
								//fmt.Printf("Struct name: %s\n", typeName)
								typeIdentifierName := string(content[typeIdentifierNode.StartByte():typeIdentifierNode.EndByte()])
								//fmt.Printf("embedded struct: %v\n", typeIdentifierName)
								structIdentifier := utils.GetIdentifier(moduleName, packageName, typeName, "")
								embeddedStructIdentifier := utils.GetIdentifier(moduleName, packageName, typeIdentifierName, "")
								graph.AddEdge(structIdentifier, embeddedStructIdentifier, "embeds")
							} else {
								typeName := string(content[structNameNode.StartByte():structNameNode.EndByte()])
								//fmt.Printf("Struct name: %s\n", typeName)
								typeIdentifierName := string(content[typeIdentifierNode.StartByte():typeIdentifierNode.EndByte()])
								//fmt.Printf("uses: %v\n", typeIdentifierName)
								structIdentifier := utils.GetIdentifier(moduleName, packageName, typeName, "")
								usedTypeIdentifier := utils.GetIdentifier(moduleName, packageName, typeIdentifierName, "")
								graph.AddEdge(structIdentifier, usedTypeIdentifier, "uses")
							}
						}

					}
				}
			}
		}
	}
}

func AddEdges(dir string, path string, queryContent string, graph *types.CodeGraph, moduleName string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read file %s: %v", path, err)
	}

	// Create TreeSitter parser
	parser := treeSitter.NewParser()
	err = parser.SetLanguage(treeSitter.NewLanguage(treeSitterGo.Language()))
	if err != nil {
		return fmt.Errorf("failed to set language: %v", err)
	}
	// Parse the content and get the syntax tree
	tree := parser.Parse(content, nil)
	rootNode := tree.RootNode()

	//packagePath, err := getPackageName(rootNode, content, filePath)
	//if err != nil {
	//	return err
	//}

	packagePath, err := getPackagePath(dir, path)
	if err != nil {
		return err
	}

	imports := extractImports(rootNode, content)

	query, queryErr := treeSitter.NewQuery(treeSitter.NewLanguage(treeSitterGo.Language()), queryContent)
	if queryErr != nil {
		return queryErr
	}

	// Execute the query on the parsed syntax tree to extract matches
	queryCursor := treeSitter.NewQueryCursor()
	queryCaptures := queryCursor.Captures(query, rootNode, content)

	// Process all matches from the query
	// Iterate over the captures and print them
	for {
		match, captureIndex := queryCaptures.Next()
		if match == nil {
			break
		}
		capture := match.Captures[captureIndex]
		node := capture.Node
		nodeKind := node.Kind()

		if nodeKind == "function_declaration" {
			funcNameNode := node.ChildByFieldName("name")
			if funcNameNode != nil {
				funcName := string(content[funcNameNode.StartByte():funcNameNode.EndByte()])
				funcIdentifier := utils.GetIdentifier(moduleName, packagePath, funcName, "")

				//Find statements within the function body and add edges
				bodyNode := node.ChildByFieldName("body")
				if bodyNode != nil {
					extractFunctionRelationships(bodyNode, content, graph, funcIdentifier, moduleName, packagePath, imports)
				}
			}
		}

		if nodeKind == "method_declaration" {
			var methodName string
			//var receiverType string
			methodNameNode := node.ChildByFieldName("name")
			methodName = string(content[methodNameNode.StartByte():methodNameNode.EndByte()])
			// Find the receiver parameter list (the first parameter_list in the method declaration)
			receiverNode := node.ChildByFieldName("receiver")
			if receiverNode != nil {
				//receiverTypeNode := receiverNode.ChildByFieldName("parameter_list")
				parameterDeclarationNode := receiverNode.NamedChild(0)
				if parameterDeclarationNode != nil {
					parameterTypeNode := parameterDeclarationNode.ChildByFieldName("type")
					rT := string(content[parameterTypeNode.StartByte():parameterTypeNode.EndByte()])
					//if receiver type starts with * remove that star
					if rT[0] == '*' {
						rT = rT[1:]
					}
					//fmt.Printf("Method: %s on %s\n", methodName, rT)
					methodIdentifier := utils.GetIdentifier(moduleName, packagePath, methodName, "")
					receiverIdentifier := utils.GetIdentifier(moduleName, packagePath, rT, "")
					graph.AddEdge(methodIdentifier, receiverIdentifier, "belongs to")
				}
			}

			if len(methodName) > 0 {
				//if receiverType != "" {
				//	fmt.Printf("Method: %s on %s\n", methodName, receiverType)
				//} else {
				methodIdentifier := utils.GetIdentifier(moduleName, packagePath, methodName, "")
				//Find statements within the method body and add edges
				bodyNode := node.ChildByFieldName("body")
				if bodyNode != nil {
					extractFunctionRelationships(bodyNode, content, graph, methodIdentifier, moduleName, packagePath, imports)
				}
			}
		}

		//fmt.Println("Processing node kind:", nodeKind)

		extractStructRelationships(&node, content, graph, moduleName, packagePath)

	}

	// Clean up
	tree.Close()

	return nil
}
