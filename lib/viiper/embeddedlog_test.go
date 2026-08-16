package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/usbip"
)

// These tests cover libVIIPER's owned diagnostic logging: a file sink that exists regardless of
// whether a VIIPERLogCallback is supplied, an optional callback that observes/mirrors without
// duplicating records into the file, and the "logging failure must never become a lifecycle
// failure" contract. None of them touch a real file, a real Windows module handle, or any real
// USB/IP driver/device.

// recordingHandler is a minimal slog.Handler used purely to assert which handlers received which
// records, independent of any real I/O.
type recordingHandler struct {
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func TestBuildEmbeddedLoggerFileSinkPresentWithoutCallback(t *testing.T) {
	file := &recordingHandler{}
	logger := buildEmbeddedLogger(file, nil)

	logger.Info("test message", "key", "value")

	if len(file.records) != 1 {
		t.Fatalf("file handler received %d records, want 1 (callback absence must not disable the file sink)", len(file.records))
	}
}

func TestBuildEmbeddedLoggerCallbackAndFileReceiveSameRecordExactlyOnce(t *testing.T) {
	file := &recordingHandler{}
	callback := &recordingHandler{}
	logger := buildEmbeddedLogger(file, callback)

	logger.Info("test message", "key", "value")

	if len(file.records) != 1 {
		t.Fatalf("file handler received %d records, want exactly 1 (no duplication)", len(file.records))
	}
	if len(callback.records) != 1 {
		t.Fatalf("callback handler received %d records, want exactly 1", len(callback.records))
	}
	if file.records[0].Message != "test message" || callback.records[0].Message != "test message" {
		t.Fatalf("file/callback did not receive the same logical record: file=%q callback=%q", file.records[0].Message, callback.records[0].Message)
	}
}

func TestBuildEmbeddedLoggerCallbackOnlyWhenNoFileSink(t *testing.T) {
	callback := &recordingHandler{}
	logger := buildEmbeddedLogger(nil, callback)

	logger.Info("test message")

	if len(callback.records) != 1 {
		t.Fatalf("callback handler received %d records, want 1", len(callback.records))
	}
}

func TestBuildEmbeddedLoggerNoHandlersDiscardsSafely(t *testing.T) {
	logger := buildEmbeddedLogger(nil, nil)
	if logger == nil {
		t.Fatal("buildEmbeddedLogger(nil, nil) must still return a usable logger")
	}
	// Must not panic and must not require a file/callback to exist.
	logger.Info("test message")
	logger.Error("test message")
}

func TestEmbeddedFileHandlerPreservesStructuredAttributes(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(embeddedFileHandler(&buf))

	logger.Info("attachment-timing",
		"operation", "attach",
		"layer", "canonical",
		"result", "success",
		"backendCalled", true,
		"totalUs", int64(1234),
		"lockWaitUs", int64(56),
	)

	out := buf.String()
	for _, want := range []string{
		"operation=attach",
		"layer=canonical",
		"result=success",
		"backendCalled=true",
		"totalUs=1234",
		"lockWaitUs=56",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("file output missing %q; got: %s", want, out)
		}
	}
}

func TestOpenEmbeddedLogFileHandlerResolutionFailureReturnsNilNotError(t *testing.T) {
	handler, writer := openEmbeddedLogFileHandler(
		func() (string, bool) { return "", false },
		func(string) (io.WriteCloser, error) {
			t.Fatal("openFile must not be called when resolve fails")
			return nil, nil
		},
	)
	if handler != nil || writer != nil {
		t.Fatal("expected a nil handler and nil writer when path resolution fails")
	}
}

func TestOpenEmbeddedLogFileHandlerOpenFailureReturnsNilNotError(t *testing.T) {
	handler, writer := openEmbeddedLogFileHandler(
		func() (string, bool) { return "C:\\nonexistent\\deeply\\nested\\path\\libVIIPER.log", true },
		func(string) (io.WriteCloser, error) { return nil, errors.New("simulated open failure") },
	)
	if handler != nil || writer != nil {
		t.Fatal("expected a nil handler and nil writer when file open fails")
	}
}

func TestOpenEmbeddedLogFileHandlerSuccessUsesResolvedPath(t *testing.T) {
	var openedPath string
	fw := &recordingWriteCloser{}
	handler, writer := openEmbeddedLogFileHandler(
		func() (string, bool) { return "fake/libVIIPER.log", true },
		func(path string) (io.WriteCloser, error) { openedPath = path; return fw, nil },
	)
	if handler == nil || writer == nil {
		t.Fatal("expected a non-nil handler and writer on success")
	}
	if openedPath != "fake/libVIIPER.log" {
		t.Fatalf("openFile path = %q, want fake/libVIIPER.log", openedPath)
	}
	slog.New(handler).Info("hello")
	if !writer.Flush() {
		t.Fatal("flush timed out waiting for the fake writer")
	}
	if len(fw.writes) == 0 {
		t.Fatal("handler never wrote to the opened file")
	}
}

type recordingWriteCloser struct{ writes [][]byte }

func (w *recordingWriteCloser) Write(p []byte) (int, error) {
	w.writes = append(w.writes, append([]byte(nil), p...))
	return len(p), nil
}
func (w *recordingWriteCloser) Close() error { return nil }

// TestAttachmentTimingFieldsSurviveIntoOwnedFile proves PR #26's attachment-timing diagnostics
// are unaffected by routing them through the new owned-file sink: the same stable field
// vocabulary (operation/layer/result/backendCalled/totalUs/lockWaitUs/backendUs/...) appears in
// the file's text output, unchanged.
func TestAttachmentTimingFieldsSurviveIntoOwnedFile(t *testing.T) {
	var buf bytes.Buffer
	hw, _ := newLifecycleTestServer(t, 9601)
	hw.logger = buildEmbeddedLogger(embeddedFileHandler(&buf), nil)
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 211}, nil
	}
	hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error { return nil }

	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9601, mustNewTestMouse(t), false)
	hw.lifecycleMu.Unlock()
	if !ok {
		t.Fatal("create failed")
	}
	if attachUSBDeviceResult(uintptr(h)) != deviceAttachSuccess {
		t.Fatal("attach failed")
	}
	if detachUSBDeviceResult(uintptr(h)) != deviceDetachSuccess {
		t.Fatal("detach failed")
	}

	out := buf.String()
	if !strings.Contains(out, "msg=attachment-timing") {
		t.Fatalf("owned file never received an attachment-timing record: %s", out)
	}
	for _, want := range []string{
		"operation=attach", "operation=detach",
		"layer=canonical",
		"result=success",
		"backendCalled=true",
		"totalUs=", "lockWaitUs=", "backendUs=",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("owned file output missing %q; got: %s", want, out)
		}
	}
}

// TestNoHighFrequencyLoggingOnStateUpdates proves state-setter calls (the per-input/per-frame
// hot path, e.g. SetSteamDeckDeviceState) never log anything, regardless of how many times they
// are called. Routine lifecycle/attach/detach diagnostics are exempt by construction: this test
// exercises only a state update loop, no attach/detach/create/remove calls.
func TestNoHighFrequencyLoggingOnStateUpdates(t *testing.T) {
	recorder := &recordingHandler{}
	hw, _ := newLifecycleTestServer(t, 9602)
	hw.logger = buildEmbeddedLogger(recorder, nil)

	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9602, mustNewTestMouse(t), false)
	hw.lifecycleMu.Unlock()
	if !ok {
		t.Fatal("create failed")
	}
	recorder.records = nil // Ignore whatever creation itself may have logged.

	for i := 0; i < 1000; i++ {
		if !withActiveDeviceHandle(uintptr(h), func(dhw *deviceHandleWrapper) bool { return true }) {
			t.Fatal("state-update seam rejected an active handle")
		}
	}

	if len(recorder.records) != 0 {
		t.Fatalf("state-update loop produced %d log records, want 0 (no per-input logging)", len(recorder.records))
	}
}

// withInvalidHandleLogger swaps invalidHandleLoggerFunc for the duration of a test, restoring it
// afterward, so tests can observe invalid-handle diagnostics without depending on the real
// process-wide singleton (real file, real module resolution).
func withInvalidHandleLogger(t *testing.T, logger *slog.Logger) {
	t.Helper()
	original := invalidHandleLoggerFunc
	invalidHandleLoggerFunc = func() *slog.Logger { return logger }
	t.Cleanup(func() { invalidHandleLoggerFunc = original })
}

// TestInvalidHandleTimingGoesToOwnedLoggerNotDefaultNotAnyServerCallback proves the fix for the
// review's core finding: zero/stale-handle attach/detach timing must land in libVIIPER's owned
// file-only fallback logger, never in Go's process-global slog.Default(), and never in any
// particular USBServerHandle's own VIIPERLogCallback -- an invalid handle has no legitimate
// server to own a callback for.
func TestInvalidHandleTimingGoesToOwnedLoggerNotDefaultNotAnyServerCallback(t *testing.T) {
	owned := &recordingHandler{}
	withInvalidHandleLogger(t, slog.New(owned))

	serverACallback := &recordingHandler{}
	hwA, _ := newLifecycleTestServer(t, 9610)
	hwA.logger = buildEmbeddedLogger(nil, serverACallback)

	if got := attachUSBDeviceResult(0); got != deviceAttachInvalid {
		t.Fatalf("attach result = %d, want invalid", got)
	}
	if got := detachUSBDeviceResult(0); got != deviceDetachInvalid {
		t.Fatalf("detach result = %d, want invalid", got)
	}

	if len(owned.records) != 2 {
		t.Fatalf("owned invalid-handle logger received %d records, want 2 (one attach, one detach)", len(owned.records))
	}
	for _, r := range owned.records {
		attrs := recordAttrs(r)
		if attrs["result"] != "invalid" || attrs["layer"] != "canonical" {
			t.Fatalf("unexpected invalid-handle record attrs: %+v", attrs)
		}
	}
	if len(serverACallback.records) != 0 {
		t.Fatalf("server A's unrelated callback received %d invalid-handle records, want 0", len(serverACallback.records))
	}
}

// TestMultiServerIsolationInvalidHandleNeverRoutesToMostRecentlyCreatedServer proves that
// creating a second USBServerHandle after the first does not cause an invalid-handle diagnostic
// (which belongs to neither) to leak into either server's own callback -- the exact failure mode
// slog.SetDefault previously allowed.
func TestMultiServerIsolationInvalidHandleNeverRoutesToMostRecentlyCreatedServer(t *testing.T) {
	owned := &recordingHandler{}
	withInvalidHandleLogger(t, slog.New(owned))

	callbackA := &recordingHandler{}
	hwA, _ := newLifecycleTestServer(t, 9611)
	hwA.logger = buildEmbeddedLogger(nil, callbackA)

	callbackB := &recordingHandler{}
	hwB, _ := newLifecycleTestServer(t, 9612)
	hwB.logger = buildEmbeddedLogger(nil, callbackB)

	// hwB was created after hwA -- exactly the scenario where a process-global slog.SetDefault
	// would have silently redirected hwA's invalid-handle fallback into hwB's callback.
	if got := attachUSBDeviceResult(0); got != deviceAttachInvalid {
		t.Fatalf("attach result = %d, want invalid", got)
	}

	if len(owned.records) != 1 {
		t.Fatalf("owned invalid-handle logger received %d records, want 1", len(owned.records))
	}
	if len(callbackA.records) != 0 {
		t.Fatalf("server A's callback received %d invalid-handle records, want 0", len(callbackA.records))
	}
	if len(callbackB.records) != 0 {
		t.Fatalf("server B's callback received %d invalid-handle records, want 0 (this is the regression this test guards)", len(callbackB.records))
	}
}

// TestAttachDetachReturnPromptlyDespiteStuckBackingWriter is the integration-level regression
// this PR exists to fix: even with a deliberately stuck backing disk writer, attachUSBDeviceResult
// and detachUSBDeviceResult (the logic behind AttachUSBDeviceEx/DetachUSBDeviceEx) return promptly
// -- their timing diagnostic is enqueued, not synchronously written, so a slow filesystem never
// extends the wall-clock time an embedding application waits for these calls.
func TestAttachDetachReturnPromptlyDespiteStuckBackingWriter(t *testing.T) {
	gw := newGatedWriter() // Never opened: every real write to it would block forever.
	writer := newAsyncLogWriter(gw, asyncLogQueueCapacity)

	hw, _ := newLifecycleTestServer(t, 9603)
	hw.logger = buildEmbeddedLogger(embeddedFileHandler(writer), nil)
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 212}, nil
	}
	hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error { return nil }

	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9603, mustNewTestMouse(t), false)
	hw.lifecycleMu.Unlock()
	if !ok {
		t.Fatal("create failed")
	}

	attachDone := make(chan deviceAttachResult, 1)
	go func() { attachDone <- attachUSBDeviceResult(uintptr(h)) }()
	select {
	case got := <-attachDone:
		if got != deviceAttachSuccess {
			t.Fatalf("attach result = %d, want success", got)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("attachUSBDeviceResult blocked on a stuck backing log writer")
	}

	detachDone := make(chan deviceDetachResult, 1)
	go func() { detachDone <- detachUSBDeviceResult(uintptr(h)) }()
	select {
	case got := <-detachDone:
		if got != deviceDetachSuccess {
			t.Fatalf("detach result = %d, want success", got)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("detachUSBDeviceResult blocked on a stuck backing log writer")
	}
}

// TestLoggingFailureDoesNotChangeLifecycleResultSemantics proves that even with a completely
// broken embedded logger (file sink unavailable, no callback), the classified attach/detach
// contract established in PR #23/#24/#26 is entirely unaffected -- a logging failure is
// diagnostic-only and must never become a routing/lifecycle failure.
func TestLoggingFailureDoesNotChangeLifecycleResultSemantics(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9600)
	hw.logger = buildEmbeddedLogger(nil, nil) // Simulates total logging failure: no file, no callback.
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 210}, nil
	}
	hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error { return nil }
	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9600, mustNewTestMouse(t), false)
	hw.lifecycleMu.Unlock()
	if !ok {
		t.Fatal("create failed")
	}
	if got := attachUSBDeviceResult(uintptr(h)); got != deviceAttachSuccess {
		t.Fatalf("result = %d, want success even with a fully broken logger", got)
	}
	if got := detachUSBDeviceResult(uintptr(h)); got != deviceDetachSuccess {
		t.Fatalf("result = %d, want success even with a fully broken logger", got)
	}
}
