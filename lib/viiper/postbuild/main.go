package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

type fileSystem struct {
	readDir   func(string) ([]os.DirEntry, error)
	readFile  func(string) ([]byte, error)
	writeFile func(string, []byte, os.FileMode) error
}

var defaultFileSystem = fileSystem{os.ReadDir, os.ReadFile, os.WriteFile}

func formatDocCommentLine(line string) string {
	if line == "" {
		return " *"
	}
	return " * " + line
}

func collectDocs(sourceDir string, fs fileSystem) (map[string]string, error) {
	entries, err := fs.readDir(sourceDir)
	if err != nil {
		return nil, fmt.Errorf("read source directory %q: %w", sourceDir, err)
	}
	fset := token.NewFileSet()
	comments := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(sourceDir, e.Name())
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("parse source %q: %w", path, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Doc == nil {
				continue
			}
			var name string
			var lines []string
			for _, c := range fn.Doc.List {
				if n, ok := strings.CutPrefix(c.Text, "//export "); ok {
					name = n
				} else {
					line, ok := strings.CutPrefix(c.Text, "// ")
					if !ok {
						line, _ = strings.CutPrefix(c.Text, "//")
					}
					lines = append(lines, line)
				}
			}
			if name != "" && len(lines) > 0 {
				comments[name] = strings.Join(lines, "\n")
			}
		}
	}
	return comments, nil
}

func run(sourceDir, headerPath string, fs fileSystem) error {
	comments, err := collectDocs(sourceDir, fs)
	if err != nil {
		return err
	}

	data, err := fs.readFile(headerPath)
	if err != nil {
		return fmt.Errorf("read generated header %q: %w", headerPath, err)
	}
	var out []string
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "extern ") {
			for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "*/" {
				end := len(out) - 1
				for end >= 0 && strings.TrimSpace(out[end]) != "/*" {
					end--
				}
				if end < 0 {
					break
				}
				out = out[:end]
			}
			for _, p := range strings.Fields(line)[1:] {
				if before, _, ok := strings.Cut(p, "("); ok {
					if doc, ok := comments[before]; ok {
						out = append(out, "/*")
						for _, dl := range strings.Split(doc, "\n") {
							out = append(out, formatDocCommentLine(dl))
						}
						out = append(out, " */")
					}
					break
				}
			}
		}
		out = append(out, line)
	}
	if err := fs.writeFile(headerPath, []byte(strings.Join(out, "\n")), 0644); err != nil {
		return fmt.Errorf("write generated header %q: %w", headerPath, err)
	}
	return nil
}

func main() {
	if err := run("lib/viiper", "dist/libVIIPER/libVIIPER.h", defaultFileSystem); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
