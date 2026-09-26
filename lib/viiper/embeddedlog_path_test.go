package main

import (
	"path/filepath"
	"sync"
	"testing"
)

func TestDiagnosticLogDirectoryConfigOwnsPath(t *testing.T) {
	var config diagnosticLogDirectoryConfig
	buffer := []byte(`X:\test\ctw-log-dir`)
	want := string(buffer)
	if !config.set(want) {
		t.Fatal("valid UTF-8 directory was rejected")
	}
	for i := range buffer {
		buffer[i] = 'x'
	}
	if got := config.freeze(); got != want {
		t.Fatalf("frozen directory = %q, want original path %q", got, want)
	}
}

func TestDiagnosticLogDirectoryConfigAllowsReplacementBeforeFreeze(t *testing.T) {
	var config diagnosticLogDirectoryConfig
	if !config.set(`X:\original`) || !config.set(`Y:\replacement`) {
		t.Fatal("pending directory could not be replaced before initialization")
	}
	if got := config.freeze(); got != `Y:\replacement` {
		t.Fatalf("frozen directory = %q, want replacement path", got)
	}
}

func TestDiagnosticLogDirectoryConfigRejectsInvalidAndLateValues(t *testing.T) {
	var config diagnosticLogDirectoryConfig
	if config.set("") {
		t.Fatal("empty directory was accepted")
	}
	if config.set(string([]byte{0xff})) {
		t.Fatal("invalid UTF-8 directory was accepted")
	}
	if !config.set(`X:\test\ctw-log-dir`) {
		t.Fatal("valid directory was rejected")
	}
	before := config.freeze()
	if config.set(`Y:\too-late`) {
		t.Fatal("late directory change was accepted")
	}
	if after := config.freeze(); after != before {
		t.Fatalf("late setter changed path from %q to %q", before, after)
	}
}

func TestResolveEmbeddedLogPathUsesConfiguredDirectory(t *testing.T) {
	path, ok := resolveEmbeddedLogPath(`X:\test\ctw-log-dir`)
	if !ok {
		t.Fatal("configured directory did not resolve")
	}
	want := filepath.Join(`X:\test\ctw-log-dir`, embeddedLogFileName)
	if path != want {
		t.Fatalf("resolved path = %q, want %q", path, want)
	}
}

func TestXbox360DiagnosticBuildMarkerIsEmittedOnce(t *testing.T) {
	handler := &recordingHandler{}
	logger := buildEmbeddedLogger(handler, nil)
	var once sync.Once
	logXbox360RumbleDiagnosticBuildMarker(&once, logger)
	logXbox360RumbleDiagnosticBuildMarker(&once, logger)
	if len(handler.records) != 1 {
		t.Fatalf("diagnostic build marker count = %d, want 1", len(handler.records))
	}
	attrs := recordAttrs(handler.records[0])
	if attrs["Event"] != "X360RumbleDiagnosticBuild" || attrs["Mode"] != "forced-on" {
		t.Fatalf("diagnostic build marker attrs = %+v", attrs)
	}
}
