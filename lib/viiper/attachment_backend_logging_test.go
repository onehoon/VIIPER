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
	if attrs["native"] == nil || !strings.Contains(fmt.Sprint(attrs["native"]), "scope") || !strings.Contains(fmt.Sprint(attrs["native"]), "port") {
		t.Fatalf("attrs=%v", attrs)
	}
}

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
