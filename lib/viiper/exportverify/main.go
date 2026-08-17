package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	headerExportPattern = regexp.MustCompile(`(?m)^\s*extern\s+[^;\r\n]*\b([A-Za-z_]\w*)\s*\([^;\r\n]*\)\s*;`)
	defExportPattern    = regexp.MustCompile(`(?m)^\s*([A-Za-z_]\w*)\s*$`)
	dllExportPattern    = regexp.MustCompile(`^\s*\[\s*\d+\].*?\s([A-Za-z_]\w*)\s*$`)
)

func canonicalExports(sourceDir string) ([]string, error) {
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		return nil, fmt.Errorf("read source directory %q: %w", sourceDir, err)
	}
	set := map[string]struct{}{}
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
				if export != fn.Name.Name {
					return nil, fmt.Errorf("export directive %q does not match function %q in %s", export, fn.Name.Name, path)
				}
				if _, exists := set[export]; exists {
					return nil, fmt.Errorf("duplicate canonical export %q in %s", export, path)
				}
				set[export] = struct{}{}
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

func readArtifact(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read artifact %q: %w", path, err)
	}
	return string(data), nil
}

func exactHeaderExports(text string) map[string]struct{} {
	result := map[string]struct{}{}
	for _, match := range headerExportPattern.FindAllStringSubmatch(text, -1) {
		result[match[1]] = struct{}{}
	}
	return result
}

func exactDefExports(text string) map[string]struct{} {
	result := map[string]struct{}{}
	inExports := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.EqualFold(trimmed, "EXPORTS") {
			inExports = true
			continue
		}
		if inExports && !strings.HasPrefix(trimmed, ";") {
			if match := defExportPattern.FindStringSubmatch(line); match != nil {
				result[match[1]] = struct{}{}
			}
		}
	}
	return result
}

func exactDLLExports(text string) map[string]struct{} {
	result := map[string]struct{}{}
	inNameTable := false
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "Ordinal/Name Pointer") {
			inNameTable = true
			continue
		}
		if inNameTable && strings.HasPrefix(strings.TrimSpace(line), "The ") {
			break
		}
		if inNameTable {
			if match := dllExportPattern.FindStringSubmatch(line); match != nil {
				result[match[1]] = struct{}{}
			}
		}
	}
	return result
}

func missingExports(available map[string]struct{}, exports []string) []string {
	missing := make([]string, 0)
	for _, name := range exports {
		if _, ok := available[name]; !ok {
			missing = append(missing, name)
		}
	}
	return missing
}

func verifyProjection(sourceDir, header, def, dllOutput string) error {
	exports, err := canonicalExports(sourceDir)
	if err != nil {
		return err
	}
	headerText, err := readArtifact(header)
	if err != nil {
		return err
	}
	if missing := missingExports(exactHeaderExports(headerText), exports); len(missing) > 0 {
		return fmt.Errorf("artifact %q is missing canonical exports: %s", header, strings.Join(missing, ", "))
	}
	if def != "" {
		defText, err := readArtifact(def)
		if err != nil {
			return err
		}
		if missing := missingExports(exactDefExports(defText), exports); len(missing) > 0 {
			return fmt.Errorf("artifact %q is missing canonical exports: %s", def, strings.Join(missing, ", "))
		}
	}
	if dllOutput != "" {
		dllText, err := readArtifact(dllOutput)
		if err != nil {
			return err
		}
		if missing := missingExports(exactDLLExports(dllText), exports); len(missing) > 0 {
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
