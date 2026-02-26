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
	startFunc := flag.String("start", "main", "The starting function to analyze")
	outFile := flag.String("out", "flow.md", "The file to write the Mermaid diagram to")
	flag.Parse()

	targetDir := "."
	if len(flag.Args()) > 0 {
		targetDir = flag.Args()[0]
	}

	mermaidCode, err := analyzeCFG(targetDir, *startFunc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// Dropped ELK renderer for strict Top-Down (TD) layout
	output := fmt.Sprintf("```mermaid\nflowchart TD;\n%s```\n", mermaidCode)

	if *outFile == "-" {
		fmt.Println(output)
	} else {
		err = os.WriteFile(*outFile, []byte(output), 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error writing file: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Successfully generated %s for function %s()\n", *outFile, *startFunc)
	}
}

func analyzeCFG(path string, startFunc string) (string, error) {
	config := &packages.Config{
		Mode: packages.NeedName | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedFiles | packages.NeedCompiledGoFiles,
		Dir:  path,
	}

	pkgs, err := packages.Load(config, "./...")
	if err != nil || packages.PrintErrors(pkgs) > 0 {
		return "", fmt.Errorf("failed to load packages")
	}

	var targetDecl *ast.FuncDecl
	var fset *token.FileSet

	for _, pkg := range pkgs {
		for _, file := range pkg.Syntax {
			ast.Inspect(file, func(n ast.Node) bool {
				if fn, ok := n.(*ast.FuncDecl); ok && fn.Name.Name == startFunc {
					targetDecl = fn
					fset = pkg.Fset
					return false
				}
				return true
			})
		}
	}

	if targetDecl == nil {
		return "", fmt.Errorf("function '%s' not found", startFunc)
	}

	flowGraph := cfg.New(targetDecl.Body, func(call *ast.CallExpr) bool { return false })

	// --- NEW: Pre-process the graph to filter out noise BEFORE layout ---
	for _, block := range flowGraph.Blocks {
		block.Nodes = filterNoise(block.Nodes)
	}

	var buf bytes.Buffer

	// Define visual styles for Start and End nodes
	buf.WriteString("    classDef root fill:#007acc,stroke:#fff,stroke-width:2px,color:#fff;\n")
	buf.WriteString("    classDef endNode fill:#cc3300,stroke:#fff,stroke-width:2px,color:#fff;\n\n")

	// Force ROOT to top
	buf.WriteString(fmt.Sprintf("    ROOT([\"func %s\"]):::root\n", startFunc))
	firstBlock := resolveDestination(flowGraph.Blocks[0])
	buf.WriteString(fmt.Sprintf("    ROOT --> %s;\n", getEntryPoint(firstBlock)))

	for _, block := range flowGraph.Blocks {
		if isEmptyPassThrough(block) {
			continue
		}

		isSplit := len(block.Nodes) > 1 && len(block.Succs) == 2

		if isSplit {
			setupLabel := formatNodes(fset, block.Nodes[:len(block.Nodes)-1])
			condLabel := formatNodes(fset, block.Nodes[len(block.Nodes)-1:])

			buf.WriteString(fmt.Sprintf("    B%d_setup[\"%s\"];\n", block.Index, setupLabel))
			buf.WriteString(fmt.Sprintf("    B%d{\"%s\"};\n", block.Index, condLabel))
			buf.WriteString(fmt.Sprintf("    B%d_setup --> B%d;\n", block.Index, block.Index))
		} else {
			label := formatNodes(fset, block.Nodes)
			if len(block.Nodes) == 0 {
				label = getStructuralLabel(block)
			}
			shapeStart, shapeEnd := "[\"", "\"]"
			if len(block.Succs) == 2 {
				shapeStart, shapeEnd = "{\"", "\"}"
			}

			classStr := ""
			if len(block.Succs) == 0 { // If it's a final block, color it red
				classStr = ":::endNode"
			}

			buf.WriteString(fmt.Sprintf("    B%d%s%s%s%s;\n", block.Index, shapeStart, label, shapeEnd, classStr))
		}

		exitPoint := fmt.Sprintf("B%d", block.Index)

		if len(block.Succs) == 1 {
			dest := resolveDestination(block.Succs[0])
			arrow := "-->"
			if dest.Index <= block.Index {
				arrow = "-.->|Loop|"
			}
			buf.WriteString(fmt.Sprintf("    %s %s %s;\n", exitPoint, arrow, getEntryPoint(dest)))
		} else if len(block.Succs) == 2 {
			destTrue := resolveDestination(block.Succs[0])
			destFalse := resolveDestination(block.Succs[1])

			arrowTrue := "-->|True|"
			if destTrue.Index <= block.Index {
				arrowTrue = "-.->|Loop True|"
			}
			buf.WriteString(fmt.Sprintf("    %s %s %s;\n", exitPoint, arrowTrue, getEntryPoint(destTrue)))

			arrowFalse := "-->|False|"
			if destFalse.Index <= block.Index {
				arrowFalse = "-.->|Loop False|"
			}
			buf.WriteString(fmt.Sprintf("    %s %s %s;\n", exitPoint, arrowFalse, getEntryPoint(destFalse)))
		}
	}

	return buf.String(), nil
}

// --- NEW: filterNoise removes tracing, metrics, and logs ---
func filterNoise(nodes []ast.Node) []ast.Node {
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
					name := ident.Name
					// Ignore calls to these packages/variables
					if name == "metrics" || name == "span" || name == "tracing" || name == "log" || name == "logger" {
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

func getEntryPoint(b *cfg.Block) string {
	if len(b.Nodes) > 1 && len(b.Succs) == 2 {
		return fmt.Sprintf("B%d_setup", b.Index)
	}
	return fmt.Sprintf("B%d", b.Index)
}

func isEmptyPassThrough(b *cfg.Block) bool {
	return len(b.Nodes) == 0 && len(b.Succs) == 1
}

func resolveDestination(b *cfg.Block) *cfg.Block {
	curr := b
	visited := make(map[int32]bool)
	for isEmptyPassThrough(curr) {
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
		return "Loop / Switch Entry"
	}
	return "Merge Point"
}

func formatNodes(fset *token.FileSet, nodes []ast.Node) string {
	var lines []string
	for _, n := range nodes {
		s := toNaturalLanguage(fset, n)
		s = strings.ReplaceAll(s, "\n", " ")
		s = strings.ReplaceAll(s, "\t", "")
		s = strings.ReplaceAll(s, "\"", "'")
		s = strings.ReplaceAll(s, "{", "")
		s = strings.ReplaceAll(s, "}", "")

		// Bumped limit to 80 characters so returns are not truncated
		if len(s) > 80 {
			s = s[:77] + "..."
		}
		lines = append(lines, strings.TrimSpace(s))
	}
	return strings.Join(lines, "<br>")
}

func toNaturalLanguage(fset *token.FileSet, n ast.Node) string {
	switch x := n.(type) {
	case *ast.UnaryExpr:
		if x.Op == token.NOT {
			return fmt.Sprintf("%s is false", printRawNode(fset, x.X))
		}
	case *ast.BinaryExpr:
		left := printRawNode(fset, x.X)
		right := printRawNode(fset, x.Y)
		switch x.Op {
		case token.EQL:
			return fmt.Sprintf("%s equals %s", left, right)
		case token.NEQ:
			return fmt.Sprintf("%s does not equal %s", left, right)
		case token.LSS:
			return fmt.Sprintf("%s is less than %s", left, right)
		case token.GTR:
			return fmt.Sprintf("%s is greater than %s", left, right)
		case token.LEQ:
			return fmt.Sprintf("%s is at most %s", left, right)
		case token.GEQ:
			return fmt.Sprintf("%s is at least %s", left, right)
		case token.LAND:
			return fmt.Sprintf("%s AND %s", left, right)
		case token.LOR:
			return fmt.Sprintf("%s OR %s", left, right)
		}
	case *ast.AssignStmt:
		if len(x.Lhs) == 1 && len(x.Rhs) == 1 {
			left := printRawNode(fset, x.Lhs[0])
			right := printRawNode(fset, x.Rhs[0])
			return fmt.Sprintf("Set %s to %s", left, right)
		}
	case *ast.IncDecStmt:
		val := printRawNode(fset, x.X)
		if x.Tok == token.INC {
			return fmt.Sprintf("Increase %s by 1", val)
		} else if x.Tok == token.DEC {
			return fmt.Sprintf("Decrease %s by 1", val)
		}
	case *ast.ReturnStmt:
		if len(x.Results) > 0 {
			var res []string
			for _, r := range x.Results {
				res = append(res, printRawNode(fset, r))
			}
			return "Return " + strings.Join(res, ", ")
		}
		return "Return"
	}
	return printRawNode(fset, n)
}

func printRawNode(fset *token.FileSet, n ast.Node) string {
	var b bytes.Buffer
	printer.Fprint(&b, fset, n)
	return b.String()
}
