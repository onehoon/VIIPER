package main

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/Alia5/VIIPER/internal/server/api"
	viiperusb "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/Alia5/VIIPER/virtualbus"
)

func callCreateUSBBusForTest(handle uintptr, busID uint32) bool {
	fn := reflect.ValueOf(CreateUSBBus)
	args := []reflect.Value{reflect.New(fn.Type().In(0)).Elem(), reflect.New(fn.Type().In(1)).Elem()}
	args[0].SetUint(uint64(handle))
	bus := reflect.New(fn.Type().In(1).Elem())
	bus.Elem().SetUint(uint64(busID))
	args[1].Set(bus)
	return fn.Call(args)[0].Bool()
}

func assertRejectionLogsAreLockSafe(t *testing.T, hlog *teardownRecordingHandler, want int) {
	t.Helper()
	hlog.mu.Lock()
	defer hlog.mu.Unlock()
	count := 0
	for i, record := range hlog.records {
		if strings.Contains(record.Message, "server mutation rejected") {
			count++
			if !hlog.lockFree[i] {
				t.Fatal("rejected-mutation log emitted while lifecycleMu held")
			}
		}
	}
	if count != want {
		t.Fatalf("rejected warning count=%d want=%d", count, want)
	}
}

func TestRejectedTypedMutationLoggingIsLockSafeAndDeduplicated(t *testing.T) {
	hw, hlog := newTeardownTestServer(t, 10060)
	h := addTestMouse(t, hw, 10060)
	hw.lifecycleMu.Lock()
	hw.state = serverCloseFailed
	hw.lifecycleMu.Unlock()

	if withActiveDeviceHandle(uintptr(h), func(*deviceHandleWrapper) bool { return true }) {
		t.Fatal("rejected mutation unexpectedly succeeded")
	}
	if withActiveDeviceHandle(uintptr(h), func(*deviceHandleWrapper) bool { return true }) {
		t.Fatal("rejected mutation unexpectedly succeeded on repeat")
	}
	assertRejectionLogsAreLockSafe(t, hlog, 1)
}

func TestRejectedTypedCreateLoggingIsLockSafeAndDeduplicated(t *testing.T) {
	hw, hlog := newTeardownTestServer(t, 10064)
	serverHandle := diagnosticServerHandle(t, hw)
	hw.lifecycleMu.Lock()
	hw.state = serverCloseFailed
	hw.lifecycleMu.Unlock()

	var first, second deviceHandle
	if createXbox360Device(serverHandle, &first, 10064, false, 0, 0, 0) || createXbox360Device(serverHandle, &second, 10064, false, 0, 0, 0) {
		t.Fatal("rejected typed creation unexpectedly succeeded")
	}
	assertRejectionLogsAreLockSafe(t, hlog, 1)
}

func TestRollbackFailureLoggingIsLockSafe(t *testing.T) {
	hw, hlog := newTeardownTestServer(t, 10065)
	serverHandle := diagnosticServerHandle(t, hw)
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		return api.LocalhostAttachment{}, errors.New("injected attach failure")
	}
	hw.ops.rollbackDevice = func(*virtualbus.VirtualBus, viiperusb.Device) error {
		return errors.New("injected rollback failure")
	}
	var handle deviceHandle
	if createXbox360Device(serverHandle, &handle, 10065, true, 0, 0, 0) {
		t.Fatal("creation unexpectedly succeeded")
	}
	hlog.mu.Lock()
	defer hlog.mu.Unlock()
	found := false
	for i, record := range hlog.records {
		if strings.Contains(record.Message, "failed to roll back logical device") {
			found = true
			if !hlog.lockFree[i] {
				t.Fatal("rollback error log emitted while lifecycleMu held")
			}
		}
	}
	if !found {
		t.Fatal("missing rollback error log")
	}
}

func TestRejectedCreateUSBBusLoggingIsLockSafe(t *testing.T) {
	hw, hlog := newTeardownTestServer(t, 10061)
	serverHandle := diagnosticServerHandle(t, hw)
	hw.lifecycleMu.Lock()
	hw.state = serverCloseFailed
	hw.lifecycleMu.Unlock()

	if callCreateUSBBusForTest(serverHandle, 10062) {
		t.Fatal("rejected bus creation unexpectedly succeeded")
	}
	assertRejectionLogsAreLockSafe(t, hlog, 1)
}

func TestRejectedAttachDetachLoggingIsLockSafe(t *testing.T) {
	hw, hlog := newTeardownTestServer(t, 10063)
	h := addTestMouse(t, hw, 10063)
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 5300}, nil
	}
	if attachUSBDeviceResult(uintptr(h)) != deviceAttachSuccess {
		t.Fatal("attach setup failed")
	}
	hw.lifecycleMu.Lock()
	hw.state = serverCloseFailed
	hw.lifecycleMu.Unlock()

	if got := attachUSBDeviceResult(uintptr(h)); got != deviceAttachInvalid {
		t.Fatalf("attach result=%v", got)
	}
	if got := detachUSBDeviceResult(uintptr(h)); got != deviceDetachInvalid {
		t.Fatalf("detach result=%v", got)
	}
	assertRejectionLogsAreLockSafe(t, hlog, 0)

	hlog.mu.Lock()
	for _, free := range hlog.lockFree {
		if !free {
			hlog.mu.Unlock()
			t.Fatal("attachment timing log emitted while lifecycleMu held")
		}
	}
	hlog.mu.Unlock()
}
