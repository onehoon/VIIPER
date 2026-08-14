package usb_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	srvusb "github.com/Alia5/VIIPER/internal/server/usb"
	rootusb "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/Alia5/VIIPER/virtualbus"
)

type blockingINDevice struct {
	entered    chan struct{}
	cancelled  chan struct{}
	release    chan struct{}
	exited     chan struct{}
	once       sync.Once
	descriptor rootusb.Descriptor
}

func (d *blockingINDevice) HandleTransfer(ctx context.Context, ep uint32, dir uint32, _ []byte) []byte {
	if dir != usbip.DirIn || ep != 1 {
		return nil
	}
	d.once.Do(func() { close(d.entered) })
	<-ctx.Done()
	close(d.cancelled)
	<-d.release
	close(d.exited)
	return nil
}

func (d *blockingINDevice) GetDescriptor() *rootusb.Descriptor { return &d.descriptor }

func (d *blockingINDevice) GetDeviceSpecificArgs() map[string]any { return nil }

func TestManagedDeviceDrainWaitsForAsyncINWorker(t *testing.T) {
	dev := &blockingINDevice{
		entered:   make(chan struct{}),
		cancelled: make(chan struct{}),
		release:   make(chan struct{}),
		exited:    make(chan struct{}),
		descriptor: rootusb.Descriptor{
			Device: rootusb.DeviceDescriptor{BNumConfigurations: 1, Speed: 3},
			Interfaces: []rootusb.InterfaceConfig{{
				Descriptor: rootusb.InterfaceDescriptor{BInterfaceNumber: 0, BNumEndpoints: 1},
				Endpoints:  []rootusb.EndpointDescriptor{{BEndpointAddress: 0x81, BMAttributes: 0x03, BInterval: 1}},
			}},
		},
	}
	server := srvusb.New(srvusb.ServerConfig{
		Addr: "127.0.0.1:0", ConnectionTimeout: time.Hour,
		DisableAutoBusCleanup: true, ManagedTransportLifecycle: true,
	}, slog.Default(), nil)
	bus, err := virtualbus.NewWithBusID(3050)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bus.Add(dev); err != nil {
		t.Fatal(err)
	}
	if err := server.AddBus(bus); err != nil {
		t.Fatal(err)
	}
	defer server.RemoveBus(3050)

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
	_, _ = request.Write(meta.USBBusID[:])
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
	if _, err := io.CopyN(io.Discard, conn, 92); err != nil {
		t.Fatal(err)
	}

	cmd := usbip.CmdSubmit{Basic: usbip.HeaderBasic{Command: usbip.CmdSubmitCode, Seqnum: 1, Dir: usbip.DirIn, Ep: 1}, TransferBufferLen: 8}
	if err := cmd.Write(conn); err != nil {
		t.Fatal(err)
	}
	select {
	case <-dev.entered:
	case <-time.After(time.Second):
		t.Fatal("async IN worker did not start")
	}

	drain := server.BeginDeviceDrain(dev)
	drainDone := make(chan struct{})
	go func() { drain.Wait(); close(drainDone) }()
	select {
	case <-dev.cancelled:
	case <-time.After(time.Second):
		t.Fatal("async IN worker did not observe cancellation")
	}
	select {
	case <-drainDone:
		t.Fatal("drain completed before async IN worker exited")
	default:
	}
	close(dev.release)
	select {
	case <-dev.exited:
	case <-time.After(time.Second):
		t.Fatal("async IN worker did not exit")
	}
	select {
	case <-drainDone:
	case <-time.After(time.Second):
		t.Fatal("drain did not complete after async IN worker exit")
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-serveDone; err != nil {
		t.Fatal(err)
	}
}
