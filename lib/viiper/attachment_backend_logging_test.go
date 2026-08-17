package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/usbip"
)

func backendRecordMessages(h *teardownRecordingHandler) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var messages []string
	for _, record := range h.records {
		if strings.Contains(record.Message, "backend-probe") {
			messages = append(messages, record.Message)
		}
	}
	return messages
}

func assertBackendRecordsLockFree(t *testing.T, h *teardownRecordingHandler) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, record := range h.records {
		if strings.Contains(record.Message, "backend-probe") && !h.lockFree[i] {
			t.Fatal("backend record replayed while lifecycleMu held")
		}
	}
}

func TestDeferredBackendLogBatchPreservesStructuredRecord(t *testing.T) {
	hw, hlog := newTeardownTestServer(t, 10073)
	batch := newDeferredLogBatch()
	batch.logger.With("scope", "backend").WithGroup("native").Warn("backend-probe-structured", "port", 5373)
	batch.replay(hw.logger)
	hlog.mu.Lock()
	defer hlog.mu.Unlock()
	if len(hlog.records) != 1 {
		t.Fatalf("records=%d want=1", len(hlog.records))
	}
	record := hlog.records[0]
	if record.Level != slog.LevelWarn || record.Message != "backend-probe-structured" || record.Time.IsZero() || !hlog.lockFree[0] {
		t.Fatalf("record=%+v lockFree=%v", record, hlog.lockFree[0])
	}
	attrs := recordAttrs(record)
	if attrs["scope"] != "backend" {
		t.Fatalf("root attrs=%v, want scope=backend", attrs)
	}
	nativeAttrs, ok := groupAttrs(attrs["native"])
	if !ok {
		t.Fatalf("native=%v, want group", attrs["native"])
	}
	if nativeAttrs["port"] != int64(5373) {
		t.Fatalf("native attrs=%v, want port=5373", nativeAttrs)
	}
}

func recordAttrsFromAttrs(attrs []slog.Attr) map[string]any {
	m := make(map[string]any, len(attrs))
	for _, attr := range attrs {
		m[attr.Key] = attr.Value.Any()
	}
	return m
}

func groupAttrs(value any) (map[string]any, bool) {
	switch group := value.(type) {
	case []slog.Attr:
		return recordAttrsFromAttrs(group), true
	case slog.Value:
		if group.Kind() == slog.KindGroup {
			return recordAttrsFromAttrs(group.Group()), true
		}
	}
	return nil, false
}

func TestDeferredBackendLogBatchPreservesOrderedGroups(t *testing.T) {
	batch := newDeferredLogBatch()
	batch.logger.WithGroup("g1").With("a", 1).WithGroup("g2").With("b", 2).Info("ordered", "c", 3)

	var captured slog.Record
	handler := &recordingHandler{}
	logger := slog.New(handler)
	batch.replay(logger)
	if len(handler.records) != 1 {
		t.Fatalf("records=%d want=1", len(handler.records))
	}
	captured = handler.records[0]
	root := recordAttrs(captured)
	if len(root) != 1 {
		t.Fatalf("root=%v want only g1", root)
	}
	g1Attrs, ok := groupAttrs(root["g1"])
	if !ok {
		t.Fatalf("g1=%v want group", root["g1"])
	}
	if g1Attrs["a"] != int64(1) {
		t.Fatalf("g1=%v want a=1", g1Attrs)
	}
	g2Attrs, ok := groupAttrs(g1Attrs["g2"])
	if !ok {
		t.Fatalf("g1=%v want nested g2", g1Attrs)
	}
	if g2Attrs["b"] != int64(2) || g2Attrs["c"] != int64(3) {
		t.Fatalf("g2=%v want b=2,c=3", g2Attrs)
	}
}

func TestDeferredBackendLogBatchHonorsDestinationEnabled(t *testing.T) {
	batch := newDeferredLogBatch()
	batch.logger.Info("enabled-check")
	handler := &levelRecordingHandler{enabled: false}
	batch.replay(slog.New(handler))
	if handler.handleCalls != 0 {
		t.Fatalf("destination Handle calls=%d want=0", handler.handleCalls)
	}
	handler.enabled = true
	batch.replay(slog.New(handler))
	if handler.handleCalls != 1 {
		t.Fatalf("enabled destination Handle calls=%d want=1", handler.handleCalls)
	}
}

type levelRecordingHandler struct {
	enabled     bool
	handleCalls int
}

func (h *levelRecordingHandler) Enabled(context.Context, slog.Level) bool { return h.enabled }
func (h *levelRecordingHandler) Handle(context.Context, slog.Record) error {
	h.handleCalls++
	return nil
}
func (h *levelRecordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *levelRecordingHandler) WithGroup(string) slog.Handler      { return h }

func TestAttachmentBackendLogsReplayAfterUnlock(t *testing.T) {
	hw, hlog := newTeardownTestServer(t, 10070)
	h := addTestMouse(t, hw, 10070)
	var attached api.LocalhostAttachment
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		return func() api.LocalhostAttachment {
			attached = api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 5370}
			return attached
		}(), nil
	}
	hw.ops.detachLocalhost = func(_ context.Context, got api.LocalhostAttachment, logger *slog.Logger) error {
		if got != attached {
			t.Fatalf("detach token=%v want=%v", got, attached)
		}
		logger.Warn("backend-probe-detach", "port", got.Port)
		return nil
	}
	// The attach callback must receive the capture logger, not the real handler.
	hw.ops.attachLocalhostTracked = func(_ context.Context, _ *usbip.ExportMeta, _ uint16, _ bool, logger *slog.Logger) (api.LocalhostAttachment, error) {
		logger.Info("backend-probe-attach", "port", 5370)
		attached = api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 5370}
		return attached, nil
	}
	if got := attachUSBDeviceResult(uintptr(h)); got != deviceAttachSuccess {
		t.Fatalf("attach result=%v", got)
	}
	if got := detachUSBDeviceResult(uintptr(h)); got != deviceDetachSuccess {
		t.Fatalf("detach result=%v", got)
	}
	assertBackendRecordsLockFree(t, hlog)
	messages := backendRecordMessages(hlog)
	if fmt.Sprint(messages) != "[backend-probe-attach backend-probe-detach]" {
		t.Fatalf("backend messages=%v", messages)
	}
}

func TestAttachmentBackendFailureLogsReplayAfterUnlock(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want deviceAttachResult
	}{
		{name: "retryable", err: errors.New("attach failed"), want: deviceAttachRetryableFailure},
		{name: "unknown", err: api.ErrAttachmentOutcomeUnknown, want: deviceAttachUnsafeOutcomeUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hw, hlog := newTeardownTestServer(t, 10071)
			h := addTestMouse(t, hw, 10071)
			hw.ops.attachLocalhostTracked = func(_ context.Context, _ *usbip.ExportMeta, _ uint16, _ bool, logger *slog.Logger) (api.LocalhostAttachment, error) {
				logger.Warn("backend-probe-failure", "kind", tc.name)
				return api.LocalhostAttachment{}, tc.err
			}
			if got := attachUSBDeviceResult(uintptr(h)); got != tc.want {
				t.Fatalf("result=%v want=%v", got, tc.want)
			}
			assertBackendRecordsLockFree(t, hlog)
			if len(backendRecordMessages(hlog)) != 1 {
				t.Fatalf("backend records=%v", backendRecordMessages(hlog))
			}
		})
	}
}

func TestExplicitDetachFailureAndUnknownReplayAfterUnlock(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want deviceDetachResult
	}{
		{name: "retryable", err: errors.New("detach failed"), want: deviceDetachRetryableFailure},
		{name: "unknown", err: api.ErrDetachmentOutcomeUnknown, want: deviceDetachUnsafeOutcomeUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hw, hlog := newTeardownTestServer(t, 10074)
			h := addTestMouse(t, hw, 10074)
			token := api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 5374}
			hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
				return token, nil
			}
			detachCalls := 0
			hw.ops.detachLocalhost = func(_ context.Context, got api.LocalhostAttachment, logger *slog.Logger) error {
				detachCalls++
				if got != token {
					t.Fatalf("detach token=%v want=%v", got, token)
				}
				logger.Warn("backend-probe-detach-failure", "port", got.Port)
				return tc.err
			}
			if attachUSBDeviceResult(uintptr(h)) != deviceAttachSuccess {
				t.Fatal("attach setup failed")
			}
			if got := detachUSBDeviceResult(uintptr(h)); got != tc.want {
				t.Fatalf("result=%v want=%v", got, tc.want)
			}
			if tc.want == deviceDetachUnsafeOutcomeUnknown {
				if got := detachUSBDeviceResult(uintptr(h)); got != tc.want {
					t.Fatalf("repeated result=%v want=%v", got, tc.want)
				}
			}
			if detachCalls != 1 {
				t.Fatalf("detach calls=%d want=1", detachCalls)
			}
			dhw := hw.deviceHandleRecords[h]
			if dhw == nil || dhw.attachment.attachment != token {
				t.Fatalf("retained token=%v want=%v", dhw.attachment.attachment, token)
			}
			if tc.want == deviceDetachRetryableFailure && hw.state != serverActive {
				t.Fatalf("server state=%s want active", hw.state)
			}
			if tc.want == deviceDetachUnsafeOutcomeUnknown && (dhw.attachment.state != attachmentOutcomeUnknown || hw.state != serverCloseFailed) {
				t.Fatalf("state=%v server=%s", dhw.attachment.state, hw.state)
			}
			assertBackendRecordsLockFree(t, hlog)
		})
	}
}

func TestAttachmentBackendRecordsPrecedeCanonicalSummaries(t *testing.T) {
	hw, hlog := newTeardownTestServer(t, 10075)
	h := addTestMouse(t, hw, 10075)
	token := api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 5375}
	hw.ops.attachLocalhostTracked = func(_ context.Context, _ *usbip.ExportMeta, _ uint16, _ bool, logger *slog.Logger) (api.LocalhostAttachment, error) {
		logger.Info("backend-probe-attach-order")
		return token, nil
	}
	hw.ops.detachLocalhost = func(_ context.Context, _ api.LocalhostAttachment, logger *slog.Logger) error {
		logger.Info("backend-probe-detach-order")
		return nil
	}
	if attachUSBDeviceResult(uintptr(h)) != deviceAttachSuccess || detachUSBDeviceResult(uintptr(h)) != deviceDetachSuccess {
		t.Fatal("attach/detach failed")
	}
	hlog.mu.Lock()
	defer hlog.mu.Unlock()
	attachBackend, attachSummary, detachBackend, detachSummary := -1, -1, -1, -1
	for i, record := range hlog.records {
		switch record.Message {
		case "backend-probe-attach-order":
			attachBackend = i
		case "backend-probe-detach-order":
			detachBackend = i
		case "attachment-timing":
			attrs := recordAttrs(record)
			if attrs["operation"] == "attach" {
				attachSummary = i
			}
			if attrs["operation"] == "detach" {
				detachSummary = i
			}
		}
	}
	if !(attachBackend >= 0 && attachSummary > attachBackend && detachBackend > attachSummary && detachSummary > detachBackend) {
		t.Fatalf("record order backend attach=%d attach summary=%d backend detach=%d detach summary=%d", attachBackend, attachSummary, detachBackend, detachSummary)
	}
}

func TestTypedRemoveBackendRecordPrecedesTeardown(t *testing.T) {
	hw, hlog := newTeardownTestServer(t, 10076)
	h := addTestMouse(t, hw, 10076)
	token := api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 5376}
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		return token, nil
	}
	hw.ops.detachLocalhost = func(_ context.Context, _ api.LocalhostAttachment, logger *slog.Logger) error {
		logger.Info("backend-probe-typed-remove")
		return nil
	}
	if attachUSBDeviceResult(uintptr(h)) != deviceAttachSuccess || removeTypedDeviceResult(uintptr(h), func(any) bool { return true }) != typedDeviceRemoveSuccess {
		t.Fatal("setup or typed remove failed")
	}
	hlog.mu.Lock()
	defer hlog.mu.Unlock()
	backend, teardown := -1, -1
	for i, record := range hlog.records {
		if record.Message == "backend-probe-typed-remove" {
			backend = i
		}
		if record.Message == "typed-device-remove teardown" {
			teardown = i
		}
	}
	if backend < 0 || teardown <= backend {
		t.Fatalf("backend index=%d teardown index=%d", backend, teardown)
	}
}

func TestRemoveUSBBusPreservesDeferredDetachOrder(t *testing.T) {
	hw, hlog := newTeardownTestServer(t, 10077)
	serverHandle := diagnosticServerHandle(t, hw)
	first := addTestMouse(t, hw, 10077)
	second := addTestMouse(t, hw, 10077)
	// Use registration order explicitly: each attach receives its own token.
	attachCount := 0
	hw.ops.attachLocalhostTracked = func(_ context.Context, _ *usbip.ExportMeta, _ uint16, _ bool, _ *slog.Logger) (api.LocalhostAttachment, error) {
		attachCount++
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: int32(5377 + attachCount - 1)}, nil
	}
	hw.ops.detachLocalhost = func(_ context.Context, token api.LocalhostAttachment, logger *slog.Logger) error {
		logger.Info("backend-probe-bus-order", "port", token.Port)
		return nil
	}
	if attachUSBDeviceResult(uintptr(first)) != deviceAttachSuccess || attachUSBDeviceResult(uintptr(second)) != deviceAttachSuccess {
		t.Fatal("attach setup failed")
	}
	if !callRemoveUSBBusForTest(serverHandle, 10077) {
		t.Fatal("bus remove failed")
	}
	hlog.mu.Lock()
	defer hlog.mu.Unlock()
	var got []int64
	for _, record := range hlog.records {
		if record.Message == "backend-probe-bus-order" {
			got = append(got, recordAttrs(record)["port"].(int64))
		}
	}
	if fmt.Sprint(got) != "[5377 5378]" {
		t.Fatalf("detach order=%v want=[5377 5378]", got)
	}
}

func TestAttachmentTimingSnapshotsBeforeReplay(t *testing.T) {
	hw, hlog := newTeardownTestServer(t, 10078)
	h := addTestMouse(t, hw, 10078)
	entered := make(chan struct{})
	release := make(chan struct{})
	var snapshot int64
	hw.onAttachmentTimingSnapshot = func(totalUs int64) {
		snapshot = totalUs
	}
	hw.ops.attachLocalhostTracked = func(_ context.Context, _ *usbip.ExportMeta, _ uint16, _ bool, logger *slog.Logger) (api.LocalhostAttachment, error) {
		logger.Info("backend-probe-timing")
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 5378}, nil
	}
	// The handler blocks replay deterministically, after the snapshot hook ran.
	blocking := &blockingBackendHandler{inner: hlog, entered: entered, release: release}
	hw.logger = slog.New(blocking)
	done := make(chan deviceAttachResult, 1)
	go func() { done <- attachUSBDeviceResult(uintptr(h)) }()
	<-entered
	if snapshot < 0 {
		t.Fatalf("snapshot totalUs=%d", snapshot)
	}
	close(release)
	if got := <-done; got != deviceAttachSuccess {
		t.Fatalf("attach result=%v", got)
	}
	attrs := recordAttrs(hlog.records[len(hlog.records)-1])
	if attrs["operation"] != "attach" || attrs["totalUs"] != snapshot {
		t.Fatalf("timing attrs=%v snapshot=%d", attrs, snapshot)
	}
}

type blockingBackendHandler struct {
	inner   *teardownRecordingHandler
	entered chan<- struct{}
	release <-chan struct{}
}

func (h *blockingBackendHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *blockingBackendHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Message == "backend-probe-timing" {
		close(h.entered)
		<-h.release
	}
	return h.inner.Handle(ctx, record)
}
func (h *blockingBackendHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *blockingBackendHandler) WithGroup(string) slog.Handler      { return h }

func TestBackendLogReplayAcrossCreateRemoveBusAndClose(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind string
	}{
		{name: "create", kind: "create"},
		{name: "remove", kind: "remove"},
		{name: "bus", kind: "bus"},
		{name: "close", kind: "close"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hw, hlog := newTeardownTestServer(t, 10072)
			hw.ops.attachLocalhostTracked = func(_ context.Context, _ *usbip.ExportMeta, _ uint16, _ bool, logger *slog.Logger) (api.LocalhostAttachment, error) {
				logger.Info("backend-probe-attach", "kind", tc.kind)
				return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 5372}, nil
			}
			hw.ops.detachLocalhost = func(_ context.Context, _ api.LocalhostAttachment, logger *slog.Logger) error {
				logger.Info("backend-probe-detach", "kind", tc.kind)
				return nil
			}
			serverHandle := diagnosticServerHandle(t, hw)
			if tc.kind == "create" {
				var h deviceHandle
				if !createXbox360Device(serverHandle, &h, 10072, true, 0, 0, 0) {
					t.Fatal("auto-attach create failed")
				}
			} else {
				h := addTestMouse(t, hw, 10072)
				if attachUSBDeviceResult(uintptr(h)) != deviceAttachSuccess {
					t.Fatal("attach setup failed")
				}
				switch tc.kind {
				case "remove":
					if removeTypedDeviceResult(uintptr(h), func(any) bool { return true }) != typedDeviceRemoveSuccess {
						t.Fatal("typed remove failed")
					}
				case "bus":
					if !callRemoveUSBBusForTest(serverHandle, 10072) {
						t.Fatal("bus remove failed")
					}
				case "close":
					if !callCloseUSBServerForTest(serverHandle) {
						t.Fatal("server close failed")
					}
				}
			}
			assertBackendRecordsLockFree(t, hlog)
			if len(backendRecordMessages(hlog)) == 0 {
				t.Fatal("missing replayed backend records")
			}
		})
	}
}
