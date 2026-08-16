//go:build windows

package main

import (
	"path/filepath"
	"testing"
)

// This is a smoke test of the real GetModuleHandleExW/GetModuleFileNameW resolution path. It
// deliberately does not assert on a specific directory (this differs between `go test`'s own
// test binary and a real libVIIPER.dll load, and must not depend on the developer machine's
// install layout) -- only that resolution succeeds deterministically from within a loaded PE
// module and produces a well-formed libVIIPER.log path. The behavior-affecting logic this
// resolver feeds into (openEmbeddedLogFileHandler) is covered independently in
// embeddedlog_test.go via injected fakes, which is the actual required test seam.
func TestResolveEmbeddedLogPathSmoke(t *testing.T) {
	path, ok := resolveEmbeddedLogPath()
	if !ok {
		t.Fatal("resolveEmbeddedLogPath failed inside a loaded PE module; module handle resolution should always succeed here")
	}
	if filepath.Base(path) != embeddedLogFileName {
		t.Fatalf("resolved path = %q, want basename %q", path, embeddedLogFileName)
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("resolved path = %q, want an absolute path", path)
	}
}
