package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func canonicalExports(sourceDir string) ([]string, error) {
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		return nil, fmt.Errorf("read source directory %q: %w", sourceDir, err)
	}
	set := map[string]bool{}
	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(sourceDir, entry.Name())
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("parse source %q: %w", path, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Doc == nil {
				continue
			}
			var export string
			for _, comment := range fn.Doc.List {
				if strings.HasPrefix(comment.Text, "//export ") {
					name := strings.TrimSpace(strings.TrimPrefix(comment.Text, "//export "))
					if name == "" || strings.ContainsAny(name, " \t") || export != "" {
						return nil, fmt.Errorf("ambiguous export directive near %s", path)
					}
					export = name
				}
			}
			if export != "" {
				set[export] = true
			}
		}
	}
	result := make([]string, 0, len(set))
	for name := range set {
		result = append(result, name)
	}
	sort.Strings(result)
	return result, nil
}

func artifactContains(path string, exports []string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read artifact %q: %w", path, err)
	}
	text := string(data)
	missing := make([]string, 0)
	for _, name := range exports {
		if !strings.Contains(text, name) {
			missing = append(missing, name)
		}
	}
	return missing, nil
}

func verifyProjection(sourceDir, header, def, dllOutput string) error {
	exports, err := canonicalExports(sourceDir)
	if err != nil {
		return err
	}
	artifacts := []string{header}
	if def != "" {
		artifacts = append(artifacts, def)
	}
	for _, artifact := range artifacts {
		missing, err := artifactContains(artifact, exports)
		if err != nil {
			return err
		}
		if len(missing) > 0 {
			return fmt.Errorf("artifact %q is missing canonical exports: %s", artifact, strings.Join(missing, ", "))
		}
	}
	if dllOutput != "" {
		missing, err := artifactContains(dllOutput, exports)
		if err != nil {
			return err
		}
		if len(missing) > 0 {
			return fmt.Errorf("DLL export table %q is missing canonical exports: %s", dllOutput, strings.Join(missing, ", "))
		}
	}
	fmt.Printf("canonical source exports: %d\n", len(exports))
	return nil
}

func main() {
	source := flag.String("source", "lib/viiper", "canonical Go source directory")
	header := flag.String("header", "dist/libVIIPER/libVIIPER.h", "generated header")
	def := flag.String("def", "", "Windows DEF file")
	dllOutput := flag.String("dll-output", "", "text export table produced by llvm-objdump/objdump")
	flag.Parse()
	if err := verifyProjection(*source, *header, *def, *dllOutput); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
