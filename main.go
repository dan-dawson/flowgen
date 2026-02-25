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

	output := fmt.Sprintf("```mermaid\n%s```\n", mermaidCode)

	if *outFile == "-" {
		fmt.Println(output)
	} else {
		os.WriteFile(*outFile, []byte(output), 0644)
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

	// 1. Hunt for the specific function declaration
	for _, pkg := range pkgs {
		for _, file := range pkg.Syntax {
			ast.Inspect(file, func(n ast.Node) bool {
				if fn, ok := n.(*ast.FuncDecl); ok && fn.Name.Name == startFunc {
					targetDecl = fn
					fset = pkg.Fset
					return false // Found it, stop searching
				}
				return true
			})
		}
	}

	if targetDecl == nil {
		return "", fmt.Errorf("function '%s' not found", startFunc)
	}

	// 2. Build the Control Flow Graph
	// The second argument is a function that determines if a call might panic/exit. We return false for simplicity.
	flowGraph := cfg.New(targetDecl.Body, func(call *ast.CallExpr) bool { return false })

	// Helper function to turn AST code blocks back into readable strings for the diagram
	formatNode := func(n ast.Node) string {
		var b bytes.Buffer
		printer.Fprint(&b, fset, n)
		s := b.String()
		s = strings.ReplaceAll(s, "\n", " ") // Mermaid doesn't like raw newlines in nodes
		s = strings.ReplaceAll(s, "\"", "'") // Swap quotes so Mermaid doesn't break
		if len(s) > 40 {
			s = s[:37] + "..." // Truncate long lines
		}
		return s
	}

	// 3. Translate CFG Blocks to Mermaid Code
	var buf bytes.Buffer
	buf.WriteString("graph TD;\n")

	for _, block := range flowGraph.Blocks {
		label := "Empty Block"

		// Determine the text to show inside the shape
		if len(block.Nodes) > 0 {
			// The last node in a block is usually the condition or the final action
			label = formatNode(block.Nodes[len(block.Nodes)-1])
		} else if block.Index == 0 {
			label = "Start"
		} else if len(block.Succs) == 0 {
			label = "Return / End"
		}

		// If a block has 2 successors, it's a decision block (if/else), so we use a diamond {}
		shapeStart, shapeEnd := "[", "]"
		if len(block.Succs) == 2 {
			shapeStart, shapeEnd = "{", "}"
		}

		// Print the node
		buf.WriteString(fmt.Sprintf("    B%d%s\"%s\"%s;\n", block.Index, shapeStart, label, shapeEnd))

		// Print the connecting arrows
		if len(block.Succs) == 1 {
			buf.WriteString(fmt.Sprintf("    B%d --> B%d;\n", block.Index, block.Succs[0].Index))
		} else if len(block.Succs) == 2 {
			buf.WriteString(fmt.Sprintf("    B%d -->|True| B%d;\n", block.Index, block.Succs[0].Index))
			buf.WriteString(fmt.Sprintf("    B%d -->|False| B%d;\n", block.Index, block.Succs[1].Index))
		}
	}

	return buf.String(), nil
}
