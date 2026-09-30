package usb

import (
	"context"
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
	descriptor rootusb.Descriptor
	mu         sync.Mutex
	received   [][]byte
	notify     chan struct{}
	response   []byte
}

func newTransportOutTestDevice() *transportOutTestDevice {
	return &transportOutTestDevice{notify: make(chan struct{}, 8)}
}

func (d *transportOutTestDevice) HandleTransfer(_ context.Context, _ uint32, _ uint32, payload []byte) []byte {
	d.mu.Lock()
	d.received = append(d.received, append([]byte(nil), payload...))
	d.mu.Unlock()
	d.notify <- struct{}{}
	return append([]byte(nil), d.response...)
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

type transportOutHarness struct {
	client   net.Conn
	finished <-chan struct{}
	done     <-chan error
	device   *transportOutTestDevice
}

func newTransportOutHarness(t *testing.T, failResponseWrite bool) *transportOutHarness {
	t.Helper()
	return newTransportOutHarnessWithDevice(t, newTransportOutTestDevice(), failResponseWrite)
}

func newTransportOutHarnessWithDevice(t *testing.T, device *transportOutTestDevice, failResponseWrite bool) *transportOutHarness {
	t.Helper()
	busID := transportOutTestBusID.Add(1) + 41000
	bus, err := virtualbus.NewWithBusID(busID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bus.Add(device); err != nil {
		t.Fatal(err)
	}
	server := New(ServerConfig{DisableAutoBusCleanup: true}, slog.New(slog.DiscardHandler), nil)
	if err := server.AddBus(bus); err != nil {
		t.Fatal(err)
	}
	serverConn, clientConn := net.Pipe()
	var streamConn net.Conn = serverConn
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
	return &transportOutHarness{client: clientConn, finished: finished, done: done, device: device}
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
	cmd := usbip.CmdSubmit{Basic: usbip.HeaderBasic{Command: usbip.CmdSubmitCode, Seqnum: seq, Dir: usbip.DirOut, Ep: 1}, TransferBufferLen: uint32(len(payload))}
	if err := cmd.Write(h.client); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := h.client.Write(payload); err != nil {
			return err
		}
	}
	return nil
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
