package usb_test

import (
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/device/mouse"
	srvusb "github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/virtualbus"
)

func TestManagedServerKeepsEmptyBusAfterDeviceRemoval(t *testing.T) {
	server := srvusb.New(srvusb.ServerConfig{
		DisableAutoBusCleanup:     true,
		ManagedTransportLifecycle: true,
		BusCleanupTimeout:         time.Millisecond,
	}, slog.Default(), nil)
	bus, err := virtualbus.NewWithBusID(3041)
	if err != nil {
		t.Fatal(err)
	}
	defer server.RemoveBus(3041)
	dev, err := mouse.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bus.Add(dev); err != nil {
		t.Fatal(err)
	}
	if err := server.AddBus(bus); err != nil {
		t.Fatal(err)
	}
	meta := bus.GetAllDeviceMetas()[0].Meta
	deviceID := strconv.FormatUint(uint64(meta.DevID), 10)
	if err := server.RemoveDeviceByID(3041, deviceID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if server.GetBus(3041) == nil {
		t.Fatal("managed server removed caller-owned empty bus")
	}
}

func TestLegacyServerPerformsEmptyBusCleanup(t *testing.T) {
	server := srvusb.New(srvusb.ServerConfig{BusCleanupTimeout: time.Millisecond}, slog.Default(), nil)
	bus, err := virtualbus.NewWithBusID(3042)
	if err != nil {
		t.Fatal(err)
	}
	dev, err := mouse.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bus.Add(dev); err != nil {
		t.Fatal(err)
	}
	if err := server.AddBus(bus); err != nil {
		t.Fatal(err)
	}
	meta := bus.GetAllDeviceMetas()[0].Meta
	deviceID := strconv.FormatUint(uint64(meta.DevID), 10)
	if err := server.RemoveDeviceByID(3042, deviceID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && server.GetBus(3042) != nil {
		time.Sleep(time.Millisecond)
	}
	if server.GetBus(3042) != nil {
		t.Fatal("legacy server did not perform empty-bus cleanup")
	}
}
