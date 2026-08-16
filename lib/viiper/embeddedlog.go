package main

import (
	"io"
	"log/slog"
	"os"
	"sync"

	vlog "github.com/Alia5/VIIPER/internal/log"
)

// embeddedLogLevel intentionally captures the same verbosity a VIIPERLogCallback has always
// received (Enabled has never filtered levels for the callback path) so the file sink does not
// silently see less than a callback consumer already does.
const embeddedLogLevel = slog.LevelDebug

// buildEmbeddedLogger composes libVIIPER's own diagnostic sink with an optional observer.
// fileHandler is nil when no file sink is available (module-path resolution or file-open
// failure) -- that is diagnostic-only and must never fail server creation or fall back to
// stdout/stderr CLI-style output inside an embedded DLL. callbackHandler is nil when no
// VIIPERLogCallback was supplied; its absence never disables the file sink, and its presence
// never duplicates a record into the file (each handler receives the record exactly once).
// This never calls slog.SetDefault: the returned logger is always libVIIPER's own explicit
// logger, never a replacement for the embedding process's global default.
func buildEmbeddedLogger(fileHandler, callbackHandler slog.Handler) *slog.Logger {
	var handlers []slog.Handler
	if fileHandler != nil {
		handlers = append(handlers, fileHandler)
	}
	if callbackHandler != nil {
		handlers = append(handlers, callbackHandler)
	}
	switch len(handlers) {
	case 0:
		return slog.New(slog.DiscardHandler)
	case 1:
		return slog.New(handlers[0])
	default:
		return slog.New(vlog.NewMultiHandler(handlers...))
	}
}

// embeddedFileHandler wraps a writer as the plain structured text handler libVIIPER's owned log
// file uses. Deliberately not the CLI's colorHandler (which emits ANSI escape codes unsuitable
// for a plain log file) and deliberately not internal/log.SetupLogger (which also attaches
// stdout/stderr handlers and calls slog.SetDefault -- both CLI-specific behaviors that do not
// belong in an embedded DLL).
func embeddedFileHandler(w io.Writer) slog.Handler {
	return slog.NewTextHandler(w, &slog.HandlerOptions{Level: embeddedLogLevel})
}

// openEmbeddedLogFileHandler resolves and opens libVIIPER's owned diagnostic file. resolve and
// openFile are injected so this logic is fully testable without any real module-path Windows API
// call or real filesystem dependency; failures at either step return a nil handler rather than an
// error, matching the "logging failures must never become routing failures" contract -- callers
// must never treat a nil result as anything other than "no file sink this run."
func openEmbeddedLogFileHandler(resolve func() (string, bool), openFile func(path string) (io.WriteCloser, error)) slog.Handler {
	path, ok := resolve()
	if !ok {
		return nil
	}
	f, err := openFile(path)
	if err != nil {
		return nil
	}
	return embeddedFileHandler(f)
}

var (
	embeddedLogFileHandlerOnce  sync.Once
	embeddedLogFileHandlerCache slog.Handler
)

// openRealEmbeddedLogFileHandler opens (once per process) the real libVIIPER.log beside the
// loaded shared library in append mode, so multiple NewUSBServer calls in the same process share
// one file and never truncate earlier diagnostic history. resolveEmbeddedLogPath is platform-
// specific (embeddedlog_windows.go / embeddedlog_other.go).
func openRealEmbeddedLogFileHandler() slog.Handler {
	embeddedLogFileHandlerOnce.Do(func() {
		embeddedLogFileHandlerCache = openEmbeddedLogFileHandler(resolveEmbeddedLogPath, func(path string) (io.WriteCloser, error) {
			return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		})
	})
	return embeddedLogFileHandlerCache
}
