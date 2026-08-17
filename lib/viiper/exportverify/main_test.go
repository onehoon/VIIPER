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
	write(t, header, "extern void Beta();\nextern void Alpha();\n")
	write(t, def, "EXPORTS\nBeta\nAlpha\n")
	write(t, dll, "Ordinal/Name Pointer] Table\n [ 0] +base[ 1] 0000 Alpha\n [ 1] +base[ 2] 0001 Beta\n")
	if err := verifyProjection(source, header, def, dll); err != nil {
		t.Fatal(err)
	}
	write(t, def, "EXPORTS\nAlpha\n")
	if err := verifyProjection(source, header, def, ""); err == nil || !strings.Contains(err.Error(), "Beta") {
		t.Fatal(err)
	}
}

func TestExactArtifactMatchingRejectsPrefixOnlyMatches(t *testing.T) {
	header := exactHeaderExports("/* RemoveSteamControllerDevice */\nextern void RemoveSteamControllerDeviceEx();\n")
	if missing := missingExports(header, []string{"RemoveSteamControllerDevice"}); len(missing) != 1 {
		t.Fatalf("header prefix was accepted: %v", missing)
	}
	def := exactDefExports("EXPORTS\nRemoveSteamControllerDeviceEx\n")
	if missing := missingExports(def, []string{"RemoveSteamControllerDevice"}); len(missing) != 1 {
		t.Fatalf("DEF prefix was accepted: %v", missing)
	}
	dll := exactDLLExports("Ordinal/Name Pointer] Table\n [ 0] +base[ 1] 0000 RemoveSteamControllerDeviceEx\n")
	if missing := missingExports(dll, []string{"RemoveSteamControllerDevice"}); len(missing) != 1 {
		t.Fatalf("DLL prefix was accepted: %v", missing)
	}
}

func TestDLLNonExportTextDoesNotSatisfyProjection(t *testing.T) {
	dll := exactDLLExports("Import Tables\nRemoveSteamControllerDevice\nThe .rsrc Resource Directory section:\n")
	if missing := missingExports(dll, []string{"RemoveSteamControllerDevice"}); len(missing) != 1 {
		t.Fatalf("non-export text was accepted: %v", missing)
	}
}

func TestLLVMObjdumpExportTable(t *testing.T) {
	fixture := `The Export Tables (interpreted .edata section contents)
 Export Table:
  DLL name: libVIIPER.dll
  Ordinal base: 1
  Ordinal      RVA  Name
        1 0x00100000 AttachUSBDevice
        2 0x00100010 AttachUSBDeviceEx
       29 0x001001d0 RemoveSteamControllerDevice
       30 0x001001e0 RemoveSteamControllerDeviceEx
 The Import Tables (interpreted .idata section contents)`
	exports := exactDLLExports(fixture)
	for _, name := range []string{"AttachUSBDevice", "AttachUSBDeviceEx", "RemoveSteamControllerDevice", "RemoveSteamControllerDeviceEx"} {
		if _, ok := exports[name]; !ok {
			t.Fatalf("LLVM fixture did not extract %s: %v", name, exports)
		}
	}
	if missing := missingExports(exports, []string{"RemoveSteamControllerDevice"}); len(missing) != 0 {
		t.Fatal(missing)
	}
}

func TestDLLMalformedOrPrefixOnlyFailsClosed(t *testing.T) {
	if exports := exactDLLExports("not an export table\nRemoveSteamControllerDevice\n"); len(exports) != 0 {
		t.Fatalf("malformed dump produced exports: %v", exports)
	}
	prefixOnly := exactDLLExports("Export Table:\nOrdinal      RVA  Name\n 1 0x00100000 RemoveSteamControllerDeviceEx\n")
	if missing := missingExports(prefixOnly, []string{"RemoveSteamControllerDevice"}); len(missing) != 1 {
		t.Fatalf("prefix-only LLVM export was accepted: %v", missing)
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

func TestCanonicalExportsRejectNameMismatchAndDuplicate(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "mismatch.go"), "package p\n//export Foo\nfunc Bar() {}\n")
	if _, err := canonicalExports(dir); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "mismatch.go"), "package p\n//export Same\nfunc Same() {}\n")
	write(t, filepath.Join(dir, "duplicate.go"), "package p\n//export Same\nfunc Same() {}\n")
	if _, err := canonicalExports(dir); err == nil || !strings.Contains(err.Error(), "duplicate canonical export") {
		t.Fatal(err)
	}
}
