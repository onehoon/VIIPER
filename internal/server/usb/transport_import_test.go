package usb_test

import (
	"bytes"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/device/mouse"
	srvusb "github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/Alia5/VIIPER/virtualbus"
)

func TestManagedImportRepliesOnlyForBoundDevice(t *testing.T) {
	server := srvusb.New(srvusb.ServerConfig{
		Addr: "127.0.0.1:0", ConnectionTimeout: time.Hour, ManagedTransportLifecycle: true,
	}, slog.Default(), nil)
	bus, err := virtualbus.NewWithBusID(3021)
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
	defer func() {
		if err := server.RemoveBus(3021); err != nil {
			t.Errorf("remove test bus: %v", err)
		}
	}()
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.ListenAndServe() }()
	<-server.Ready()

	conn, err := net.Dial("tcp", server.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	meta := bus.GetAllDeviceMetas()[0].Meta
	var request bytes.Buffer
	if err := (&usbip.MgmtHeader{Version: usbip.Version, Command: usbip.OpReqImport}).Write(&request); err != nil {
		t.Fatal(err)
	}
	request.Write(meta.USBBusID[:])
	if _, err := conn.Write(request.Bytes()); err != nil {
		t.Fatal(err)
	}
	var reply [8]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		t.Fatal(err)
	}
	if got := binary.BigEndian.Uint16(reply[2:4]); got != usbip.OpRepImport {
		t.Fatalf("reply command = %#x, want OP_REP_IMPORT", got)
	}
	if got := binary.BigEndian.Uint32(reply[4:8]); got != 0 {
		t.Fatalf("reply status = %d, want success", got)
	}

	drain := server.BeginDeviceDrain(dev)
	done := make(chan struct{})
	go func() { drain.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("managed import drain did not complete")
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-serveDone; err != nil {
		t.Fatal(err)
	}
}
