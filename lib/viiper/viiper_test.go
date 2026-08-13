package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/cgo"
	"strings"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/device/mouse"
	"github.com/Alia5/VIIPER/internal/server/usb"
	viiperusb "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/Alia5/VIIPER/virtualbus"
)

func newLifecycleTestServer(t *testing.T, busID uint32) (*usbServerHandleWrapper, *virtualbus.VirtualBus) {
	t.Helper()
	s := usb.New(usb.ServerConfig{Addr: "127.0.0.1:0", BusCleanupTimeout: time.Hour}, slog.Default(), nil)
	b, err := virtualbus.NewWithBusID(busID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddBus(b); err != nil {
		t.Fatal(err)
	}
	hw := &usbServerHandleWrapper{s: s, state: serverActive, deviceHandles: map[uint32][]deviceHandle{busID: {}}, deviceHandleRecords: make(map[deviceHandle]*deviceHandleWrapper), ops: defaultServerOperations(), logger: slog.Default(), rejectionWarnings: make(map[string]bool)}
	t.Cleanup(func() {
		for _, id := range s.ListBuses() {
			_ = s.RemoveBus(id)
		}
		_ = s.Close()
	})
	return hw, b
}

func addTestBus(t *testing.T, hw *usbServerHandleWrapper, busID uint32) *virtualbus.VirtualBus {
	t.Helper()
	b, err := virtualbus.NewWithBusID(busID)
	if err != nil {
		t.Fatal(err)
	}
	if err := hw.s.AddBus(b); err != nil {
		t.Fatal(err)
	}
	hw.deviceHandles[busID] = nil
	return b
}

func addTestMouse(t *testing.T, hw *usbServerHandleWrapper, busID uint32) deviceHandle {
	t.Helper()
	d, err := mouse.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	h, ok := hw.createDeviceLocked(busID, d, false)
	if !ok {
		t.Fatal("device creation failed")
	}
	return h
}

func TestCreateDeviceRollsBackOnlyFailedDevice(t *testing.T) {
	hw, bus := newLifecycleTestServer(t, 9101)
	hw.ops.attachLocalhost = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) error {
		return errors.New("injected attach failure")
	}
	first, err := mouse.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bus.Add(first); err != nil {
		t.Fatal(err)
	}
	second, err := mouse.New(nil)
	if err != nil {
		t.Fatal(err)
	}

	hw.lifecycleMu.Lock()
	_, ok := hw.createDeviceLocked(9101, second, true)
	hw.lifecycleMu.Unlock()
	if ok {
		t.Fatal("creation succeeded despite injected attach failure")
	}
	if got := len(bus.Devices()); got != 1 {
		t.Fatalf("devices after rollback = %d, want 1", got)
	}
	if hw.s.GetBus(9101) == nil {
		t.Fatal("rollback removed caller-owned bus")
	}
}

func TestRollbackFailureTransitionsToCloseFailed(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9109)
	hw.lifecycleMu.Lock()
	hw.rollbackCreatedDeviceLocked(9109, 1, failingRemovalBus{}, mustNewTestMouse(t), "injected rollback failure")
	hw.lifecycleMu.Unlock()
	if hw.state != serverCloseFailed {
		t.Fatalf("state = %s, want close-failed", hw.state)
	}
}

func TestCreateWithoutAutoAttachDoesNotInvokeAttach(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9110)
	hw.ops.attachLocalhost = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) error {
		t.Fatal("auto-attach was invoked when disabled")
		return nil
	}
	hw.lifecycleMu.Lock()
	_ = addTestMouse(t, hw, 9110)
	hw.lifecycleMu.Unlock()
}

func TestCloseRetryFinalizesEachBusHandleExactlyOnce(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9102)
	addTestBus(t, hw, 9103)
	addTestBus(t, hw, 9104)
	hw.lifecycleMu.Lock()
	h1 := addTestMouse(t, hw, 9102)
	h2 := addTestMouse(t, hw, 9103)
	h3 := addTestMouse(t, hw, 9104)
	deleteCalls := map[deviceHandle]int{}
	hw.ops.deleteHandle = func(h cgo.Handle) { deleteCalls[deviceHandle(h)]++; h.Delete() }
	hw.ops.removeBus = func(s *usb.Server, id uint32) error {
		if id == 9103 {
			return errors.New("injected remove failure")
		}
		return s.RemoveBus(id)
	}
	if hw.closeLocked() {
		hw.lifecycleMu.Unlock()
		t.Fatal("close unexpectedly succeeded")
	}
	hw.lifecycleMu.Unlock()

	if hw.state != serverCloseFailed {
		t.Fatalf("state = %s, want close-failed", hw.state)
	}
	if hw.s.GetBus(9102) != nil {
		t.Fatal("successfully removed bus remained present")
	}
	if hw.s.GetBus(9103) == nil || hw.s.GetBus(9104) == nil {
		t.Fatal("unprocessed buses were not retained")
	}
	if deleteCalls[h1] != 1 || deleteCalls[h2] != 0 || deleteCalls[h3] != 0 {
		t.Fatalf("unexpected partial finalization counts: %#v", deleteCalls)
	}
	if _, ok := lookupDeviceIdentity(uintptr(h1)); ok {
		t.Fatal("finalized bus handle remained valid")
	}
	if !lookupIdentityExists(uintptr(h2)) || !lookupIdentityExists(uintptr(h3)) {
		t.Fatal("surviving handles were not available for diagnostics")
	}

	hw.lifecycleMu.Lock()
	hw.ops.removeBus = func(s *usb.Server, id uint32) error { return s.RemoveBus(id) }
	if !hw.closeLocked() {
		hw.lifecycleMu.Unlock()
		t.Fatal("close retry failed")
	}
	hw.lifecycleMu.Unlock()
	if deleteCalls[h1] != 1 || deleteCalls[h2] != 1 || deleteCalls[h3] != 1 {
		t.Fatalf("retry finalization counts: %#v", deleteCalls)
	}
}

func TestCloseTreatsConcurrentlyRemovedSnapshotBusAsFinalized(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9105)
	hw.lifecycleMu.Lock()
	h := addTestMouse(t, hw, 9105)
	deleteCalls := 0
	hw.ops.deleteHandle = func(h cgo.Handle) { deleteCalls++; h.Delete() }
	hw.ops.removeBus = func(s *usb.Server, id uint32) error {
		if err := s.RemoveBus(id); err != nil {
			return err
		}
		return errors.New("bus removed by cleanup")
	}
	if !hw.closeLocked() {
		hw.lifecycleMu.Unlock()
		t.Fatal("close treated an already removed bus as failure")
	}
	hw.lifecycleMu.Unlock()
	if deleteCalls != 1 {
		t.Fatalf("delete calls = %d, want 1", deleteCalls)
	}
	if _, ok := lookupDeviceIdentity(uintptr(h)); ok {
		t.Fatal("removed bus handle remained valid")
	}
}

func TestCloseRetryAfterUnderlyingServerCloseFailure(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9106)
	hw.lifecycleMu.Lock()
	h := addTestMouse(t, hw, 9106)
	closeCalls := 0
	hw.ops.close = func(*usb.Server) error {
		closeCalls++
		if closeCalls == 1 {
			return errors.New("injected server close failure")
		}
		return nil
	}
	if hw.closeLocked() {
		hw.lifecycleMu.Unlock()
		t.Fatal("close unexpectedly succeeded")
	}
	if hw.state != serverCloseFailed {
		hw.lifecycleMu.Unlock()
		t.Fatalf("state = %s, want close-failed", hw.state)
	}
	hw.lifecycleMu.Unlock()
	if _, ok := lookupDeviceIdentity(uintptr(h)); ok {
		t.Fatal("finalized handle remained valid after server close failure")
	}
	hw.lifecycleMu.Lock()
	if !hw.closeLocked() {
		hw.lifecycleMu.Unlock()
		t.Fatal("close retry failed")
	}
	hw.lifecycleMu.Unlock()
	if closeCalls != 2 {
		t.Fatalf("underlying close calls = %d, want 2", closeCalls)
	}
}

func TestCloseFailedRejectsFurtherDeviceCreation(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9107)
	hw.lifecycleMu.Lock()
	hw.state = serverCloseFailed
	if _, ok := hw.createDeviceLocked(9107, mustNewTestMouse(t), false); ok {
		hw.lifecycleMu.Unlock()
		t.Fatal("create succeeded while close-failed")
	}
	hw.lifecycleMu.Unlock()
}

func TestInFlightCreateAndCloseAreSerializedByLifecycleBoundary(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9108)
	attachStarted := make(chan struct{})
	releaseAttach := make(chan struct{})
	createDone := make(chan struct {
		h  deviceHandle
		ok bool
	}, 1)
	closeDone := make(chan bool, 1)
	hw.ops.attachLocalhost = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) error {
		close(attachStarted)
		<-releaseAttach
		return nil
	}
	hw.ops.close = func(*usb.Server) error { return nil }
	d := mustNewTestMouse(t)

	go func() {
		hw.lifecycleMu.Lock()
		h, ok := hw.createDeviceLocked(9108, d, true)
		hw.lifecycleMu.Unlock()
		createDone <- struct {
			h  deviceHandle
			ok bool
		}{h, ok}
	}()
	<-attachStarted
	go func() {
		hw.lifecycleMu.Lock()
		ok := hw.closeLocked()
		hw.lifecycleMu.Unlock()
		closeDone <- ok
	}()

	close(releaseAttach)
	created := <-createDone
	if !created.ok {
		t.Fatal("in-flight create failed")
	}
	if !<-closeDone {
		t.Fatal("close failed after in-flight create")
	}
	if hw.state != serverClosed {
		t.Fatalf("state = %s, want closed", hw.state)
	}
	if _, ok := lookupDeviceIdentity(uintptr(created.h)); ok {
		t.Fatal("handle from in-flight create survived close")
	}
}

func mustNewTestMouse(t *testing.T) *mouse.Mouse {
	t.Helper()
	d, err := mouse.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

type failingRemovalBus struct{}

func (failingRemovalBus) Remove(viiperusb.Device) error {
	return errors.New("injected rollback failure")
}

func TestExportedTypedAPIsDoNotUseRawCgoHandleValue(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("unable to locate test source")
	}
	entries, err := os.ReadDir(filepath.Dir(file))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" || entry.Name() == "viiper_test.go" {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(filepath.Dir(file), entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(contents), ".Value()") {
			t.Fatalf("raw cgo handle Value access in %s", entry.Name())
		}
	}
}

func lookupIdentityExists(raw uintptr) bool { _, ok := lookupDeviceIdentity(raw); return ok }
