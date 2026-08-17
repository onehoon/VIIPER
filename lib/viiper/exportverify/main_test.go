package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestCanonicalExportsAndProjection(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	_ = os.Mkdir(source, 0755)
	write(t, filepath.Join(source, "a.go"), "package p\n//export Alpha\nfunc Alpha() {}\nfunc hidden() {}\n")
	write(t, filepath.Join(source, "b.go"), "package p\n//export Beta\nfunc Beta() {}\n")
	write(t, filepath.Join(source, "a_test.go"), "package p\n//export TestOnly\nfunc TestOnly() {}\n")
	exports, err := canonicalExports(source)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(exports, ",") != "Alpha,Beta" {
		t.Fatalf("exports = %v", exports)
	}
	header := filepath.Join(dir, "x.h")
	def := filepath.Join(dir, "x.def")
	dll := filepath.Join(dir, "dll.txt")
	for _, path := range []string{header, def, dll} {
		write(t, path, "Beta\nAlpha\n")
	}
	if err := verifyProjection(source, header, def, dll); err != nil {
		t.Fatal(err)
	}
	write(t, def, "Alpha\n")
	if err := verifyProjection(source, header, def, ""); err == nil || !strings.Contains(err.Error(), "Beta") {
		t.Fatal(err)
	}
}

func TestCanonicalExportsFailOnMalformedSource(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "bad.go"), "package p\nfunc (")
	if _, err := canonicalExports(dir); err == nil || !strings.Contains(err.Error(), "parse source") {
		t.Fatal(err)
	}
}

func TestCanonicalExportsRejectAmbiguousDirective(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "ambiguous.go"), "package p\n//export First\n//export Second\nfunc First() {}\n")
	if _, err := canonicalExports(dir); err == nil || !strings.Contains(err.Error(), "ambiguous export directive") {
		t.Fatal(err)
	}
}
