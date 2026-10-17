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

type noReplayEndpointINDevice struct {
	mu         sync.Mutex
	calls      map[uint32]int
	timeouts   chan uint32
	descriptor rootusb.Descriptor
}

func newNoReplayEndpointINDevice() *noReplayEndpointINDevice {
	return &noReplayEndpointINDevice{
		calls:    make(map[uint32]int),
		timeouts: make(chan uint32, 64),
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

func (d *noReplayEndpointINDevice) HandleTransfer(ctx context.Context, ep uint32, dir uint32, _ []byte) []byte {
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
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		select {
		case d.timeouts <- ep:
		default:
		}
	}
	return nil
}

func (d *noReplayEndpointINDevice) GetDescriptor() *rootusb.Descriptor { return &d.descriptor }

func (d *noReplayEndpointINDevice) GetDeviceSpecificArgs() map[string]any { return nil }

func TestAsyncINDoesNotReplayReportsAcrossEndpointTimeouts(t *testing.T) {
	dev := newNoReplayEndpointINDevice()
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
	timeoutCounts := map[uint32]int{1: 0, 2: 0}
	waitForEndpointTimeouts(t, dev.timeouts, timeoutCounts, 1)
	assertNoAsyncINResponse(t, conn)
	waitForEndpointTimeouts(t, dev.timeouts, timeoutCounts, 2)
	assertNoAsyncINResponse(t, conn)

	for _, unlink := range []usbip.CmdUnlink{
		{Basic: usbip.HeaderBasic{Command: usbip.CmdUnlinkCode, Seqnum: 201, Dir: usbip.DirIn, Ep: 1}, UnlinkSeqnum: 103},
		{Basic: usbip.HeaderBasic{Command: usbip.CmdUnlinkCode, Seqnum: 202, Dir: usbip.DirIn, Ep: 2}, UnlinkSeqnum: 104},
	} {
		if err := unlink.Write(conn); err != nil {
			t.Fatalf("unlink pending IN request: %v", err)
		}
	}
	assertAsyncINUnlinkResponses(t, conn, 201, 202)
	assertNoAsyncINResponse(t, conn)
}

func waitForEndpointTimeouts(t *testing.T, timeouts <-chan uint32, counts map[uint32]int, want int) {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for counts[1] < want || counts[2] < want {
		select {
		case ep := <-timeouts:
			if _, ok := counts[ep]; ok {
				counts[ep]++
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for %d deadline expirations per endpoint; got %v", want, counts)
		}
	}
}

type largeINDeviceWithoutReplay struct {
	mu         sync.Mutex
	calls      int
	response   []byte
	fresh      chan []byte
	timeouts   chan struct{}
	descriptor rootusb.Descriptor
}

func newLargeINDeviceWithoutReplay() *largeINDeviceWithoutReplay {
	response := make([]byte, 100)
	for i := range response {
		response[i] = byte(i + 1)
	}
	return &largeINDeviceWithoutReplay{
		response: response,
		fresh:    make(chan []byte, 1),
		timeouts: make(chan struct{}, 64),
		descriptor: rootusb.Descriptor{
			Device: rootusb.DeviceDescriptor{BNumConfigurations: 1, Speed: 3},
			Interfaces: []rootusb.InterfaceConfig{{
				Descriptor: rootusb.InterfaceDescriptor{BInterfaceNumber: 0, BNumEndpoints: 1},
				Endpoints:  []rootusb.EndpointDescriptor{{BEndpointAddress: 0x81, BMAttributes: 0x03, BInterval: 2}},
			}},
		},
	}
}

func (d *largeINDeviceWithoutReplay) HandleTransfer(ctx context.Context, ep uint32, dir uint32, _ []byte) []byte {
	if dir != usbip.DirIn || ep != 1 {
		return nil
	}
	d.mu.Lock()
	d.calls++
	call := d.calls
	d.mu.Unlock()
	if call == 1 {
		return append([]byte(nil), d.response...)
	}
	select {
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			select {
			case d.timeouts <- struct{}{}:
			default:
			}
		}
		return nil
	case report := <-d.fresh:
		return report
	}
}

func (d *largeINDeviceWithoutReplay) GetDescriptor() *rootusb.Descriptor { return &d.descriptor }

func (d *largeINDeviceWithoutReplay) GetDeviceSpecificArgs() map[string]any { return nil }

func TestLegacyCustomDeviceLargeINPayloadWithoutIdleReplay(t *testing.T) {
	dev := newLargeINDeviceWithoutReplay()
	_, conn := startAsyncINTestSession(t, dev, 3062)
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	defer conn.SetReadDeadline(time.Time{})
	submit := func(seq uint32) {
		t.Helper()
		cmd := usbip.CmdSubmit{Basic: usbip.HeaderBasic{Command: usbip.CmdSubmitCode, Seqnum: seq, Dir: usbip.DirIn, Ep: 1}, TransferBufferLen: 128}
		if err := cmd.Write(conn); err != nil {
			t.Fatalf("submit legacy IN sequence %d: %v", seq, err)
		}
	}
	readReport := func(seq uint32, want []byte) {
		t.Helper()
		got := readAsyncINResponse(t, conn, seq)
		if len(got) != len(want) || !bytes.Equal(got, want) {
			t.Fatalf("response %d payload mismatch: got %d bytes, want %d", seq, len(got), len(want))
		}
	}

	submit(121)
	readReport(121, dev.response)

	submit(122)
	waitForDeadlineCount(t, dev.timeouts, 1)
	assertNoAsyncINResponse(t, conn)
	waitForDeadlineCount(t, dev.timeouts, 3)
	assertNoAsyncINResponse(t, conn)

	fresh := bytes.Repeat([]byte{0xd7}, 100)
	dev.fresh <- fresh
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	readReport(122, fresh)
}

func waitForDeadlineCount(t *testing.T, deadlines <-chan struct{}, want int) {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for i := 0; i < want; i++ {
		select {
		case <-deadlines:
		case <-timer.C:
			t.Fatalf("timed out waiting for %d IN attempt deadline expirations", want)
		}
	}
}

func assertAsyncINUnlinkResponses(t *testing.T, conn net.Conn, unlinkSeqs ...uint32) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	defer conn.SetReadDeadline(time.Time{})
	want := make(map[uint32]bool, len(unlinkSeqs))
	for _, seq := range unlinkSeqs {
		want[seq] = true
	}
	for range unlinkSeqs {
		var response [48]byte
		if _, err := io.ReadFull(conn, response[:]); err != nil {
			t.Fatalf("read RET_UNLINK: %v", err)
		}
		if got := binary.BigEndian.Uint32(response[0:4]); got != usbip.RetUnlinkCode {
			t.Fatalf("response command = %#x, want RET_UNLINK", got)
		}
		seq := binary.BigEndian.Uint32(response[4:8])
		if !want[seq] {
			t.Fatalf("unexpected or duplicate RET_UNLINK sequence %d", seq)
		}
		delete(want, seq)
		if got := int32(binary.BigEndian.Uint32(response[20:24])); got != -104 {
			t.Fatalf("RET_UNLINK sequence %d status = %d, want -ECONNRESET (-104)", seq, got)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing RET_UNLINK responses for sequences %v", want)
	}
}

type overlappingBufferedINDevice struct {
	entered    chan []byte
	release    chan struct{}
	mu         sync.Mutex
	next       byte
	descriptor rootusb.Descriptor
}

func newOverlappingBufferedINDevice() *overlappingBufferedINDevice {
	return &overlappingBufferedINDevice{
		entered: make(chan []byte, 2),
		release: make(chan struct{}),
		descriptor: rootusb.Descriptor{
			Device: rootusb.DeviceDescriptor{BNumConfigurations: 1, Speed: 3},
			Interfaces: []rootusb.InterfaceConfig{{
				Descriptor: rootusb.InterfaceDescriptor{BInterfaceNumber: 0, BNumEndpoints: 1},
				Endpoints:  []rootusb.EndpointDescriptor{{BEndpointAddress: 0x81, BMAttributes: 0x03, BInterval: 0xff}},
			}},
		},
	}
}

func (d *overlappingBufferedINDevice) HandleTransfer(context.Context, uint32, uint32, []byte) []byte {
	return nil
}

func (d *overlappingBufferedINDevice) HandleInterruptIN(ctx context.Context, ep uint32, dst []byte) []byte {
	if ep != 1 {
		return nil
	}
	d.entered <- dst
	select {
	case <-ctx.Done():
		return nil
	case <-d.release:
		d.mu.Lock()
		d.next++
		marker := d.next
		d.mu.Unlock()
		dst[0] = marker
		return dst[:1]
	}
}

func (d *overlappingBufferedINDevice) GetDescriptor() *rootusb.Descriptor { return &d.descriptor }

func (d *overlappingBufferedINDevice) GetDeviceSpecificArgs() map[string]any { return nil }

func TestOverlappingBufferedINRequestsUseIndependentWorkerAndFallbackScratch(t *testing.T) {
	dev := newOverlappingBufferedINDevice()
	_, conn := startAsyncINTestSession(t, dev, 3063)
	for _, seq := range []uint32{131, 132} {
		cmd := usbip.CmdSubmit{Basic: usbip.HeaderBasic{Command: usbip.CmdSubmitCode, Seqnum: seq, Dir: usbip.DirIn, Ep: 1}, TransferBufferLen: 64}
		if err := cmd.Write(conn); err != nil {
			t.Fatalf("submit buffered IN sequence %d: %v", seq, err)
		}
	}
	var first, second []byte
	for i := 0; i < 2; i++ {
		select {
		case report := <-dev.entered:
			if i == 0 {
				first = report
			} else {
				second = report
			}
		case <-time.After(time.Second):
			t.Fatal("overlapping optional IN method calls did not both start")
		}
	}
	if len(first) < 64 || len(second) < 64 || &first[0] == &second[0] {
		t.Fatal("overlapping worker and fallback calls did not receive independent 64-byte scratch buffers")
	}
	close(dev.release)
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	seenSeq := make(map[uint32]bool, 2)
	seenMarkers := make(map[byte]bool, 2)
	for range 2 {
		var header [48]byte
		if _, err := io.ReadFull(conn, header[:]); err != nil {
			t.Fatalf("read buffered IN RET_SUBMIT: %v", err)
		}
		if got := binary.BigEndian.Uint32(header[0:4]); got != usbip.RetSubmitCode {
			t.Fatalf("response command = %#x, want RET_SUBMIT", got)
		}
		seq := binary.BigEndian.Uint32(header[4:8])
		if (seq != 131 && seq != 132) || seenSeq[seq] {
			t.Fatalf("unexpected or duplicate response sequence %d", seq)
		}
		seenSeq[seq] = true
		if got := binary.BigEndian.Uint32(header[24:28]); got != 1 {
			t.Fatalf("response %d actual length = %d, want 1", seq, got)
		}
		var payload [1]byte
		if _, err := io.ReadFull(conn, payload[:]); err != nil {
			t.Fatalf("read response %d payload: %v", seq, err)
		}
		if payload[0] == 0 || seenMarkers[payload[0]] {
			t.Fatalf("response %d marker = %d, want unique nonzero marker", seq, payload[0])
		}
		seenMarkers[payload[0]] = true
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

			submit := func(seq uint32) {
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
			}
			read := func(seq uint32) []byte {
				t.Helper()
				submit(seq)
				report := readAsyncINResponse(t, conn, seq)
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
			updated := read(2)
			tc.check(t, updated) // Updated state reaches the host.
			if tc.name == "xbox360" {
				submit(3)
				assertNoAsyncINResponse(t, conn)
			} else {
				idle := read(3) // Devices with a timeout report preserve their own latest-state behavior.
				tc.check(t, idle)
				if tc.name == "dualsenseedge" {
					if idle[7] != updated[7]+1 {
						t.Fatalf("DualSense Edge timeout sequence = %d, want next after %d", idle[7], updated[7])
					}
					updatedTimestamp := binary.LittleEndian.Uint32(updated[28:32])
					idleTimestamp := binary.LittleEndian.Uint32(idle[28:32])
					if idleTimestamp <= updatedTimestamp {
						t.Fatalf("DualSense Edge timeout timestamp = %d, want greater than %d", idleTimestamp, updatedTimestamp)
					}
				}
			}
		})
	}
}

type observedXbox360INDevice struct {
	*xbox360.Xbox360
	waitBudget time.Duration
	timeouts   chan struct{}
	entered    chan struct{}
	overlap    chan struct{}
	mu         sync.Mutex
	inFlight   int
}

func (d *observedXbox360INDevice) InterruptINWaitBudget(ep uint32) time.Duration {
	if ep == 1 {
		return d.waitBudget
	}
	return 0
}

func (d *observedXbox360INDevice) HandleInterruptIN(ctx context.Context, ep uint32, dst []byte) []byte {
	d.mu.Lock()
	d.inFlight++
	overlapping := d.inFlight > 1
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.inFlight--
		d.mu.Unlock()
	}()

	select {
	case d.entered <- struct{}{}:
	default:
	}
	if overlapping {
		select {
		case d.overlap <- struct{}{}:
		default:
		}
	}

	report := d.Xbox360.HandleInterruptIN(ctx, ep, dst)
	if report == nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		select {
		case d.timeouts <- struct{}{}:
		default:
		}
	}
	return report
}

func TestXbox360NAKIdleWireBehavior(t *testing.T) {
	xbox, err := xbox360.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	dev := &observedXbox360INDevice{
		Xbox360:    xbox,
		waitBudget: 4 * time.Millisecond,
		timeouts:   make(chan struct{}, 128),
		entered:    make(chan struct{}, 128),
		overlap:    make(chan struct{}, 1),
	}
	_, conn := startAsyncINTestSession(t, dev, 3063)
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	defer conn.SetReadDeadline(time.Time{})

	submit := func(seq uint32) {
		t.Helper()
		cmd := usbip.CmdSubmit{Basic: usbip.HeaderBasic{Command: usbip.CmdSubmitCode, Seqnum: seq, Dir: usbip.DirIn, Ep: 1}, TransferBufferLen: 20}
		if err := cmd.Write(conn); err != nil {
			t.Fatalf("submit Xbox360 IN sequence %d: %v", seq, err)
		}
	}
	readReport := func(seq uint32) []byte {
		t.Helper()
		return readAsyncINResponse(t, conn, seq)
	}
	assertNeutral := func(report []byte) {
		t.Helper()
		if len(report) != 20 {
			t.Fatalf("Xbox360 report length = %d, want 20", len(report))
		}
		if report[2] != 0 || report[3] != 0 {
			t.Fatalf("Xbox360 button bytes = %02x %02x, want neutral", report[2], report[3])
		}
	}
	discardTimeouts := func() {
		t.Helper()
		for {
			select {
			case <-dev.timeouts:
			default:
				return
			}
		}
	}

	submit(41)
	assertNeutral(readReport(41))

	discardTimeouts()
	submit(42)
	waitForDeadlineCount(t, dev.timeouts, 3)
	assertNoAsyncINResponse(t, conn)
	xbox.UpdateInputState(xbox360.InputState{}) // An identical state submission remains fresh input.
	assertNeutral(readReport(42))

	discardTimeouts()
	submit(43)
	waitForDeadlineCount(t, dev.timeouts, 3)
	assertNoAsyncINResponse(t, conn)
	xbox.UpdateInputState(xbox360.InputState{Buttons: xbox360.ButtonA})
	pressed := readReport(43)
	if len(pressed) != 20 || pressed[3]&0x10 == 0 {
		t.Fatalf("Xbox360 A-button report = % x, want A pressed", pressed)
	}

	discardTimeouts()
	submit(44)
	xbox.UpdateInputState(xbox360.InputState{})
	released := readReport(44)
	if len(released) != 20 || released[3]&0x10 != 0 {
		t.Fatalf("Xbox360 A-button release report = % x, want A released", released)
	}

	discardTimeouts()
	submit(45)
	waitForDeadlineCount(t, dev.timeouts, 3)
	assertNoAsyncINResponse(t, conn)
	unlink := usbip.CmdUnlink{Basic: usbip.HeaderBasic{Command: usbip.CmdUnlinkCode, Seqnum: 46, Dir: usbip.DirIn, Ep: 1}, UnlinkSeqnum: 45}
	if err := unlink.Write(conn); err != nil {
		t.Fatalf("unlink idle Xbox360 IN: %v", err)
	}
	assertAsyncINUnlinkResponses(t, conn, 46)
	assertNoAsyncINResponse(t, conn)
}

func TestXbox360PendingFallbackINCanBeUnlinkedIndependently(t *testing.T) {
	xbox, err := xbox360.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	dev := &observedXbox360INDevice{
		Xbox360:    xbox,
		waitBudget: 250 * time.Millisecond,
		timeouts:   make(chan struct{}, 16),
		entered:    make(chan struct{}, 16),
		overlap:    make(chan struct{}, 1),
	}
	_, conn := startAsyncINTestSession(t, dev, 3064)
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	defer conn.SetReadDeadline(time.Time{})

	submit := func(seq uint32) {
		t.Helper()
		cmd := usbip.CmdSubmit{Basic: usbip.HeaderBasic{Command: usbip.CmdSubmitCode, Seqnum: seq, Dir: usbip.DirIn, Ep: 1}, TransferBufferLen: 20}
		if err := cmd.Write(conn); err != nil {
			t.Fatalf("submit Xbox360 IN sequence %d: %v", seq, err)
		}
	}
	unlink := func(unlinkSeq, requestSeq uint32) {
		t.Helper()
		cmd := usbip.CmdUnlink{Basic: usbip.HeaderBasic{Command: usbip.CmdUnlinkCode, Seqnum: unlinkSeq, Dir: usbip.DirIn, Ep: 1}, UnlinkSeqnum: requestSeq}
		if err := cmd.Write(conn); err != nil {
			t.Fatalf("unlink Xbox360 IN sequence %d: %v", requestSeq, err)
		}
		assertAsyncINUnlinkResponses(t, conn, unlinkSeq)
		assertNoAsyncINResponse(t, conn)
	}

	submit(51)
	readAsyncINResponse(t, conn, 51)
	<-dev.entered
	submit(52)
	select {
	case <-dev.entered:
	case <-time.After(time.Second):
		t.Fatal("persistent-worker Xbox360 request did not enter the device")
	}
	submit(53)
	select {
	case <-dev.overlap:
	case <-time.After(time.Second):
		t.Fatal("overlapping Xbox360 request did not enter the independent fallback")
	}

	unlink(54, 53)
	unlink(55, 52)
}

type observedSteamDeckINDevice struct {
	*steamdeck.SteamDeck
	entered         chan struct{}
	deadlineReports chan struct{}
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

func (d *observedSteamDeckINDevice) HandleInterruptIN(ctx context.Context, ep uint32, dst []byte) []byte {
	if ep == 3 {
		select {
		case d.entered <- struct{}{}:
		default:
		}
	}
	report := d.SteamDeck.HandleInterruptIN(ctx, ep, dst)
	if ep == 3 && report != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		select {
		case d.deadlineReports <- struct{}{}:
		default:
		}
	}
	return report
}

func readAsyncINResponse(t *testing.T, conn net.Conn, seq uint32) []byte {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	defer conn.SetReadDeadline(time.Time{})
	var header [48]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		t.Fatalf("read RET_SUBMIT: %v", err)
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
		t.Fatalf("read RET_SUBMIT payload: %v", err)
	}
	return payload
}

func TestSteamDeckAsyncINReturnsFreshStateForPendingRequest(t *testing.T) {
	deck, err := steamdeck.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	dev := &observedSteamDeckINDevice{
		SteamDeck:       deck,
		entered:         make(chan struct{}, 4),
		deadlineReports: make(chan struct{}, 4),
	}
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
	initial := readAsyncINResponse(t, conn, 31)
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

	latest := readAsyncINResponse(t, conn, 32)
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

	third := usbip.CmdSubmit{Basic: usbip.HeaderBasic{Command: usbip.CmdSubmitCode, Seqnum: 33, Dir: usbip.DirIn, Ep: 3}, TransferBufferLen: steamdeck.InputReportLen}
	if err := third.Write(conn); err != nil {
		t.Fatal(err)
	}
	select {
	case <-dev.entered:
	case <-time.After(time.Second):
		t.Fatal("idle Steam Deck IN did not reach the device")
	}
	waitForDeadlineCount(t, dev.deadlineReports, 1)
	idle := readAsyncINResponse(t, conn, 33)
	if len(idle) != steamdeck.InputReportLen {
		t.Fatalf("idle report length = %d, want %d", len(idle), steamdeck.InputReportLen)
	}
	if got := binary.LittleEndian.Uint32(idle[4:8]); got != 1 {
		t.Fatalf("idle frame = %d, want latest state frame 1", got)
	}
	if got := int16(binary.LittleEndian.Uint16(idle[30:32])); got != 1234 {
		t.Fatalf("idle gyro pitch = %d, want 1234", got)
	}
	if got := int16(binary.LittleEndian.Uint16(idle[48:50])); got != -2345 {
		t.Fatalf("idle stick X = %d, want -2345", got)
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
	readAsyncINResponse(t, conn, 41)

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
