//go:build windows

package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This is a smoke test of the real GetModuleHandleExW/GetModuleFileNameW resolution path. It
// deliberately does not assert on a specific directory (this differs between `go test`'s own
// test binary and a real libVIIPER.dll load, and must not depend on the developer machine's
// install layout) -- only that resolution succeeds deterministically from within a loaded PE
// module and produces a well-formed libVIIPER.log path. Path handling is covered separately by
// the injected sink tests and the real-filesystem rollover regressions below.
func TestResolveEmbeddedLogPathSmoke(t *testing.T) {
	path, ok := resolveEmbeddedLogPath("")
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

func TestOSFileDailyLogWriterResetWorksWithProductionAppendHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), embeddedLogFileName)
	if err := os.WriteFile(path, []byte("stale-old-day\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })

	w := &osFileDailyLogWriter{f: f, path: path}
	if err := w.Reset(); err != nil {
		t.Fatalf("Reset failed for production append handle: %v", err)
	}
	for _, record := range []string{"new-day-1\n", "new-day-2\n"} {
		if _, err := w.Write([]byte(record)); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const want = "new-day-1\nnew-day-2\n"
	if string(got) != want {
		t.Fatalf("log contents = %q, want %q", got, want)
	}
}

func TestEmbeddedLogRolloverKeepsRelativePathStableAcrossWorkingDirectoryChange(t *testing.T) {
	originalWorkingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	t.Cleanup(func() {
		if err := os.Chdir(originalWorkingDirectory); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})

	initialWorkingDirectory := filepath.Join(root, "initial")
	changedWorkingDirectory := filepath.Join(root, "changed")
	initialLogDirectory := filepath.Join(initialWorkingDirectory, "logs")
	changedLogDirectory := filepath.Join(changedWorkingDirectory, "logs")
	for _, dir := range []string{initialLogDirectory, changedLogDirectory} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	initialLogPath := filepath.Join(initialLogDirectory, embeddedLogFileName)
	if err := os.WriteFile(initialLogPath, []byte("stale-old-day\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changedLogPath := filepath.Join(changedLogDirectory, embeddedLogFileName)
	const changedDirectorySentinel = "must-not-be-truncated\n"
	if err := os.WriteFile(changedLogPath, []byte(changedDirectorySentinel), 0o644); err != nil {
		t.Fatal(err)
	}

	today := time.Now()
	yesterday := today.AddDate(0, 0, -1)
	if err := os.Chtimes(initialLogPath, yesterday, yesterday); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(initialWorkingDirectory); err != nil {
		t.Fatal(err)
	}

	relativePath := filepath.Join("logs", embeddedLogFileName)
	wantAbsolutePath, err := filepath.Abs(relativePath)
	if err != nil {
		t.Fatal(err)
	}
	var statPath, openedPath string
	var file *os.File
	handler, writer := openEmbeddedLogFileHandler(
		func() (string, bool) { return relativePath, true },
		func(path string) (time.Time, bool, error) {
			statPath = path
			return realStatModTime(path)
		},
		func(path string) (dailyLogWriter, error) {
			openedPath = path
			var openErr error
			file, openErr = os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if openErr != nil {
				return nil, openErr
			}
			t.Cleanup(func() { _ = file.Close() })
			return &osFileDailyLogWriter{f: file, path: path}, nil
		},
		func() time.Time { return today },
	)
	if handler == nil || writer == nil {
		t.Fatal("expected a real embedded file sink")
	}
	if statPath != wantAbsolutePath || openedPath != wantAbsolutePath {
		t.Fatalf("stat/open paths = %q/%q, want fixed absolute path %q", statPath, openedPath, wantAbsolutePath)
	}

	if err := os.Chdir(changedWorkingDirectory); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(handler)
	logger.Info("new-day-1")
	logger.Info("new-day-2")
	if !writer.Flush() {
		t.Fatal("timed out flushing rollover records")
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(originalWorkingDirectory); err != nil {
		t.Fatal(err)
	}

	gotInitial, err := os.ReadFile(initialLogPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(gotInitial), "stale-old-day") ||
		!strings.Contains(string(gotInitial), "msg=new-day-1") ||
		!strings.Contains(string(gotInitial), "msg=new-day-2") {
		t.Fatalf("original log after rollover = %q, want only both new-day records", gotInitial)
	}
	gotChanged, err := os.ReadFile(changedLogPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotChanged) != changedDirectorySentinel {
		t.Fatalf("log beside changed working directory = %q, want untouched sentinel %q", gotChanged, changedDirectorySentinel)
	}
}
