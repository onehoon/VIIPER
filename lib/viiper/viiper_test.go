package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/device/mouse"
	"github.com/Alia5/VIIPER/internal/server/usb"
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
	hw := &usbServerHandleWrapper{
		s:                   s,
		state:               serverActive,
		deviceHandles:       map[uint32][]deviceHandle{busID: {}},
		deviceHandleRecords: make(map[deviceHandle]*deviceHandleWrapper),
		finalizationCounts:  make(map[deviceHandle]uint32),
		ops:                 defaultServerOperations(),
		logger:              slog.Default(),
		rejectionWarnings:   make(map[string]bool),
	}
	t.Cleanup(func() { _ = s.RemoveBus(busID) })
	return hw, b
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

func TestFinalizeBusInvalidatesHandlesExactlyOnce(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9102)
	d, err := mouse.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9102, d, false)
	if !ok {
		hw.lifecycleMu.Unlock()
		t.Fatal("creation failed")
	}
	hw.finalizeBusLocked(9102)
	hw.finalizeBusLocked(9102)
	count := hw.finalizationCounts[h]
	hw.lifecycleMu.Unlock()
	if count != 1 {
		t.Fatalf("finalization count = %d, want 1", count)
	}
	if _, ok := lookupDeviceIdentity(uintptr(h)); ok {
		t.Fatal("finalized handle remained valid")
	}
}
