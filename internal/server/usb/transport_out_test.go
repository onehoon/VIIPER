package usb

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	rootusb "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/Alia5/VIIPER/virtualbus"
)

var transportOutTestBusID atomic.Uint32

type transportOutTestDevice struct {
	descriptor      rootusb.Descriptor
	mu              sync.Mutex
	received        [][]byte
	notify          chan struct{}
	response        []byte
	controlResponse []byte
	transferRelease <-chan struct{}
}

func newTransportOutTestDevice() *transportOutTestDevice {
	return &transportOutTestDevice{notify: make(chan struct{}, 8)}
}

func (d *transportOutTestDevice) HandleTransfer(_ context.Context, _ uint32, _ uint32, payload []byte) []byte {
	d.mu.Lock()
	d.received = append(d.received, append([]byte(nil), payload...))
	response := append([]byte(nil), d.response...)
	transferRelease := d.transferRelease
	d.mu.Unlock()
	d.notify <- struct{}{}
	if transferRelease != nil {
		<-transferRelease
	}
	return response
}

func (d *transportOutTestDevice) HandleControl(_ uint8, bRequest uint8, _, _, _ uint16, _ []byte) ([]byte, bool) {
	if bRequest != 0x99 {
		return nil, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]byte(nil), d.controlResponse...), true
}

func (d *transportOutTestDevice) GetDescriptor() *rootusb.Descriptor  { return &d.descriptor }
func (*transportOutTestDevice) GetDeviceSpecificArgs() map[string]any { return nil }

func (d *transportOutTestDevice) receivedPayloads() [][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	got := make([][]byte, len(d.received))
	for i := range d.received {
		got[i] = append([]byte(nil), d.received[i]...)
	}
	return got
}

type transportOutWriteFailureConn struct {
	net.Conn
	err error
}

func (c transportOutWriteFailureConn) Write([]byte) (int, error) { return 0, c.err }

type transportOutCountingConn struct {
	net.Conn
	writes atomic.Int32
}

func (c *transportOutCountingConn) Write(p []byte) (int, error) {
	c.writes.Add(1)
	return c.Conn.Write(p)
}

type transportOutHarness struct {
	client   net.Conn
	finished <-chan struct{}
	done     <-chan error
	device   *transportOutTestDevice
	writes   *atomic.Int32
}

func newTransportOutHarness(t *testing.T, failResponseWrite bool) *transportOutHarness {
	t.Helper()
	return newTransportOutHarnessWithDevice(t, newTransportOutTestDevice(), failResponseWrite)
}

func newTransportOutHarnessWithDevice(t *testing.T, device *transportOutTestDevice, failResponseWrite bool) *transportOutHarness {
	t.Helper()
	return newTransportOutHarnessWithConfig(t, device, failResponseWrite, 0, false)
}

func newTransportOutHarnessWithConfig(t *testing.T, device *transportOutTestDevice, failResponseWrite bool, flushInterval time.Duration, countWrites bool) *transportOutHarness {
	t.Helper()
	busID := transportOutTestBusID.Add(1) + 41000
	bus, err := virtualbus.NewWithBusID(busID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bus.Add(device); err != nil {
		t.Fatal(err)
	}
	server := New(ServerConfig{DisableAutoBusCleanup: true, WriteBatchFlushInterval: flushInterval}, slog.New(slog.DiscardHandler), nil)
	if err := server.AddBus(bus); err != nil {
		t.Fatal(err)
	}
	serverConn, clientConn := net.Pipe()
	var streamConn net.Conn = serverConn
	var writeCount *atomic.Int32
	if countWrites {
		countingConn := &transportOutCountingConn{Conn: streamConn}
		streamConn = countingConn
		writeCount = &countingConn.writes
	}
	if failResponseWrite {
		streamConn = transportOutWriteFailureConn{Conn: serverConn, err: errors.New("simulated RET_SUBMIT write failure")}
	}
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		done <- server.handleUrbStream(streamConn, device)
		close(finished)
	}()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
		<-finished
		_ = server.RemoveBus(busID)
	})
	return &transportOutHarness{client: clientConn, finished: finished, done: done, device: device, writes: writeCount}
}

func (h *transportOutHarness) writeSubmitHeader(t *testing.T, seq uint32, payloadLength uint32) {
	t.Helper()
	cmd := usbip.CmdSubmit{Basic: usbip.HeaderBasic{Command: usbip.CmdSubmitCode, Seqnum: seq, Dir: usbip.DirOut, Ep: 1}, TransferBufferLen: payloadLength}
	if err := cmd.Write(h.client); err != nil {
		t.Fatalf("write CMD_SUBMIT header: %v", err)
	}
}

func (h *transportOutHarness) submit(t *testing.T, seq uint32, payload []byte) (uint32, uint32) {
	t.Helper()
	if err := h.sendSubmit(seq, payload); err != nil {
		t.Fatalf("write CMD_SUBMIT: %v", err)
	}
	return h.readResponse(t)
}

func (h *transportOutHarness) sendSubmit(seq uint32, payload []byte) error {
	return h.sendTransfer(seq, usbip.DirOut, 1, uint32(len(payload)), [8]byte{}, payload)
}

func (h *transportOutHarness) sendTransfer(seq, dir, ep, transferLength uint32, setup [8]byte, payload []byte) error {
	cmd := usbip.CmdSubmit{
		Basic:             usbip.HeaderBasic{Command: usbip.CmdSubmitCode, Seqnum: seq, Dir: dir, Ep: ep},
		TransferBufferLen: transferLength,
		Setup:             setup,
	}
	if err := cmd.Write(h.client); err != nil {
		return err
	}
	if dir == usbip.DirOut && len(payload) > 0 {
		if _, err := h.client.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

func (h *transportOutHarness) readFrame(t *testing.T, payloadLength int) ([retSubmitHeaderSize]byte, []byte) {
	t.Helper()
	var header [retSubmitHeaderSize]byte
	if _, err := io.ReadFull(h.client, header[:]); err != nil {
		t.Fatalf("read RET_SUBMIT header: %v", err)
	}
	payload := make([]byte, payloadLength)
	if _, err := io.ReadFull(h.client, payload); err != nil {
		t.Fatalf("read RET_SUBMIT payload: %v", err)
	}
	return header, payload
}

func controlVendorINSetup(length uint16) [8]byte {
	return [8]byte{0xc0, 0x99, 0, 0, 0, 0, byte(length), byte(length >> 8)}
}

func (h *transportOutHarness) readResponse(t *testing.T) (uint32, uint32) {
	t.Helper()
	var response [retSubmitHeaderSize]byte
	if _, err := io.ReadFull(h.client, response[:]); err != nil {
		t.Fatalf("read RET_SUBMIT: %v", err)
	}
	return uint32(response[4])<<24 | uint32(response[5])<<16 | uint32(response[6])<<8 | uint32(response[7]),
		uint32(response[24])<<24 | uint32(response[25])<<16 | uint32(response[26])<<8 | uint32(response[27])
}

func TestUSBIPOutProcessesOnlyAfterCompletePayloadAndPreservesResponse(t *testing.T) {
	h := newTransportOutHarness(t, false)
	payload := []byte{0x00, 0x08, 0x00, 0x03, 0x06, 0x00, 0x00, 0x00}
	h.writeSubmitHeader(t, 71, uint32(len(payload)))
	if _, err := h.client.Write(payload[:3]); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.device.notify:
		t.Fatal("device processed OUT before the complete payload arrived")
	default:
	}
	if _, err := h.client.Write(payload[3:]); err != nil {
		t.Fatal(err)
	}
	seq, actualLength := h.readResponse(t)
	if seq != 71 || actualLength != uint32(len(payload)) {
		t.Fatalf("RET_SUBMIT seq/length = %d/%d, want 71/%d", seq, actualLength, len(payload))
	}
	select {
	case <-h.device.notify:
	default:
		t.Fatal("device did not process completed OUT payload")
	}
	if got := h.device.receivedPayloads(); !reflect.DeepEqual(got, [][]byte{payload}) {
		t.Fatalf("device payloads = % x, want % x", got, payload)
	}
}

func TestUSBIPOutZeroLengthPayloadCompletesSuccessfully(t *testing.T) {
	h := newTransportOutHarness(t, false)
	seq, actualLength := h.submit(t, 82, nil)
	if seq != 82 || actualLength != 0 {
		t.Fatalf("zero-length RET_SUBMIT seq/length = %d/%d, want 82/0", seq, actualLength)
	}
	select {
	case <-h.device.notify:
	default:
		t.Fatal("device did not receive zero-length OUT")
	}
	if got := h.device.receivedPayloads(); len(got) != 1 || got[0] != nil {
		t.Fatalf("device received = %#v, want one empty payload", got)
	}
}

func TestUSBIPOutResponseWriteFailureReturnsError(t *testing.T) {
	h := newTransportOutHarness(t, true)
	payload := []byte{0x00, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	h.writeSubmitHeader(t, 91, uint32(len(payload)))
	if _, err := h.client.Write(payload); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-h.done:
		if err == nil {
			t.Fatal("handleUrbStream succeeded despite RET_SUBMIT write failure")
		}
	case <-h.finished:
		if err := <-h.done; err == nil {
			t.Fatal("handleUrbStream succeeded despite RET_SUBMIT write failure")
		}
	}
	if got := h.device.receivedPayloads(); !reflect.DeepEqual(got, [][]byte{payload}) {
		t.Fatalf("device payloads = % x, want request processed before response failure", got)
	}
}

func TestUSBIPOutRetSubmitOmitsDeviceResponseAndPreservesStreamBoundary(t *testing.T) {
	payload := []byte{0x10, 0x20, 0x30, 0x40}
	device := newTransportOutTestDevice()
	device.response = []byte{0xAA, 0xBB, 0xCC}
	h := newTransportOutHarnessWithDevice(t, device, false)

	if err := h.sendSubmit(101, payload); err != nil {
		t.Fatalf("write first CMD_SUBMIT: %v", err)
	}
	secondSubmitWritten := make(chan error, 1)
	go func() { secondSubmitWritten <- h.sendSubmit(102, payload) }()

	if seq, actualLength := h.readResponse(t); seq != 101 || actualLength != uint32(len(payload)) {
		t.Fatalf("first RET_SUBMIT seq/length = %d/%d, want 101/%d", seq, actualLength, len(payload))
	}
	if err := h.client.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	secondSeq, secondLength := h.readResponse(t)
	if err := h.client.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := <-secondSubmitWritten; err != nil {
		t.Fatalf("write second CMD_SUBMIT: %v", err)
	}
	if secondSeq != 102 || secondLength != uint32(len(payload)) {
		t.Fatalf("second RET_SUBMIT seq/length = %d/%d, want 102/%d (unexpected OUT response bytes may have shifted the stream)", secondSeq, secondLength, len(payload))
	}
	if got := device.receivedPayloads(); !reflect.DeepEqual(got, [][]byte{payload, payload}) {
		t.Fatalf("device payloads = % x, want two copies of % x", got, payload)
	}
}

func TestUSBIPRetSubmitUsesOneDirectWriteForBodyAndHeaderOnly(t *testing.T) {
	payload := []byte{0x51, 0x62, 0x73}
	device := newTransportOutTestDevice()
	device.response = payload
	h := newTransportOutHarnessWithConfig(t, device, false, 0, true)
	if err := h.client.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	defer h.client.SetReadDeadline(time.Time{})

	if err := h.sendTransfer(111, usbip.DirIn, 1, 8, [8]byte{}, nil); err != nil {
		t.Fatalf("write IN CMD_SUBMIT: %v", err)
	}
	header, body := h.readFrame(t, len(payload))
	if got := binary.BigEndian.Uint32(header[0:4]); got != usbip.RetSubmitCode {
		t.Fatalf("IN response command = %#x, want RET_SUBMIT", got)
	}
	if got := binary.BigEndian.Uint32(header[4:8]); got != 111 {
		t.Fatalf("IN response sequence = %d, want 111", got)
	}
	if got := binary.BigEndian.Uint32(header[24:28]); got != uint32(len(payload)) {
		t.Fatalf("IN response length = %d, want %d", got, len(payload))
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("IN response body = % x, want % x", body, payload)
	}
	if got := h.writes.Load(); got != 1 {
		t.Fatalf("server-side Write calls for body-bearing RET_SUBMIT = %d, want 1", got)
	}

	outPayload := []byte{0xa1, 0xb2, 0xc3, 0xd4}
	seq, actualLength := h.submit(t, 112, outPayload)
	if seq != 112 || actualLength != uint32(len(outPayload)) {
		t.Fatalf("OUT response sequence/length = %d/%d, want 112/%d", seq, actualLength, len(outPayload))
	}
	if got := h.writes.Load(); got != 2 {
		t.Fatalf("server-side Write calls after header-only OUT RET_SUBMIT = %d, want 2 total", got)
	}
}

func TestUSBIPRetSubmitLargeEP0ThenShortAndEmptyFrames(t *testing.T) {
	controlPayload := make([]byte, 128)
	for i := range controlPayload {
		controlPayload[i] = byte(i ^ 0x5a)
	}
	device := newTransportOutTestDevice()
	device.controlResponse = controlPayload
	h := newTransportOutHarnessWithDevice(t, device, false)
	if err := h.client.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	defer h.client.SetReadDeadline(time.Time{})

	for _, tc := range []struct {
		seq      uint32
		length   uint16
		wantBody []byte
	}{
		{seq: 121, length: 128, wantBody: controlPayload},
		{seq: 122, length: 3, wantBody: controlPayload[:3]},
		{seq: 123, length: 0, wantBody: nil},
	} {
		if err := h.sendTransfer(tc.seq, usbip.DirIn, 0, uint32(tc.length), controlVendorINSetup(tc.length), nil); err != nil {
			t.Fatalf("write EP0 CMD_SUBMIT %d: %v", tc.seq, err)
		}
		header, body := h.readFrame(t, len(tc.wantBody))
		if got := binary.BigEndian.Uint32(header[0:4]); got != usbip.RetSubmitCode {
			t.Fatalf("EP0 response command = %#x, want RET_SUBMIT", got)
		}
		if got := binary.BigEndian.Uint32(header[4:8]); got != tc.seq {
			t.Fatalf("EP0 response sequence = %d, want %d", got, tc.seq)
		}
		if got := binary.BigEndian.Uint32(header[24:28]); got != uint32(len(tc.wantBody)) {
			t.Fatalf("EP0 response actual length = %d, want %d", got, len(tc.wantBody))
		}
		if !bytes.Equal(body, tc.wantBody) {
			t.Fatalf("EP0 response body = % x, want % x", body, tc.wantBody)
		}
	}
}

func TestUSBIPRetSubmitMixedCompletionsRemainFramed(t *testing.T) {
	inPayload := []byte{0x11, 0x22, 0x33}
	controlPayload := make([]byte, 128)
	for i := range controlPayload {
		controlPayload[i] = byte(255 - i)
	}
	inRelease := make(chan struct{})
	var releaseOnce sync.Once
	releaseIN := func() { releaseOnce.Do(func() { close(inRelease) }) }
	defer releaseIN()

	device := newTransportOutTestDevice()
	device.response = inPayload
	device.controlResponse = controlPayload
	device.transferRelease = inRelease
	h := newTransportOutHarnessWithDevice(t, device, false)
	if err := h.client.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	defer h.client.SetReadDeadline(time.Time{})

	if err := h.sendTransfer(131, usbip.DirIn, 1, 8, [8]byte{}, nil); err != nil {
		t.Fatalf("write async IN CMD_SUBMIT: %v", err)
	}
	select {
	case <-device.notify:
	case <-time.After(time.Second):
		t.Fatal("async IN worker did not enter the device")
	}

	type responseFrame struct {
		header [retSubmitHeaderSize]byte
		body   []byte
	}
	type readResult struct {
		frames map[uint32]responseFrame
		err    error
	}
	readDone := make(chan readResult, 1)
	go func() {
		result := readResult{frames: make(map[uint32]responseFrame, 3)}
		payloadLengths := map[uint32]int{131: len(inPayload), 132: len(controlPayload), 133: 0}
		for range 3 {
			var frame responseFrame
			if _, err := io.ReadFull(h.client, frame.header[:]); err != nil {
				result.err = err
				readDone <- result
				return
			}
			seq := binary.BigEndian.Uint32(frame.header[4:8])
			payloadLength, ok := payloadLengths[seq]
			if !ok {
				result.err = errors.New("unexpected or misaligned response sequence")
				readDone <- result
				return
			}
			frame.body = make([]byte, payloadLength)
			if _, err := io.ReadFull(h.client, frame.body); err != nil {
				result.err = err
				readDone <- result
				return
			}
			result.frames[seq] = frame
		}
		readDone <- result
	}()

	if err := h.sendTransfer(132, usbip.DirIn, 0, uint32(len(controlPayload)), controlVendorINSetup(uint16(len(controlPayload))), nil); err != nil {
		t.Fatalf("write EP0 CMD_SUBMIT: %v", err)
	}
	if err := h.sendSubmit(133, []byte{0xa1, 0xb2, 0xc3, 0xd4}); err != nil {
		t.Fatalf("write OUT CMD_SUBMIT: %v", err)
	}
	select {
	case <-device.notify:
	case <-time.After(time.Second):
		t.Fatal("OUT transfer did not reach the device")
	}
	releaseIN()

	var result readResult
	select {
	case result = <-readDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out reading mixed RET_SUBMIT frames")
	}
	if result.err != nil {
		t.Fatalf("read mixed RET_SUBMIT frames: %v", result.err)
	}
	wants := map[uint32]struct {
		actualLength uint32
		body         []byte
	}{
		131: {actualLength: uint32(len(inPayload)), body: inPayload},
		132: {actualLength: uint32(len(controlPayload)), body: controlPayload},
		133: {actualLength: 4},
	}
	for seq, want := range wants {
		frame, ok := result.frames[seq]
		if !ok {
			t.Fatalf("missing RET_SUBMIT sequence %d", seq)
		}
		if got := binary.BigEndian.Uint32(frame.header[0:4]); got != usbip.RetSubmitCode {
			t.Fatalf("sequence %d command = %#x, want RET_SUBMIT", seq, got)
		}
		if got := binary.BigEndian.Uint32(frame.header[24:28]); got != want.actualLength {
			t.Fatalf("sequence %d actual length = %d, want %d", seq, got, want.actualLength)
		}
		if !bytes.Equal(frame.body, want.body) {
			t.Fatalf("sequence %d body = % x, want % x", seq, frame.body, want.body)
		}
	}
}

func TestUSBIPRetSubmitPositiveIntervalBatchingPreservesFrames(t *testing.T) {
	inPayload := []byte{0x61, 0x72, 0x83}
	device := newTransportOutTestDevice()
	device.response = inPayload
	h := newTransportOutHarnessWithConfig(t, device, false, time.Millisecond, false)
	if err := h.client.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	defer h.client.SetReadDeadline(time.Time{})

	if err := h.sendTransfer(141, usbip.DirIn, 1, 8, [8]byte{}, nil); err != nil {
		t.Fatalf("write IN CMD_SUBMIT: %v", err)
	}
	header, body := h.readFrame(t, len(inPayload))
	if got := binary.BigEndian.Uint32(header[0:4]); got != usbip.RetSubmitCode {
		t.Fatalf("IN response command = %#x, want RET_SUBMIT", got)
	}
	if got := binary.BigEndian.Uint32(header[4:8]); got != 141 {
		t.Fatalf("IN response sequence = %d, want 141", got)
	}
	if got := binary.BigEndian.Uint32(header[24:28]); got != uint32(len(inPayload)) {
		t.Fatalf("IN response actual length = %d, want %d", got, len(inPayload))
	}
	if !bytes.Equal(body, inPayload) {
		t.Fatalf("IN response body = % x, want % x", body, inPayload)
	}

	outPayload := []byte{0x91, 0xa2, 0xb3, 0xc4}
	if err := h.sendSubmit(142, outPayload); err != nil {
		t.Fatalf("write OUT CMD_SUBMIT: %v", err)
	}
	header, body = h.readFrame(t, 0)
	if got := binary.BigEndian.Uint32(header[0:4]); got != usbip.RetSubmitCode {
		t.Fatalf("OUT response command = %#x, want RET_SUBMIT", got)
	}
	if got := binary.BigEndian.Uint32(header[4:8]); got != 142 {
		t.Fatalf("OUT response sequence = %d, want 142", got)
	}
	if got := binary.BigEndian.Uint32(header[24:28]); got != uint32(len(outPayload)) {
		t.Fatalf("OUT response actual length = %d, want %d", got, len(outPayload))
	}
	if len(body) != 0 {
		t.Fatalf("OUT response body = % x, want empty", body)
	}
}
