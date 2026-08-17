package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"runtime/cgo"
	"strings"
	"sync"
	"testing"

	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/usbip"
)

type teardownRecordingHandler struct {
	mu       sync.Mutex
	hw       *usbServerHandleWrapper
	records  []slog.Record
	lockFree []bool
}

func (h *teardownRecordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *teardownRecordingHandler) Handle(_ context.Context, r slog.Record) error {
	free := h.hw.lifecycleMu.TryLock()
	if free {
		h.hw.lifecycleMu.Unlock()
	}
	h.mu.Lock()
	h.records = append(h.records, r.Clone())
	h.lockFree = append(h.lockFree, free)
	h.mu.Unlock()
	return nil
}
func (h *teardownRecordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *teardownRecordingHandler) WithGroup(string) slog.Handler      { return h }

func teardownAttrs(t *testing.T, h *teardownRecordingHandler, operation string) map[string]any {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := len(h.records) - 1; i >= 0; i-- {
		r := h.records[i]
		attrs := recordAttrs(r)
		if attrs["operation"] == operation && strings.HasSuffix(r.Message, "teardown") {
			return attrs
		}
	}
	t.Fatalf("missing teardown record for %s", operation)
	return nil
}

func TestTypedRemoveDiagnosticsClassifyEveryTeardownPhase(t *testing.T) {
	cases := []struct {
		name                             string
		attached                         bool
		err                              error
		removeErr                        error
		wantResult, wantPhase, wantAfter string
	}{
		{"attached-success", true, nil, nil, "success", "complete", "detached"},
		{"detached-success", false, nil, nil, "success", "complete", "detached"},
		{"known-detach-failure", true, errors.New("detach failed"), nil, "retryable-failure", "detach", "attached"},
		{"unknown-detach", true, api.ErrDetachmentOutcomeUnknown, nil, "unsafe-outcome-unknown", "detach", "outcome-unknown"},
		{"logical-remove-failure", true, nil, errors.New("logical remove failed"), "retryable-failure", "logical-remove", "detached"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hw, hlog := newTeardownTestServer(t, uint32(10000+i))
			if tc.attached {
				hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
					return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 5000}, nil
				}
			}
			hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error { return tc.err }
			hw.ops.removeDevice = func(*usb.Server, uint32, string) error { return tc.removeErr }
			h := addTestMouse(t, hw, uint32(10000+i))
			if tc.attached && attachUSBDeviceResult(uintptr(h)) != deviceAttachSuccess {
				t.Fatal("attach setup failed")
			}
			got := removeTypedDeviceResult(uintptr(h), func(any) bool { return true })
			if got != map[string]typedDeviceRemoveResult{"success": typedDeviceRemoveSuccess, "retryable-failure": typedDeviceRemoveRetryableFailure, "unsafe-outcome-unknown": typedDeviceRemoveUnsafeOutcomeUnknown}[tc.wantResult] {
				t.Fatalf("result=%d", got)
			}
			attrs := teardownAttrs(t, hlog, "typed-device-remove")
			for key, want := range map[string]any{"result": tc.wantResult, "phase": tc.wantPhase, "attachmentStateAfter": tc.wantAfter, "busID": uint32(10000 + i), "deviceID": "1"} {
				if fmt.Sprint(attrs[key]) != fmt.Sprint(want) {
					t.Fatalf("%s=%v want=%v", key, attrs[key], want)
				}
			}
			if tc.attached && tc.wantResult == "success" && (fmt.Sprint(attrs["attachmentBackend"]) != fmt.Sprint(api.LocalhostAttachmentBackendCommand) || fmt.Sprint(attrs["importPort"]) != "5000") {
				t.Fatalf("token evidence=%v/%v", attrs["attachmentBackend"], attrs["importPort"])
			}
			if tc.wantResult == "unsafe-outcome-unknown" && hw.state != serverCloseFailed {
				t.Fatalf("state=%s", hw.state)
			}
			hlog.mu.Lock()
			for _, free := range hlog.lockFree {
				if !free {
					t.Fatal("teardown log emitted while lifecycleMu held")
				}
			}
			hlog.mu.Unlock()
		})
	}
}

func TestTypedRemoveDiagnosticsWrongFamilyDoesNotClaimDetachBackend(t *testing.T) {
	hw, hlog := newTeardownTestServer(t, 10005)
	h := addTestMouse(t, hw, 10005)
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 5050}, nil
	}
	detachCalls := 0
	hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
		detachCalls++
		return nil
	}
	if attachUSBDeviceResult(uintptr(h)) != deviceAttachSuccess {
		t.Fatal("attach setup failed")
	}
	if got := removeTypedDeviceResult(uintptr(h), func(any) bool { return false }); got != typedDeviceRemoveInvalid {
		t.Fatalf("result=%d", got)
	}
	attrs := teardownAttrs(t, hlog, "typed-device-remove")
	if fmt.Sprint(attrs["result"]) != "invalid" || fmt.Sprint(attrs["attachmentStateBefore"]) != "attached" || fmt.Sprint(attrs["detachBackendCalled"]) != "false" || attrs["serverStateBefore"] != "active" || detachCalls != 0 {
		t.Fatalf("attrs=%v", attrs)
	}
}

func newTeardownTestServer(t *testing.T, busID uint32) (*usbServerHandleWrapper, *teardownRecordingHandler) {
	t.Helper()
	hw, _ := newLifecycleTestServer(t, busID)
	h := &teardownRecordingHandler{hw: hw}
	hw.logger = slog.New(h)
	return hw, h
}

func callRemoveUSBBusForTest(handle uintptr, busID uint32) bool {
	fn := reflect.ValueOf(RemoveUSBBus)
	args := []reflect.Value{reflect.New(fn.Type().In(0)).Elem(), reflect.New(fn.Type().In(1)).Elem()}
	args[0].SetUint(uint64(handle))
	args[1].SetUint(uint64(busID))
	return fn.Call(args)[0].Bool()
}

func callCloseUSBServerForTest(handle uintptr) bool {
	fn := reflect.ValueOf(CloseUSBServer)
	arg := reflect.New(fn.Type().In(0)).Elem()
	arg.SetUint(uint64(handle))
	return fn.Call([]reflect.Value{arg})[0].Bool()
}

func diagnosticServerHandle(t *testing.T, hw *usbServerHandleWrapper) uintptr {
	t.Helper()
	h := cgo.NewHandle(hw)
	raw := uintptr(h)
	serverHandleRecords.Store(raw, hw)
	t.Cleanup(func() {
		if _, ok := serverHandleRecords.Load(raw); ok {
			serverHandleRecords.Delete(raw)
			h.Delete()
		}
	})
	return raw
}

func TestRemoveUSBBusDiagnosticsAndLockSafety(t *testing.T) {
	hw, hlog := newTeardownTestServer(t, 10020)
	serverHandle := diagnosticServerHandle(t, hw)
	if !callRemoveUSBBusForTest(serverHandle, 10020) {
		t.Fatal("bus removal failed")
	}
	attrs := teardownAttrs(t, hlog, "RemoveUSBBus")
	if fmt.Sprint(attrs["result"]) != "success" || fmt.Sprint(attrs["phase"]) != "complete" || fmt.Sprint(attrs["busID"]) != "10020" {
		t.Fatalf("attrs=%v", attrs)
	}
	if callRemoveUSBBusForTest(serverHandle, 10020) {
		t.Fatal("missing bus unexpectedly removed")
	}
	attrs = teardownAttrs(t, hlog, "RemoveUSBBus")
	if fmt.Sprint(attrs["result"]) != "invalid" || fmt.Sprint(attrs["phase"]) != "preflight" {
		t.Fatalf("missing bus attrs=%v", attrs)
	}
	hlog.mu.Lock()
	defer hlog.mu.Unlock()
	for _, free := range hlog.lockFree {
		if !free {
			t.Fatal("bus log emitted while lifecycleMu held")
		}
	}
}

func TestCloseDiagnosticsUnknownRepresentativeAndTransportRetry(t *testing.T) {
	hw, hlog := newTeardownTestServer(t, 10030)
	serverHandle := diagnosticServerHandle(t, hw)
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		return api.LocalhostAttachment{}, api.ErrAttachmentOutcomeUnknown
	}
	h1 := addTestMouse(t, hw, 10030)
	h2 := addTestMouse(t, hw, 10030)
	if attachUSBDeviceResult(uintptr(h1)) != deviceAttachUnsafeOutcomeUnknown {
		t.Fatal("unknown attach failed")
	}
	if !lookupIdentityExists(uintptr(h2)) {
		t.Fatal("second handle missing")
	}
	if callCloseUSBServerForTest(serverHandle) {
		t.Fatal("unknown close unexpectedly succeeded")
	}
	attrs := teardownAttrs(t, hlog, "CloseUSBServer")
	if fmt.Sprint(attrs["result"]) != "unsafe-outcome-unknown" || fmt.Sprint(attrs["phase"]) != "preflight" || fmt.Sprint(attrs["serverStateAfter"]) != "close-failed" || fmt.Sprint(attrs["unknownAttachmentCount"]) != "1" {
		t.Fatalf("close attrs=%v", attrs)
	}
	hlog.mu.Lock()
	defer hlog.mu.Unlock()
	for _, free := range hlog.lockFree {
		if !free {
			t.Fatal("close log emitted while lifecycleMu held")
		}
	}
}

func TestRemoveUSBBusDiagnosticsIdentifyActualFailingDevice(t *testing.T) {
	hw, hlog := newTeardownTestServer(t, 10040)
	serverHandle := diagnosticServerHandle(t, hw)
	attachCalls := 0
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		attachCalls++
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: int32(5100 + attachCalls)}, nil
	}
	detachCalls := 0
	hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
		detachCalls++
		if detachCalls == 2 {
			return errors.New("device B detach failed")
		}
		return nil
	}
	a, b := addTestMouse(t, hw, 10040), addTestMouse(t, hw, 10040)
	if attachUSBDeviceResult(uintptr(a)) != deviceAttachSuccess || attachUSBDeviceResult(uintptr(b)) != deviceAttachSuccess {
		t.Fatal("attach setup failed")
	}
	if callRemoveUSBBusForTest(serverHandle, 10040) {
		t.Fatal("bus removal unexpectedly succeeded")
	}
	attrs := teardownAttrs(t, hlog, "RemoveUSBBus")
	for key, want := range map[string]string{"result": "retryable-failure", "phase": "detach", "deviceID": "2", "importPort": "5102"} {
		if fmt.Sprint(attrs[key]) != want {
			t.Fatalf("%s=%v want=%s", key, attrs[key], want)
		}
	}
}

func TestRemoveUSBBusDiagnosticsIdentifyUnknownPreflightDevice(t *testing.T) {
	hw, hlog := newTeardownTestServer(t, 10041)
	serverHandle := diagnosticServerHandle(t, hw)
	attachCalls := 0
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		attachCalls++
		if attachCalls == 2 {
			return api.LocalhostAttachment{}, api.ErrAttachmentOutcomeUnknown
		}
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 5201}, nil
	}
	a, b := addTestMouse(t, hw, 10041), addTestMouse(t, hw, 10041)
	if attachUSBDeviceResult(uintptr(a)) != deviceAttachSuccess {
		t.Fatal("attach setup failed")
	}
	hw.lifecycleMu.Lock()
	hw.deviceHandleRecords[b].attachment.state = attachmentOutcomeUnknown
	hw.lifecycleMu.Unlock()
	if callRemoveUSBBusForTest(serverHandle, 10041) {
		t.Fatal("unknown bus removal unexpectedly succeeded")
	}
	attrs := teardownAttrs(t, hlog, "RemoveUSBBus")
	if fmt.Sprint(attrs["result"]) != "unsafe-outcome-unknown" || fmt.Sprint(attrs["phase"]) != "preflight" || fmt.Sprint(attrs["deviceID"]) != "2" {
		t.Fatalf("attrs=%v", attrs)
	}
}

func TestRemoveUSBBusDiagnosticsPreserveBusPresenceReconciliation(t *testing.T) {
	for _, gone := range []bool{false, true} {
		t.Run(fmt.Sprintf("gone-%t", gone), func(t *testing.T) {
			hw, hlog := newTeardownTestServer(t, 10042)
			serverHandle := diagnosticServerHandle(t, hw)
			hw.ops.removeBus = func(s *usb.Server, busID uint32) error {
				if gone {
					_ = s.RemoveBus(busID)
				}
				return errors.New("bus backend failed")
			}
			if got := callRemoveUSBBusForTest(serverHandle, 10042); got != gone {
				t.Fatalf("remove result=%t want=%t", got, gone)
			}
			attrs := teardownAttrs(t, hlog, "RemoveUSBBus")
			if gone {
				if fmt.Sprint(attrs["result"]) != "success" || fmt.Sprint(attrs["busPresentAfter"]) != "false" {
					t.Fatalf("reconciled attrs=%v", attrs)
				}
			} else if fmt.Sprint(attrs["result"]) != "retryable-failure" || fmt.Sprint(attrs["busPresentAfter"]) != "true" {
				t.Fatalf("present attrs=%v", attrs)
			}
		})
	}
}

func TestCloseDiagnosticsSuccessRetryAndDeterministicUnknownRepresentative(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		hw, hlog := newTeardownTestServer(t, 10043)
		h := diagnosticServerHandle(t, hw)
		if !callCloseUSBServerForTest(h) {
			t.Fatal("close failed")
		}
		attrs := teardownAttrs(t, hlog, "CloseUSBServer")
		if fmt.Sprint(attrs["result"]) != "success" || fmt.Sprint(attrs["phase"]) != "complete" || fmt.Sprint(attrs["busCountBefore"]) != "1" || fmt.Sprint(attrs["remainingBusCount"]) != "0" {
			t.Fatalf("attrs=%v", attrs)
		}
	})
	t.Run("transport-retry", func(t *testing.T) {
		hw, hlog := newTeardownTestServer(t, 10044)
		h := diagnosticServerHandle(t, hw)
		calls := 0
		hw.ops.close = func(*usb.Server) error {
			calls++
			if calls == 1 {
				return errors.New("transport close failed")
			}
			return nil
		}
		if callCloseUSBServerForTest(h) {
			t.Fatal("first close unexpectedly succeeded")
		}
		first := teardownAttrs(t, hlog, "CloseUSBServer")
		if fmt.Sprint(first["result"]) != "retryable-failure" || fmt.Sprint(first["phase"]) != "transport-close" || first["error"] != "transport close failed" {
			t.Fatalf("first attrs=%v", first)
		}
		if !callCloseUSBServerForTest(h) {
			t.Fatal("retry close failed")
		}
		second := teardownAttrs(t, hlog, "CloseUSBServer")
		if fmt.Sprint(second["result"]) != "success" || fmt.Sprint(second["phase"]) != "complete" || fmt.Sprint(second["serverStateBefore"]) != "close-failed" {
			t.Fatalf("second attrs=%v", second)
		}
	})
	t.Run("unknown-representative", func(t *testing.T) {
		hw, hlog := newTeardownTestServer(t, 10050)
		addTestBus(t, hw, 10040)
		h := diagnosticServerHandle(t, hw)
		hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
			return api.LocalhostAttachment{}, api.ErrAttachmentOutcomeUnknown
		}
		addTestMouse(t, hw, 10050)
		addTestMouse(t, hw, 10040)
		hw.lifecycleMu.Lock()
		for _, id := range []uint32{10050, 10040} {
			hw.deviceHandleRecords[hw.deviceHandles[id][0]].attachment.state = attachmentOutcomeUnknown
		}
		hw.lifecycleMu.Unlock()
		if callCloseUSBServerForTest(h) {
			t.Fatal("unknown close unexpectedly succeeded")
		}
		attrs := teardownAttrs(t, hlog, "CloseUSBServer")
		if fmt.Sprint(attrs["busID"]) != "10040" || fmt.Sprint(attrs["unknownAttachmentCount"]) != "2" {
			t.Fatalf("representative attrs=%v", attrs)
		}
	})
}
