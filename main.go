package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/printer"
	"go/token"
	"os"
	"strings"

	"golang.org/x/tools/go/cfg"
	"golang.org/x/tools/go/packages"
)

func main() {
	startFunc := flag.String("start", "main", "The starting function to analyze (e.g., 'main' or 'manager.CreateBasket')")
	outFile := flag.String("out", "flow.md", "The file to write the Mermaid diagram to")

	// --- NEW: Dynamic Exclusion Flag ---
	excludeFlag := flag.String("exclude", "metrics,span,tracing,log,logger", "Comma-separated list of packages/variables to exclude")
	flag.Parse()

	targetDir := "."
	if len(flag.Args()) > 0 {
		targetDir = flag.Args()[0]
	}

	mermaidCode, err := analyzeCFG(targetDir, *startFunc, *excludeFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	output := fmt.Sprintf("```mermaid\nflowchart TD;\n%s```\n", mermaidCode)

	if *outFile == "-" {
		fmt.Println(output)
	} else {
		err = os.WriteFile(*outFile, []byte(output), 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error writing file: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Successfully generated %s for %s()\n", *outFile, *startFunc)
	}
}

// ==========================================
// DEV MODE (Low-Level CFG)
// ==========================================

func analyzeCFG(path string, startFunc string, excludeStr string) (string, error) {
	config := &packages.Config{
		Mode: packages.NeedName | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedFiles | packages.NeedCompiledGoFiles,
		Dir:  path,
	}

	pkgs, err := packages.Load(config, "./...")
	if err != nil || packages.PrintErrors(pkgs) > 0 {
		return "", fmt.Errorf("failed to load packages")
	}

	targetDecl, fset, err := findStartingFunction(pkgs, startFunc)
	if err != nil {
		return "", err
	}

	flowGraph := cfg.New(targetDecl.Body, func(call *ast.CallExpr) bool { return true })

	// --- NEW: Parse the exclusion list into a fast lookup map ---
	excludeMap := make(map[string]bool)
	for _, item := range strings.Split(excludeStr, ",") {
		trimmed := strings.TrimSpace(item)
		if trimmed != "" {
			excludeMap[trimmed] = true
		}
	}

	for _, block := range flowGraph.Blocks {
		block.Nodes = filterNoise(block.Nodes, excludeMap)
	}

	// Build predecessor map to detect merge points
	preds := make(map[int32][]int32)
	for _, b := range flowGraph.Blocks {
		for _, succ := range b.Succs {
			preds[succ.Index] = append(preds[succ.Index], b.Index)
		}
	}

	loopHeaders := make(map[int32]bool)
	for _, b := range flowGraph.Blocks {
		if isEmptyPassThrough(b, preds) {
			continue
		}
		for _, succ := range b.Succs {
			dest := resolveDestination(succ, preds)
			if dest.Index <= b.Index {
				loopHeaders[dest.Index] = true
			}
		}
	}

	var buf bytes.Buffer
	buf.WriteString("    classDef root fill:#007acc,stroke:#fff,stroke-width:2px,color:#fff;\n")
	buf.WriteString("    classDef successNode fill:#2ea043,stroke:#fff,stroke-width:2px,color:#fff;\n")
	buf.WriteString("    classDef errorNode fill:#cc3300,stroke:#fff,stroke-width:2px,color:#fff;\n")
	buf.WriteString("    classDef mergeNode fill:#555,stroke:#fff,stroke-width:2px,color:#fff;\n\n")

	buf.WriteString(fmt.Sprintf("    ROOT([\"func %s\"]):::root\n", startFunc))
	firstBlock := resolveDestination(flowGraph.Blocks[0], preds)
	buf.WriteString(fmt.Sprintf("    ROOT --> %s;\n", getEntryPoint(firstBlock)))

	for _, block := range flowGraph.Blocks {
		if isEmptyPassThrough(block, preds) {
			continue
		}

		isCond := len(block.Succs) == 2
		isSplit := len(block.Nodes) > 1 && isCond
		label := formatNodes(fset, block.Nodes, isCond)

		if isSplit {
			setupLabel := formatNodes(fset, block.Nodes[:len(block.Nodes)-1], false)
			condLabel := formatNodes(fset, block.Nodes[len(block.Nodes)-1:], true)

			buf.WriteString(fmt.Sprintf("    B%d_setup[\"%s\"];\n", block.Index, setupLabel))
			buf.WriteString(fmt.Sprintf("    B%d{\"%s\"};\n", block.Index, condLabel))
			buf.WriteString(fmt.Sprintf("    B%d_setup --> B%d;\n", block.Index, block.Index))
		} else {
			isMerge := false

			if len(block.Nodes) == 0 {
				if loopHeaders[block.Index] {
					label = "Evaluate Loop Condition"
				} else if len(preds[block.Index]) > 1 && len(block.Succs) == 1 {
					label = "Merge"
					isMerge = true
				} else {
					label = getStructuralLabel(block)
				}
			}

			shapeStart, shapeEnd := "[\"", "\"]"
			if isCond {
				if strings.Contains(label, "Type Switch:") {
					shapeStart, shapeEnd = "[\"", "\"]"
				} else {
					shapeStart, shapeEnd = "{\"", "\"}"
				}
			} else if isMerge {
				shapeStart, shapeEnd = "((", "))"
			}

			classStr := ""
			if len(block.Succs) == 0 {
				if isErrorReturn(block.Nodes, fset) {
					classStr = ":::errorNode"
				} else {
					classStr = ":::successNode"
				}
			} else if isMerge {
				classStr = ":::mergeNode"
			}

			buf.WriteString(fmt.Sprintf("    B%d%s%s%s%s;\n", block.Index, shapeStart, label, shapeEnd, classStr))
		}

		exitPoint := fmt.Sprintf("B%d", block.Index)

		if len(block.Succs) == 1 {
			dest := resolveDestination(block.Succs[0], preds)
			arrow := "-.->"
			if dest.Index > block.Index {
				arrow = "-->"
			}
			buf.WriteString(fmt.Sprintf("    %s %s %s;\n", exitPoint, arrow, getEntryPoint(dest)))

		} else if len(block.Succs) == 2 {
			destTrue := resolveDestination(block.Succs[0], preds)
			destFalse := resolveDestination(block.Succs[1], preds)

			isTypeSwitch := strings.Contains(label, "Type Switch:")
			isCase := strings.HasPrefix(label, "Case:")

			arrowTrue := "-->|True|"
			arrowFalse := "-->|False|"

			if isTypeSwitch {
				arrowTrue = "-->|Match First Case|"
				arrowFalse = "-->|Next|"
			} else if isCase {
				arrowTrue = "-->|Match|"
				arrowFalse = "-->|Next|"
			}

			if loopHeaders[block.Index] {
				if isTypeSwitch || isCase {
					arrowFalse = "---->|Next|"
				} else {
					arrowFalse = "---->|False|"
				}
			}

			if destTrue.Index <= block.Index {
				arrowTrue = strings.Replace(arrowTrue, "-->", "-.->", 1)
				arrowTrue = strings.Replace(arrowTrue, "---->", "-.->", 1)
			}
			if destFalse.Index <= block.Index {
				arrowFalse = strings.Replace(arrowFalse, "-->", "-.->", 1)
				arrowFalse = strings.Replace(arrowFalse, "---->", "-.->", 1)
			}

			buf.WriteString(fmt.Sprintf("    %s %s %s;\n", exitPoint, arrowTrue, getEntryPoint(destTrue)))
			buf.WriteString(fmt.Sprintf("    %s %s %s;\n", exitPoint, arrowFalse, getEntryPoint(destFalse)))
		}
	}

	return buf.String(), nil
}

// ==========================================
// UTILITIES
// ==========================================

func findStartingFunction(pkgs []*packages.Package, startParam string) (*ast.FuncDecl, *token.FileSet, error) {
	var targetRecv, targetName string
	parts := strings.Split(startParam, ".")
	if len(parts) == 2 {
		targetRecv = strings.TrimPrefix(parts[0], "*")
		targetName = parts[1]
	} else {
		targetName = startParam
	}

	for _, pkg := range pkgs {
		for _, file := range pkg.Syntax {
			filename := strings.ToLower(pkg.Fset.Position(file.Pos()).Filename)
			if strings.Contains(filename, "mock") {
				continue
			}

			var found *ast.FuncDecl
			ast.Inspect(file, func(n ast.Node) bool {
				if fn, ok := n.(*ast.FuncDecl); ok && fn.Name.Name == targetName {
					if targetRecv != "" {
						if fn.Recv == nil || len(fn.Recv.List) == 0 {
							return true
						}
						var recvBuf bytes.Buffer
						printer.Fprint(&recvBuf, pkg.Fset, fn.Recv.List[0].Type)
						recvStr := strings.TrimPrefix(recvBuf.String(), "*")
						if recvStr != targetRecv {
							return true
						}
					}
					found = fn
					return false
				}
				return true
			})

			if found != nil {
				return found, pkg.Fset, nil
			}
		}
	}
	return nil, nil, fmt.Errorf("function '%s' not found (ignored auto-generated mocks)", startParam)
}

func formatNodes(fset *token.FileSet, nodes []ast.Node, isCond bool) string {
	var lines []string
	for _, n := range nodes {
		s := toNaturalLanguage(fset, n, isCond)
		s = strings.ReplaceAll(s, "\n", " ")
		s = strings.ReplaceAll(s, "\t", "")
		s = strings.ReplaceAll(s, "\"", "'")
		s = strings.ReplaceAll(s, "{", "")
		s = strings.ReplaceAll(s, "}", "")

		if len(s) > 120 {
			s = s[:117] + "..."
		}

		s = wrapText(s, 35)

		lines = append(lines, strings.TrimSpace(s))
	}
	return strings.Join(lines, "<br><br>")
}

func wrapText(text string, limit int) string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return ""
	}

	var result string
	lineLen := 0

	for i, word := range words {
		if i > 0 {
			if lineLen+len(word) > limit {
				result += "<br>"
				lineLen = 0
			} else {
				result += " "
				lineLen++
			}
		}
		result += word
		lineLen += len(word)
	}
	return result
}

func toNaturalLanguage(fset *token.FileSet, n ast.Node, isCond bool) string {
	var result string

	switch x := n.(type) {
	case *ast.UnaryExpr:
		if x.Op == token.NOT {
			result = fmt.Sprintf("%s is false", printRawNode(fset, x.X))
		}
	case *ast.BinaryExpr:
		left := printRawNode(fset, x.X)
		right := printRawNode(fset, x.Y)
		switch x.Op {
		case token.EQL:
			result = fmt.Sprintf("%s equals %s", left, right)
		case token.NEQ:
			result = fmt.Sprintf("%s does not equal %s", left, right)
		case token.LSS:
			result = fmt.Sprintf("%s is less than %s", left, right)
		case token.GTR:
			result = fmt.Sprintf("%s is greater than %s", left, right)
		case token.LEQ:
			result = fmt.Sprintf("%s is at most %s", left, right)
		case token.GEQ:
			result = fmt.Sprintf("%s is at least %s", left, right)
		case token.LAND:
			result = fmt.Sprintf("%s AND %s", left, right)
		case token.LOR:
			result = fmt.Sprintf("%s OR %s", left, right)
		}
	case *ast.AssignStmt:
		if len(x.Lhs) == 1 && len(x.Rhs) == 1 {
			left := printRawNode(fset, x.Lhs[0])

			if ta, ok := x.Rhs[0].(*ast.TypeAssertExpr); ok && ta.Type == nil {
				expr := printRawNode(fset, ta.X)
				result = fmt.Sprintf("Type Switch: %s = %s", left, expr)
			} else {
				right := printRawNode(fset, x.Rhs[0])
				result = fmt.Sprintf("Set %s to %s", left, right)
			}
		}
	case *ast.StarExpr:
		if isCond {
			result = fmt.Sprintf("Case: %s", printRawNode(fset, x))
		}
	case *ast.IncDecStmt:
		val := printRawNode(fset, x.X)
		if x.Tok == token.INC {
			result = fmt.Sprintf("Increase %s by 1", val)
		} else if x.Tok == token.DEC {
			result = fmt.Sprintf("Decrease %s by 1", val)
		}
	case *ast.ReturnStmt:
		if len(x.Results) > 0 {
			var res []string
			for _, r := range x.Results {
				res = append(res, printRawNode(fset, r))
			}
			result = "Return " + strings.Join(res, ", ")
		} else {
			result = "Return"
		}
	case *ast.SelectorExpr, *ast.Ident:
		if isCond {
			name := printRawNode(fset, x)
			if strings.Contains(name, ".") {
				result = fmt.Sprintf("Case: %s", name)
			} else {
				result = fmt.Sprintf("Is %s?", name)
			}
		}
	}

	if result == "" {
		result = printRawNode(fset, n)
	}

	if isCond && !strings.HasPrefix(result, "Case:") && !strings.HasPrefix(result, "Type Switch:") && !strings.HasSuffix(result, "?") {
		result += "?"
	}
	return result
}

func printRawNode(fset *token.FileSet, n ast.Node) string {
	var b bytes.Buffer
	printer.Fprint(&b, fset, n)
	return b.String()
}

// --- NEW: filterNoise now uses the dynamic exclude map ---
func filterNoise(nodes []ast.Node, excludeMap map[string]bool) []ast.Node {
	var keep []ast.Node
	for _, n := range nodes {
		var expr ast.Expr

		switch x := n.(type) {
		case *ast.ExprStmt:
			expr = x.X
		case *ast.AssignStmt:
			if len(x.Rhs) == 1 {
				expr = x.Rhs[0]
			}
		case *ast.DeferStmt:
			expr = x.Call
		}

		isNoise := false
		if call, ok := expr.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if ident, ok := sel.X.(*ast.Ident); ok {
					if excludeMap[ident.Name] {
						isNoise = true
					}
				}
			}
		}

		if !isNoise {
			keep = append(keep, n)
		}
	}
	return keep
}

func isErrorReturn(nodes []ast.Node, fset *token.FileSet) bool {
	for _, n := range nodes {
		if ret, ok := n.(*ast.ReturnStmt); ok {
			for _, res := range ret.Results {
				text := printRawNode(fset, res)
				if text == "err" || strings.HasPrefix(text, "Err") || strings.Contains(text, ".Err()") || strings.Contains(text, "Error") {
					return true
				}
			}
		}
	}
	return false
}

func getEntryPoint(b *cfg.Block) string {
	if len(b.Nodes) > 1 && len(b.Succs) == 2 {
		return fmt.Sprintf("B%d_setup", b.Index)
	}
	return fmt.Sprintf("B%d", b.Index)
}

func isEmptyPassThrough(b *cfg.Block, preds map[int32][]int32) bool {
	if len(b.Nodes) == 0 && len(b.Succs) == 1 {
		if len(preds[b.Index]) > 1 {
			return false
		}
		return true
	}
	return false
}

func resolveDestination(b *cfg.Block, preds map[int32][]int32) *cfg.Block {
	curr := b
	visited := make(map[int32]bool)
	for isEmptyPassThrough(curr, preds) {
		if visited[curr.Index] {
			break
		}
		visited[curr.Index] = true
		curr = curr.Succs[0]
	}
	return curr
}

func getStructuralLabel(block *cfg.Block) string {
	if block.Index == 0 {
		return "Start"
	}
	if len(block.Succs) == 0 {
		return "End / Return"
	}
	if len(block.Succs) == 2 {
		return "Decision / Branch"
	}
	return "Merge Point"
}
