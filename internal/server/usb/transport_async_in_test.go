package usb_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
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

type immediateINDevice struct {
	entered    chan struct{}
	enterOnce  sync.Once
	response   []byte
	descriptor rootusb.Descriptor
}

func newImmediateINDevice(response []byte) *immediateINDevice {
	return &immediateINDevice{
		entered:  make(chan struct{}),
		response: response,
		descriptor: rootusb.Descriptor{
			Device: rootusb.DeviceDescriptor{BNumConfigurations: 1, Speed: 3},
			Interfaces: []rootusb.InterfaceConfig{{
				Descriptor: rootusb.InterfaceDescriptor{BInterfaceNumber: 0, BNumEndpoints: 1},
				Endpoints:  []rootusb.EndpointDescriptor{{BEndpointAddress: 0x81, BMAttributes: 0x03, BInterval: 0xff}},
			}},
		},
	}
}

func (d *immediateINDevice) HandleTransfer(_ context.Context, ep uint32, dir uint32, _ []byte) []byte {
	if dir != usbip.DirIn || ep != 1 {
		return nil
	}
	d.enterOnce.Do(func() { close(d.entered) })
	return d.response
}

func (d *immediateINDevice) GetDescriptor() *rootusb.Descriptor { return &d.descriptor }

func (d *immediateINDevice) GetDeviceSpecificArgs() map[string]any { return nil }

const usbipImportDeviceReplySize = 312

func startAsyncINTestSession(t *testing.T, dev rootusb.Device, busID uint32) (*srvusb.Server, net.Conn) {
	t.Helper()
	server := srvusb.New(srvusb.ServerConfig{
		Addr: "127.0.0.1:0", ConnectionTimeout: time.Hour,
		DisableAutoBusCleanup: true, ManagedTransportLifecycle: true,
	}, slog.Default(), nil)
	bus, err := virtualbus.NewWithBusID(busID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bus.Add(dev); err != nil {
		t.Fatal(err)
	}
	if err := server.AddBus(bus); err != nil {
		t.Fatal(err)
	}

	serveDone := make(chan error, 1)
	go func() { serveDone <- server.ListenAndServe() }()
	select {
	case <-server.Ready():
	case <-time.After(time.Second):
		t.Fatal("USB/IP server did not become ready")
	}
	conn, err := net.Dial("tcp", server.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		if err := server.Close(); err != nil {
			t.Errorf("close test server: %v", err)
		}
		select {
		case err := <-serveDone:
			if err != nil {
				t.Errorf("ListenAndServe returned %v", err)
			}
		case <-time.After(time.Second):
			t.Error("test server did not stop")
		}
		if err := server.RemoveBus(busID); err != nil {
			t.Errorf("remove test bus: %v", err)
		}
	})

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
	if _, err := io.CopyN(io.Discard, conn, usbipImportDeviceReplySize); err != nil {
		t.Fatal(err)
	}
	return server, conn
}

func waitForINTransfer(t *testing.T, dev *immediateINDevice) {
	t.Helper()
	select {
	case <-dev.entered:
	case <-time.After(time.Second):
		t.Fatal("async IN worker did not call the device")
	}
}

func assertNoAsyncINResponse(t *testing.T, conn net.Conn) {
	t.Helper()
	var response [48]byte
	if err := conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	n, err := io.ReadFull(conn, response[:])
	if err == nil || n != 0 {
		t.Fatalf("unexpected async IN response: bytes=%d command=%#x err=%v", n, binary.BigEndian.Uint32(response[:4]), err)
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("waiting for async IN response returned %v, want read timeout", err)
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
}

func TestNoDataAsyncINWaitsForUnlink(t *testing.T) {
	dev := newImmediateINDevice(nil)
	_, conn := startAsyncINTestSession(t, dev, 3051)
	cmd := usbip.CmdSubmit{Basic: usbip.HeaderBasic{Command: usbip.CmdSubmitCode, Seqnum: 1, Dir: usbip.DirIn, Ep: 1}, TransferBufferLen: 8}
	if err := cmd.Write(conn); err != nil {
		t.Fatal(err)
	}
	waitForINTransfer(t, dev)
	assertNoAsyncINResponse(t, conn)

	unlink := usbip.CmdUnlink{Basic: usbip.HeaderBasic{Command: usbip.CmdUnlinkCode, Seqnum: 2, Dir: usbip.DirIn, Ep: 1}, UnlinkSeqnum: 1}
	if err := unlink.Write(conn); err != nil {
		t.Fatal(err)
	}
	var response [48]byte
	if _, err := io.ReadFull(conn, response[:]); err != nil {
		t.Fatalf("read RET_UNLINK: %v", err)
	}
	if got := binary.BigEndian.Uint32(response[0:4]); got != usbip.RetUnlinkCode {
		t.Fatalf("response command = %#x, want RET_UNLINK", got)
	}
	if got := binary.BigEndian.Uint32(response[4:8]); got != 2 {
		t.Fatalf("RET_UNLINK sequence = %d, want 2", got)
	}
	if got := int32(binary.BigEndian.Uint32(response[20:24])); got != -104 {
		t.Fatalf("RET_UNLINK status = %d, want -ECONNRESET (-104)", got)
	}
	assertNoAsyncINResponse(t, conn)
}

func TestAsyncINDataResponsePreservesSequenceLengthAndBytes(t *testing.T) {
	want := []byte{0x11, 0x22, 0x33}
	dev := newImmediateINDevice(want)
	_, conn := startAsyncINTestSession(t, dev, 3052)
	cmd := usbip.CmdSubmit{Basic: usbip.HeaderBasic{Command: usbip.CmdSubmitCode, Seqnum: 7, Dir: usbip.DirIn, Ep: 1}, TransferBufferLen: 8}
	if err := cmd.Write(conn); err != nil {
		t.Fatal(err)
	}
	waitForINTransfer(t, dev)

	var response [48]byte
	if _, err := io.ReadFull(conn, response[:]); err != nil {
		t.Fatalf("read RET_SUBMIT: %v", err)
	}
	if got := binary.BigEndian.Uint32(response[0:4]); got != usbip.RetSubmitCode {
		t.Fatalf("response command = %#x, want RET_SUBMIT", got)
	}
	if got := binary.BigEndian.Uint32(response[4:8]); got != 7 {
		t.Fatalf("RET_SUBMIT sequence = %d, want 7", got)
	}
	if got := binary.BigEndian.Uint32(response[24:28]); got != uint32(len(want)) {
		t.Fatalf("RET_SUBMIT actual length = %d, want %d", got, len(want))
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read RET_SUBMIT payload: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("RET_SUBMIT payload = % x, want % x", got, want)
	}
}

func TestManagedDeviceDrainReleasesParkedNoDataAsyncINWorker(t *testing.T) {
	dev := newImmediateINDevice(nil)
	server, conn := startAsyncINTestSession(t, dev, 3053)
	cmd := usbip.CmdSubmit{Basic: usbip.HeaderBasic{Command: usbip.CmdSubmitCode, Seqnum: 9, Dir: usbip.DirIn, Ep: 1}, TransferBufferLen: 8}
	if err := cmd.Write(conn); err != nil {
		t.Fatal(err)
	}
	waitForINTransfer(t, dev)
	assertNoAsyncINResponse(t, conn)

	drain := server.BeginDeviceDrain(dev)
	drainDone := make(chan struct{})
	go func() { drain.Wait(); close(drainDone) }()
	select {
	case <-drainDone:
	case <-time.After(time.Second):
		t.Fatal("managed drain did not release the parked no-data IN worker")
	}
}

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
	server, conn := startAsyncINTestSession(t, dev, 3050)

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
}
