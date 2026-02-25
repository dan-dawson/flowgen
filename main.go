package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"

	"golang.org/x/tools/go/packages"
)

func main() {
	// 1. Define command-line flags
	startFunc := flag.String("start", "main", "The starting function for the flow diagram")
	outFile := flag.String("out", "flow.md", "The file to write the Mermaid diagram to")

	// Parse the flags provided by the user (or by go generate)
	flag.Parse()

	// The remaining arguments are the target packages/directories
	// If none provided, default to the current directory
	targetDir := "."
	if len(flag.Args()) > 0 {
		targetDir = flag.Args()[0]
	}

	// 2. Run the analysis
	mermaidCode, err := analyzeRepo(targetDir, *startFunc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error analyzing repo: %v\n", err)
		os.Exit(1)
	}

	// 3. Write the output
	output := fmt.Sprintf("```mermaid\n%s\n```\n", mermaidCode)

	if *outFile == "-" {
		// Print to terminal if output is set to "-"
		fmt.Println(output)
	} else {
		// Write to the specified file
		err = os.WriteFile(*outFile, []byte(output), 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error writing file: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Successfully generated %s starting from %s()\n", *outFile, *startFunc)
	}
}

// analyzeRepo remains mostly the same as the previous step
func analyzeRepo(path string, startFunc string) (string, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles | packages.NeedImports | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedSyntax,
		Dir:  path,
	}

	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return "", fmt.Errorf("failed to load packages: %v", err)
	}
	if packages.PrintErrors(pkgs) > 0 {
		return "", fmt.Errorf("package contains errors")
	}

	// Stubbed graph generation for the POC
	var buf bytes.Buffer
	buf.WriteString("graph TD;\n")
	buf.WriteString(fmt.Sprintf("    %s --> ValidateInput;\n", startFunc))
	buf.WriteString(fmt.Sprintf("    ValidateInput --> ConnectDatabase;\n"))

	return buf.String(), nil
}
