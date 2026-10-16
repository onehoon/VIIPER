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

	"github.com/Alia5/VIIPER/device/dualsense"
	"github.com/Alia5/VIIPER/device/steamdeck"
	"github.com/Alia5/VIIPER/device/xbox360"
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

type overlappingINDevice struct {
	entered    chan struct{}
	cancelled  chan struct{}
	descriptor rootusb.Descriptor
}

func newOverlappingINDevice() *overlappingINDevice {
	return &overlappingINDevice{
		entered:   make(chan struct{}, 2),
		cancelled: make(chan struct{}, 2),
		descriptor: rootusb.Descriptor{
			Device: rootusb.DeviceDescriptor{BNumConfigurations: 1, Speed: 3},
			Interfaces: []rootusb.InterfaceConfig{{
				Descriptor: rootusb.InterfaceDescriptor{BInterfaceNumber: 0, BNumEndpoints: 1},
				Endpoints:  []rootusb.EndpointDescriptor{{BEndpointAddress: 0x81, BMAttributes: 0x03, BInterval: 0xff}},
			}},
		},
	}
}

func (d *overlappingINDevice) HandleTransfer(ctx context.Context, ep uint32, dir uint32, _ []byte) []byte {
	if dir != usbip.DirIn || ep != 1 {
		return nil
	}
	d.entered <- struct{}{}
	<-ctx.Done()
	d.cancelled <- struct{}{}
	return nil
}

func (d *overlappingINDevice) GetDescriptor() *rootusb.Descriptor { return &d.descriptor }

func (d *overlappingINDevice) GetDeviceSpecificArgs() map[string]any { return nil }

func TestAsyncINUnlinkCancelsActiveRequest(t *testing.T) {
	dev := newOverlappingINDevice()
	_, conn := startAsyncINTestSession(t, dev, 3061)
	defer conn.Close()

	const requestSeq = uint32(83)
	const unlinkSeq = uint32(93)
	cmd := usbip.CmdSubmit{Basic: usbip.HeaderBasic{Command: usbip.CmdSubmitCode, Seqnum: requestSeq, Dir: usbip.DirIn, Ep: 1}, TransferBufferLen: 8}
	if err := cmd.Write(conn); err != nil {
		t.Fatalf("submit IN sequence %d: %v", requestSeq, err)
	}
	select {
	case <-dev.entered:
	case <-time.After(time.Second):
		t.Fatal("active IN request did not enter HandleTransfer")
	}

	unlink := usbip.CmdUnlink{Basic: usbip.HeaderBasic{Command: usbip.CmdUnlinkCode, Seqnum: unlinkSeq, Dir: usbip.DirIn, Ep: 1}, UnlinkSeqnum: requestSeq}
	if err := unlink.Write(conn); err != nil {
		t.Fatalf("unlink active IN sequence %d: %v", requestSeq, err)
	}
	var response [48]byte
	if _, err := io.ReadFull(conn, response[:]); err != nil {
		t.Fatalf("read RET_UNLINK: %v", err)
	}
	if got := binary.BigEndian.Uint32(response[0:4]); got != usbip.RetUnlinkCode {
		t.Fatalf("response command = %#x, want RET_UNLINK", got)
	}
	if got := binary.BigEndian.Uint32(response[4:8]); got != unlinkSeq {
		t.Fatalf("RET_UNLINK sequence = %d, want %d", got, unlinkSeq)
	}
	if got := int32(binary.BigEndian.Uint32(response[20:24])); got != -104 {
		t.Fatalf("RET_UNLINK status = %d, want -ECONNRESET (-104)", got)
	}
	select {
	case <-dev.cancelled:
	case <-time.After(time.Second):
		t.Fatal("active IN request did not observe cancellation")
	}
	assertNoAsyncINResponse(t, conn)
}

func TestOverlappingAsyncINRequestsUnlinkIndependently(t *testing.T) {
	dev := newOverlappingINDevice()
	_, conn := startAsyncINTestSession(t, dev, 3058)

	for _, seq := range []uint32{81, 82} {
		cmd := usbip.CmdSubmit{Basic: usbip.HeaderBasic{Command: usbip.CmdSubmitCode, Seqnum: seq, Dir: usbip.DirIn, Ep: 1}, TransferBufferLen: 8}
		if err := cmd.Write(conn); err != nil {
			t.Fatalf("submit IN sequence %d: %v", seq, err)
		}
	}
	for range 2 {
		select {
		case <-dev.entered:
		case <-time.After(time.Second):
			t.Fatal("overlapping IN request did not enter HandleTransfer before the first was released")
		}
	}

	writeUnlink := func(unlinkSeq, seq uint32) {
		t.Helper()
		unlink := usbip.CmdUnlink{Basic: usbip.HeaderBasic{Command: usbip.CmdUnlinkCode, Seqnum: seq, Dir: usbip.DirIn, Ep: 1}, UnlinkSeqnum: unlinkSeq}
		if err := unlink.Write(conn); err != nil {
			t.Fatalf("unlink IN sequence %d: %v", unlinkSeq, err)
		}
		var response [48]byte
		if _, err := io.ReadFull(conn, response[:]); err != nil {
			t.Fatalf("read RET_UNLINK for sequence %d: %v", unlinkSeq, err)
		}
		if got := binary.BigEndian.Uint32(response[0:4]); got != usbip.RetUnlinkCode {
			t.Fatalf("response command = %#x, want RET_UNLINK", got)
		}
		if got := binary.BigEndian.Uint32(response[4:8]); got != seq {
			t.Fatalf("RET_UNLINK sequence = %d, want %d", got, seq)
		}
		if got := int32(binary.BigEndian.Uint32(response[20:24])); got != -104 {
			t.Fatalf("RET_UNLINK status = %d, want -ECONNRESET (-104)", got)
		}
		select {
		case <-dev.cancelled:
		case <-time.After(time.Second):
			t.Fatalf("IN sequence %d did not observe cancellation", unlinkSeq)
		}
	}

	writeUnlink(82, 91)
	assertNoAsyncINResponse(t, conn)
	select {
	case <-dev.cancelled:
		t.Fatal("unlinking the fallback request also cancelled the independent active request")
	case <-time.After(20 * time.Millisecond):
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close client connection: %v", err)
	}
	select {
	case <-dev.cancelled:
	case <-time.After(time.Second):
		t.Fatal("active request was not cancelled by stream teardown")
	}
}

type endpointCacheINDevice struct {
	mu         sync.Mutex
	calls      map[uint32]int
	descriptor rootusb.Descriptor
}

func newEndpointCacheINDevice() *endpointCacheINDevice {
	return &endpointCacheINDevice{
		calls: make(map[uint32]int),
		descriptor: rootusb.Descriptor{
			Device: rootusb.DeviceDescriptor{BNumConfigurations: 1, Speed: 3},
			Interfaces: []rootusb.InterfaceConfig{{
				Descriptor: rootusb.InterfaceDescriptor{BInterfaceNumber: 0, BNumEndpoints: 2},
				Endpoints: []rootusb.EndpointDescriptor{
					{BEndpointAddress: 0x81, BMAttributes: 0x03, BInterval: 2},
					{BEndpointAddress: 0x82, BMAttributes: 0x03, BInterval: 2},
				},
			}},
		},
	}
}

func (d *endpointCacheINDevice) HandleTransfer(ctx context.Context, ep uint32, dir uint32, _ []byte) []byte {
	if dir != usbip.DirIn || ep < 1 || ep > 2 {
		return nil
	}
	d.mu.Lock()
	d.calls[ep]++
	call := d.calls[ep]
	d.mu.Unlock()
	if call == 1 {
		return []byte{byte(0xa0 + ep)}
	}
	<-ctx.Done()
	return nil
}

func (d *endpointCacheINDevice) GetDescriptor() *rootusb.Descriptor { return &d.descriptor }

func (d *endpointCacheINDevice) GetDeviceSpecificArgs() map[string]any { return nil }

func TestAsyncINReportCacheIsIndependentPerEndpoint(t *testing.T) {
	dev := newEndpointCacheINDevice()
	_, conn := startAsyncINTestSession(t, dev, 3059)
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	defer conn.SetReadDeadline(time.Time{})

	readPair := func(sequences [2]uint32) map[uint32][]byte {
		t.Helper()
		responses := make(map[uint32][]byte, len(sequences))
		for range sequences {
			var header [48]byte
			if _, err := io.ReadFull(conn, header[:]); err != nil {
				t.Fatalf("read RET_SUBMIT: %v", err)
			}
			if got := binary.BigEndian.Uint32(header[0:4]); got != usbip.RetSubmitCode {
				t.Fatalf("response command = %#x, want RET_SUBMIT", got)
			}
			seq := binary.BigEndian.Uint32(header[4:8])
			if seq != sequences[0] && seq != sequences[1] {
				t.Fatalf("unexpected RET_SUBMIT sequence %d", seq)
			}
			if _, duplicate := responses[seq]; duplicate {
				t.Fatalf("duplicate RET_SUBMIT sequence %d", seq)
			}
			payloadLen := binary.BigEndian.Uint32(header[24:28])
			payload := make([]byte, payloadLen)
			if _, err := io.ReadFull(conn, payload); err != nil {
				t.Fatalf("read RET_SUBMIT sequence %d payload: %v", seq, err)
			}
			responses[seq] = payload
		}
		return responses
	}
	submit := func(seq, ep uint32) {
		t.Helper()
		cmd := usbip.CmdSubmit{Basic: usbip.HeaderBasic{Command: usbip.CmdSubmitCode, Seqnum: seq, Dir: usbip.DirIn, Ep: ep}, TransferBufferLen: 8}
		if err := cmd.Write(conn); err != nil {
			t.Fatalf("submit IN sequence %d on endpoint %d: %v", seq, ep, err)
		}
	}

	submit(101, 1)
	submit(102, 2)
	first := readPair([2]uint32{101, 102})
	if !bytes.Equal(first[101], []byte{0xa1}) || !bytes.Equal(first[102], []byte{0xa2}) {
		t.Fatalf("first endpoint reports = % x / % x, want a1 / a2", first[101], first[102])
	}

	submit(103, 1)
	submit(104, 2)
	replayed := readPair([2]uint32{103, 104})
	if !bytes.Equal(replayed[103], []byte{0xa1}) || !bytes.Equal(replayed[104], []byte{0xa2}) {
		t.Fatalf("cached endpoint reports = % x / % x, want a1 / a2", replayed[103], replayed[104])
	}
}

func TestAsyncINWorkersExitOnClientEOFWhileDeviceRemainsAttached(t *testing.T) {
	dev := newOverlappingINDevice()
	server, conn := startAsyncINTestSession(t, dev, 3060)
	for _, seq := range []uint32{111, 112} {
		cmd := usbip.CmdSubmit{Basic: usbip.HeaderBasic{Command: usbip.CmdSubmitCode, Seqnum: seq, Dir: usbip.DirIn, Ep: 1}, TransferBufferLen: 8}
		if err := cmd.Write(conn); err != nil {
			t.Fatalf("submit IN sequence %d: %v", seq, err)
		}
	}
	for range 2 {
		select {
		case <-dev.entered:
		case <-time.After(time.Second):
			t.Fatal("overlapping IN requests did not both enter HandleTransfer")
		}
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close imported client connection: %v", err)
	}
	for range 2 {
		select {
		case <-dev.cancelled:
		case <-time.After(time.Second):
			t.Fatal("client EOF did not cancel all in-flight IN requests")
		}
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- server.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("close server after client EOF: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("server close did not join workers after client EOF while the device remained attached")
	}
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

func TestXbox360AndDualSenseEdgeUSBIPINRegression(t *testing.T) {
	xbox, err := xbox360.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	edge, err := dualsense.NewEdge(nil)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name         string
		dev          rootusb.Device
		bus          uint32
		endpoint     uint32
		addr         byte
		interval     byte
		reportLen    int
		reportID     byte
		update       func()
		check        func(*testing.T, []byte)
		checkNeutral func(*testing.T, []byte)
	}{
		{
			name: "xbox360", dev: xbox, bus: 3056, endpoint: 1,
			addr: 0x81, interval: 4, reportLen: 20, reportID: 0x00,
			update: func() {
				xbox.UpdateInputState(xbox360.InputState{Buttons: xbox360.ButtonA})
			},
			check: func(t *testing.T, report []byte) {
				if report[3]&0x10 == 0 {
					t.Fatal("Xbox360 A button missing from report")
				}
			},
			checkNeutral: func(t *testing.T, report []byte) {
				if report[2] != 0 || report[3] != 0 {
					t.Fatalf("initial Xbox360 buttons = %02x %02x, want neutral", report[2], report[3])
				}
			},
		},
		{
			name: "dualsenseedge", dev: edge, bus: 3057, endpoint: 4,
			addr: 0x84, interval: 2, reportLen: 64, reportID: 0x01,
			update: func() {
				edge.UpdateInputState(&dualsense.InputState{GyroX: 0x1234})
			},
			check: func(t *testing.T, report []byte) {
				if got := binary.LittleEndian.Uint16(report[16:18]); got != 0x1234 {
					t.Fatalf("DualSense Edge gyro X = %#x, want 0x1234", got)
				}
			},
			checkNeutral: func(t *testing.T, report []byte) {
				if !bytes.Equal(report[1:5], []byte{0x80, 0x80, 0x80, 0x80}) {
					t.Fatalf("initial DualSense Edge stick axes = % x, want centered", report[1:5])
				}
				if got := binary.LittleEndian.Uint16(report[16:18]); got != 0 {
					t.Fatalf("initial DualSense Edge gyro X = %#x, want neutral", got)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := tc.dev.(interface {
				InterruptINWaitBudget(uint32) time.Duration
			}); ok {
				t.Fatal("non-Deck device unexpectedly overrides the interrupt IN wait budget")
			}

			foundEndpoint := false
			for _, iface := range tc.dev.GetDescriptor().Interfaces {
				for _, endpoint := range iface.Endpoints {
					if endpoint.BEndpointAddress == tc.addr {
						foundEndpoint = true
						if endpoint.BInterval != tc.interval {
							t.Fatalf("endpoint %#x interval = %d, want %d", tc.addr, endpoint.BInterval, tc.interval)
						}
					}
				}
			}
			if !foundEndpoint {
				t.Fatalf("IN endpoint %#x not found", tc.addr)
			}

			_, conn := startAsyncINTestSession(t, tc.dev, tc.bus)
			if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			defer conn.SetReadDeadline(time.Time{})

			read := func(seq uint32) []byte {
				t.Helper()
				cmd := usbip.CmdSubmit{
					Basic: usbip.HeaderBasic{
						Command: usbip.CmdSubmitCode,
						Seqnum:  seq,
						Dir:     usbip.DirIn,
						Ep:      tc.endpoint,
					},
					TransferBufferLen: 64,
				}
				if err := cmd.Write(conn); err != nil {
					t.Fatal(err)
				}
				report := readSteamDeckINResponse(t, conn, seq)
				if len(report) != tc.reportLen {
					t.Fatalf("%s report length = %d, want %d", tc.name, len(report), tc.reportLen)
				}
				if report[0] != tc.reportID {
					t.Fatalf("%s report ID = %#x, want %#x", tc.name, report[0], tc.reportID)
				}
				return report
			}

			tc.checkNeutral(t, read(1))
			tc.update()
			tc.check(t, read(2)) // Updated state reaches the host.
			tc.check(t, read(3)) // No update: preserve each device's existing latest-state fallback.
		})
	}
}

type observedSteamDeckINDevice struct {
	*steamdeck.SteamDeck
	entered chan struct{}
}

func (d *observedSteamDeckINDevice) HandleTransfer(ctx context.Context, ep uint32, dir uint32, out []byte) []byte {
	if dir == usbip.DirIn && ep == 3 {
		select {
		case d.entered <- struct{}{}:
		default:
		}
	}
	return d.SteamDeck.HandleTransfer(ctx, ep, dir, out)
}

func readSteamDeckINResponse(t *testing.T, conn net.Conn, seq uint32) []byte {
	t.Helper()
	var header [48]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		t.Fatalf("read Steam Deck RET_SUBMIT: %v", err)
	}
	if got := binary.BigEndian.Uint32(header[0:4]); got != usbip.RetSubmitCode {
		t.Fatalf("response command = %#x, want RET_SUBMIT", got)
	}
	if got := binary.BigEndian.Uint32(header[4:8]); got != seq {
		t.Fatalf("RET_SUBMIT sequence = %d, want %d", got, seq)
	}
	payloadLen := binary.BigEndian.Uint32(header[24:28])
	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(conn, payload); err != nil {
		t.Fatalf("read Steam Deck RET_SUBMIT payload: %v", err)
	}
	return payload
}

func TestSteamDeckAsyncINReturnsFreshStateForPendingRequest(t *testing.T) {
	deck, err := steamdeck.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	dev := &observedSteamDeckINDevice{SteamDeck: deck, entered: make(chan struct{}, 4)}
	_, conn := startAsyncINTestSession(t, dev, 3054)

	first := usbip.CmdSubmit{Basic: usbip.HeaderBasic{Command: usbip.CmdSubmitCode, Seqnum: 31, Dir: usbip.DirIn, Ep: 3}, TransferBufferLen: steamdeck.InputReportLen}
	if err := first.Write(conn); err != nil {
		t.Fatal(err)
	}
	select {
	case <-dev.entered:
	case <-time.After(time.Second):
		t.Fatal("Steam Deck async IN worker did not reach the device")
	}
	initial := readSteamDeckINResponse(t, conn, 31)
	if len(initial) != steamdeck.InputReportLen {
		t.Fatalf("initial report length = %d, want %d", len(initial), steamdeck.InputReportLen)
	}
	if got := binary.LittleEndian.Uint32(initial[4:8]); got != 0 {
		t.Fatalf("initial frame = %d, want neutral frame 0", got)
	}

	second := usbip.CmdSubmit{Basic: usbip.HeaderBasic{Command: usbip.CmdSubmitCode, Seqnum: 32, Dir: usbip.DirIn, Ep: 3}, TransferBufferLen: steamdeck.InputReportLen}
	if err := second.Write(conn); err != nil {
		t.Fatal(err)
	}
	select {
	case <-dev.entered:
	case <-time.After(time.Second):
		t.Fatal("pending Steam Deck async IN worker did not reach the device")
	}
	dev.UpdateInputState(&steamdeck.InputState{Pitch: 1234, LStickX: -2345})

	latest := readSteamDeckINResponse(t, conn, 32)
	if len(latest) != steamdeck.InputReportLen {
		t.Fatalf("updated report length = %d, want %d", len(latest), steamdeck.InputReportLen)
	}
	if got := binary.LittleEndian.Uint32(latest[4:8]); got != 1 {
		t.Fatalf("updated frame = %d, want latest state frame 1", got)
	}
	if got := int16(binary.LittleEndian.Uint16(latest[30:32])); got != 1234 {
		t.Fatalf("updated gyro pitch = %d, want 1234", got)
	}
	if got := int16(binary.LittleEndian.Uint16(latest[48:50])); got != -2345 {
		t.Fatalf("updated stick X = %d, want -2345", got)
	}
}

func TestManagedDrainCancelsPendingSteamDeckINWithoutLateCompletion(t *testing.T) {
	deck, err := steamdeck.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	dev := &observedSteamDeckINDevice{SteamDeck: deck, entered: make(chan struct{}, 4)}
	server, conn := startAsyncINTestSession(t, dev, 3055)

	first := usbip.CmdSubmit{Basic: usbip.HeaderBasic{Command: usbip.CmdSubmitCode, Seqnum: 41, Dir: usbip.DirIn, Ep: 3}, TransferBufferLen: steamdeck.InputReportLen}
	if err := first.Write(conn); err != nil {
		t.Fatal(err)
	}
	select {
	case <-dev.entered:
	case <-time.After(time.Second):
		t.Fatal("initial Steam Deck IN did not reach device")
	}
	readSteamDeckINResponse(t, conn, 41)

	second := usbip.CmdSubmit{Basic: usbip.HeaderBasic{Command: usbip.CmdSubmitCode, Seqnum: 42, Dir: usbip.DirIn, Ep: 3}, TransferBufferLen: steamdeck.InputReportLen}
	if err := second.Write(conn); err != nil {
		t.Fatal(err)
	}
	select {
	case <-dev.entered:
	case <-time.After(time.Second):
		t.Fatal("pending Steam Deck IN did not reach device")
	}

	drain := server.BeginDeviceDrain(dev)
	drainDone := make(chan struct{})
	go func() { drain.Wait(); close(drainDone) }()
	select {
	case <-drainDone:
	case <-time.After(time.Second):
		t.Fatal("managed drain did not cancel and join pending Steam Deck IN")
	}
	if err := conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var response [48]byte
	n, readErr := io.ReadFull(conn, response[:])
	if n != 0 || readErr == nil || (!errors.Is(readErr, io.EOF) && !isTimeoutError(readErr)) {
		t.Fatalf("pending Steam Deck IN produced a late response or unexpected stream result: bytes=%d err=%v", n, readErr)
	}
}

func isTimeoutError(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
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
