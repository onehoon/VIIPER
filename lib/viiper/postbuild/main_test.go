package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFormatDocCommentLine(t *testing.T) {
	if got := formatDocCommentLine(""); got != " *" {
		t.Fatalf("empty line = %q", got)
	}
	if got := formatDocCommentLine("Example text"); got != " * Example text" {
		t.Fatalf("text line = %q", got)
	}
}

func TestRunInsertsDocsDeterministically(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	_ = os.Mkdir(source, 0755)
	if err := os.WriteFile(filepath.Join(source, "a.go"), []byte("package p\n// New docs\n//export NewThing\nfunc NewThing() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "b.go"), []byte("package p\n//export Other\nfunc Other() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	header := filepath.Join(dir, "x.h")
	if err := os.WriteFile(header, []byte("extern void NewThing();\nextern void Other();\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := run(source, header, defaultFileSystem); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(header)
	got := string(data)
	if !strings.Contains(got, "/*\n * New docs\n */\nextern void NewThing();") {
		t.Fatalf("documentation was not inserted: %s", got)
	}
	if err := run(source, header, defaultFileSystem); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(header)
	if string(again) != got {
		t.Fatal("postbuild output is not deterministic")
	}
}

func TestRunFailsClosed(t *testing.T) {
	fs := fileSystem{readDir: func(string) ([]os.DirEntry, error) { return nil, errors.New("directory unavailable") }, readFile: os.ReadFile, writeFile: os.WriteFile}
	if err := run("missing", "header", fs); err == nil || !strings.Contains(err.Error(), "read source directory") {
		t.Fatal(err)
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	_ = os.Mkdir(source, 0755)
	if err := os.WriteFile(filepath.Join(source, "bad.go"), []byte("package p\nfunc ("), 0644); err != nil {
		t.Fatal(err)
	}
	if err := run(source, filepath.Join(dir, "header"), defaultFileSystem); err == nil || !strings.Contains(err.Error(), "parse source") {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(source, "bad.go"))
	if err := run(source, filepath.Join(dir, "missing.h"), defaultFileSystem); err == nil || !strings.Contains(err.Error(), "read generated header") {
		t.Fatal(err)
	}
	writeFail := defaultFileSystem
	writeFail.writeFile = func(string, []byte, os.FileMode) error { return errors.New("write denied") }
	good := filepath.Join(source, "good.go")
	_ = os.WriteFile(good, []byte("package p\n//export Good\nfunc Good() {}\n"), 0644)
	header := filepath.Join(dir, "good.h")
	_ = os.WriteFile(header, []byte("extern void Good();\n"), 0644)
	if err := run(source, header, writeFail); err == nil || !strings.Contains(err.Error(), "write generated header") {
		t.Fatal(err)
	}
}
