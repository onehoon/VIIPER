package usb

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	rootusb "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/Alia5/VIIPER/virtualbus"
)

var usbipOutTraceTestBusID atomic.Uint32

type usbipOutTraceCapture struct {
	mu      sync.Mutex
	records []slog.Record
	notify  chan struct{}
}

func (*usbipOutTraceCapture) Enabled(context.Context, slog.Level) bool { return true }
func (h *usbipOutTraceCapture) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.records = append(h.records, r.Clone())
	h.mu.Unlock()
	select {
	case h.notify <- struct{}{}:
	default:
	}
	return nil
}
func (h *usbipOutTraceCapture) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *usbipOutTraceCapture) WithGroup(string) slog.Handler      { return h }

func (h *usbipOutTraceCapture) events() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	var events []map[string]any
	for _, record := range h.records {
		attrs := make(map[string]any, record.NumAttrs())
		record.Attrs(func(attr slog.Attr) bool {
			attrs[attr.Key] = attr.Value.Any()
			return true
		})
		if _, ok := attrs["Event"]; ok {
			events = append(events, attrs)
		}
	}
	return events
}

// usbipOutTraceProbe exercises the server's optional hook boundary without
// importing Xbox360 into this package (which would form an import cycle).
type usbipOutTraceProbe struct {
	descriptor rootusb.Descriptor
	logger     *slog.Logger
	trace      bool
	busID      uint32
	deviceID   uint32
	mu         sync.Mutex
	received   [][]byte
}

func (d *usbipOutTraceProbe) HandleTransfer(_ context.Context, _ uint32, _ uint32, payload []byte) []byte {
	copyOfPayload := append([]byte(nil), payload...)
	d.mu.Lock()
	d.received = append(d.received, copyOfPayload)
	d.mu.Unlock()
	if d.trace {
		d.log("ProbeHandleTransfer", "Payload", hex.EncodeToString(copyOfPayload))
	}
	return nil
}

func (d *usbipOutTraceProbe) GetDescriptor() *rootusb.Descriptor  { return &d.descriptor }
func (*usbipOutTraceProbe) GetDeviceSpecificArgs() map[string]any { return nil }

func (d *usbipOutTraceProbe) TraceUSBIPOutIngress(seq, ep, declaredLength uint32, payload []byte) {
	if !d.trace || ep != 1 {
		return
	}
	bounded := payload
	if len(bounded) > 32 {
		bounded = bounded[:32]
	}
	d.log("X360USBIPOutIngress", "USBIPSeq", seq, "Endpoint", ep, "DeclaredLength", declaredLength, "Payload", hex.EncodeToString(bounded))
}

func (d *usbipOutTraceProbe) TraceUSBIPOutWriterAccepted(seq, ep, actualLength uint32) {
	if !d.trace || ep != 1 {
		return
	}
	d.log("X360USBIPOutWriterAccepted", "USBIPSeq", seq, "Endpoint", ep, "ActualLength", actualLength)
}

func (d *usbipOutTraceProbe) log(event string, attrs ...any) {
	base := []any{"Event", event, "ProcessID", os.Getpid(), "BusID", d.busID, "DeviceID", d.deviceID, "TraceSessionID", uint64(1)}
	d.logger.Info("test-only USB/IP boundary", append(base, attrs...)...)
}

func (d *usbipOutTraceProbe) receivedPayloads() [][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	got := make([][]byte, len(d.received))
	for i := range d.received {
		got[i] = append([]byte(nil), d.received[i]...)
	}
	return got
}

type usbipOutPlainDevice struct {
	descriptor rootusb.Descriptor
	mu         sync.Mutex
	received   [][]byte
}

func (d *usbipOutPlainDevice) HandleTransfer(_ context.Context, _ uint32, _ uint32, payload []byte) []byte {
	d.mu.Lock()
	d.received = append(d.received, append([]byte(nil), payload...))
	d.mu.Unlock()
	return nil
}
func (d *usbipOutPlainDevice) GetDescriptor() *rootusb.Descriptor  { return &d.descriptor }
func (*usbipOutPlainDevice) GetDeviceSpecificArgs() map[string]any { return nil }

type usbipOutTraceWriteFailureConn struct {
	net.Conn
	writeErr error
}

func (c usbipOutTraceWriteFailureConn) Write([]byte) (int, error) { return 0, c.writeErr }

type usbipOutTraceHarness struct {
	client   net.Conn
	finished <-chan struct{}
	done     <-chan error
	capture  *usbipOutTraceCapture
}

func newUSBIPOutTraceHarness(t *testing.T, device rootusb.Device, probe *usbipOutTraceProbe, failResponseWrite bool, batchInterval time.Duration) *usbipOutTraceHarness {
	t.Helper()
	busID := usbipOutTraceTestBusID.Add(1) + 41000
	bus, err := virtualbus.NewWithBusID(busID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bus.Add(device); err != nil {
		t.Fatal(err)
	}
	capture := &usbipOutTraceCapture{notify: make(chan struct{}, 128)}
	if probe != nil {
		meta := bus.GetAllDeviceMetas()[0].Meta
		probe.busID, probe.deviceID = meta.BusID, meta.DevID
		probe.logger = slog.New(capture)
	}
	server := New(ServerConfig{DisableAutoBusCleanup: true, WriteBatchFlushInterval: batchInterval}, slog.New(slog.DiscardHandler), nil)
	if err := server.AddBus(bus); err != nil {
		t.Fatal(err)
	}
	serverConn, clientConn := net.Pipe()
	var streamConn net.Conn = serverConn
	if failResponseWrite {
		streamConn = usbipOutTraceWriteFailureConn{Conn: serverConn, writeErr: fmt.Errorf("simulated RET_SUBMIT write failure")}
	}
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		done <- server.handleUrbStream(streamConn, device)
		close(finished)
	}()
	h := &usbipOutTraceHarness{client: clientConn, finished: finished, done: done, capture: capture}
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
		<-finished
		_ = server.RemoveBus(busID)
	})
	return h
}

func (h *usbipOutTraceHarness) waitWriterAccepted(t *testing.T, seq string) {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		if outTraceEvent(h.capture.events(), "X360USBIPOutWriterAccepted", seq) != nil {
			return
		}
		select {
		case <-h.capture.notify:
		case <-timer.C:
			t.Fatalf("timed out waiting for X360USBIPOutWriterAccepted USBIPSeq=%s", seq)
		}
	}
}

func (h *usbipOutTraceHarness) submit(t *testing.T, seq uint32, payload []byte) (uint32, uint32) {
	t.Helper()
	cmd := usbip.CmdSubmit{Basic: usbip.HeaderBasic{Command: usbip.CmdSubmitCode, Seqnum: seq, Dir: usbip.DirOut, Ep: 1}, TransferBufferLen: uint32(len(payload))}
	if err := cmd.Write(h.client); err != nil {
		t.Fatalf("write CMD_SUBMIT header: %v", err)
	}
	if len(payload) > 0 {
		if _, err := h.client.Write(payload); err != nil {
			t.Fatalf("write CMD_SUBMIT payload: %v", err)
		}
	}
	return h.readResponse(t)
}

func (h *usbipOutTraceHarness) readResponse(t *testing.T) (uint32, uint32) {
	t.Helper()
	var response [retSubmitHeaderSize]byte
	if _, err := io.ReadFull(h.client, response[:]); err != nil {
		t.Fatalf("read RET_SUBMIT: %v", err)
	}
	return uint32(response[4])<<24 | uint32(response[5])<<16 | uint32(response[6])<<8 | uint32(response[7]),
		uint32(response[24])<<24 | uint32(response[25])<<16 | uint32(response[26])<<8 | uint32(response[27])
}

func outTraceEvent(events []map[string]any, name string, seq string) map[string]any {
	for _, event := range events {
		if event["Event"] == name && fmt.Sprint(event["USBIPSeq"]) == seq {
			return event
		}
	}
	return nil
}

func TestUSBIPXbox360OutTraceOrdersAndPreservesPackets(t *testing.T) {
	device := &usbipOutTraceProbe{trace: true}
	h := newUSBIPOutTraceHarness(t, device, device, false, 0)
	stop := []byte{0x00, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	cmd := usbip.CmdSubmit{Basic: usbip.HeaderBasic{Command: usbip.CmdSubmitCode, Seqnum: 71, Dir: usbip.DirOut, Ep: 1}, TransferBufferLen: uint32(len(stop))}
	if err := cmd.Write(h.client); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.Write(stop[:3]); err != nil {
		t.Fatal(err)
	}
	if events := h.capture.events(); len(events) != 0 {
		t.Fatalf("trace was emitted before the complete declared payload arrived: %+v", events)
	}
	if _, err := h.client.Write(stop[3:]); err != nil {
		t.Fatal(err)
	}
	if gotSeq, gotLength := h.readResponse(t); gotSeq != 71 || gotLength != uint32(len(stop)) {
		t.Fatalf("STOP RET_SUBMIT seq/length = %d/%d, want 71/%d", gotSeq, gotLength, len(stop))
	}
	h.waitWriterAccepted(t, "71")

	nonzero := []byte{0x00, 0x08, 0x00, 0x03, 0x06, 0x00, 0x00, 0x00}
	if gotSeq, gotLength := h.submit(t, 72, nonzero); gotSeq != 72 || gotLength != uint32(len(nonzero)) {
		t.Fatalf("nonzero RET_SUBMIT seq/length = %d/%d, want 72/%d", gotSeq, gotLength, len(nonzero))
	}
	h.waitWriterAccepted(t, "72")
	if got, want := device.receivedPayloads(), [][]byte{stop, nonzero}; !reflect.DeepEqual(got, want) {
		t.Fatalf("device payloads = % x, want % x", got, want)
	}

	events := h.capture.events()
	wantOrder := []string{
		"X360USBIPOutIngress", "ProbeHandleTransfer", "X360USBIPOutWriterAccepted",
		"X360USBIPOutIngress", "ProbeHandleTransfer", "X360USBIPOutWriterAccepted",
	}
	if len(events) != len(wantOrder) {
		t.Fatalf("event count = %d, want %d: %+v", len(events), len(wantOrder), events)
	}
	for i, want := range wantOrder {
		if events[i]["Event"] != want {
			t.Fatalf("event %d = %v, want %s", i, events[i]["Event"], want)
		}
		for _, key := range []string{"ProcessID", "BusID", "DeviceID", "TraceSessionID"} {
			if events[i][key] == nil || fmt.Sprint(events[i][key]) == "0" {
				t.Fatalf("event %d missing %s identity: %+v", i, key, events[i])
			}
		}
	}
	for _, tc := range []struct {
		seq     string
		payload []byte
	}{{"71", stop}, {"72", nonzero}} {
		ingress := outTraceEvent(events, "X360USBIPOutIngress", tc.seq)
		accepted := outTraceEvent(events, "X360USBIPOutWriterAccepted", tc.seq)
		if ingress == nil || accepted == nil {
			t.Fatalf("missing boundary events for USBIPSeq %s: %+v", tc.seq, events)
		}
		if fmt.Sprint(ingress["Endpoint"]) != "1" || fmt.Sprint(ingress["DeclaredLength"]) != fmt.Sprint(len(tc.payload)) || ingress["Payload"] != hex.EncodeToString(tc.payload) {
			t.Fatalf("ingress for seq %s = %+v", tc.seq, ingress)
		}
		if fmt.Sprint(accepted["Endpoint"]) != "1" || fmt.Sprint(accepted["ActualLength"]) != fmt.Sprint(len(tc.payload)) {
			t.Fatalf("writer-accepted for seq %s = %+v", tc.seq, accepted)
		}
	}
	if first, second := outTraceEvent(events, "X360USBIPOutIngress", "71"), outTraceEvent(events, "X360USBIPOutIngress", "72"); first["Payload"] != hex.EncodeToString(stop) || second["Payload"] != hex.EncodeToString(nonzero) {
		t.Fatalf("scratch-buffer reuse changed a prior record: first=%+v second=%+v", first, second)
	}
}

func TestUSBIPXbox360OutTraceBoundsPayloadAndLogsZeroLength(t *testing.T) {
	device := &usbipOutTraceProbe{trace: true}
	h := newUSBIPOutTraceHarness(t, device, device, false, 0)
	longPayload := make([]byte, 40)
	for i := range longPayload {
		longPayload[i] = byte(i)
	}
	if _, gotLength := h.submit(t, 81, longPayload); gotLength != uint32(len(longPayload)) {
		t.Fatalf("long-payload response length = %d, want %d", gotLength, len(longPayload))
	}
	if _, gotLength := h.submit(t, 82, nil); gotLength != 0 {
		t.Fatalf("zero-length response length = %d, want 0", gotLength)
	}
	h.waitWriterAccepted(t, "82")
	if got, want := device.receivedPayloads(), [][]byte{longPayload, nil}; !reflect.DeepEqual(got, want) {
		t.Fatalf("device payloads = % x, want % x", got, want)
	}
	events := h.capture.events()
	longIngress := outTraceEvent(events, "X360USBIPOutIngress", "81")
	if longIngress == nil || fmt.Sprint(longIngress["DeclaredLength"]) != "40" || longIngress["Payload"] != hex.EncodeToString(longPayload[:32]) {
		t.Fatalf("bounded ingress record = %+v", longIngress)
	}
	zeroIngress := outTraceEvent(events, "X360USBIPOutIngress", "82")
	if zeroIngress == nil || fmt.Sprint(zeroIngress["DeclaredLength"]) != "0" || zeroIngress["Payload"] != "" {
		t.Fatalf("zero-length ingress record = %+v", zeroIngress)
	}
}

func TestUSBIPXbox360OutTraceOmitsWriterAcceptedOnWriteFailure(t *testing.T) {
	device := &usbipOutTraceProbe{trace: true}
	h := newUSBIPOutTraceHarness(t, device, device, true, 0)
	cmd := usbip.CmdSubmit{Basic: usbip.HeaderBasic{Command: usbip.CmdSubmitCode, Seqnum: 91, Dir: usbip.DirOut, Ep: 1}, TransferBufferLen: 8}
	if err := cmd.Write(h.client); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.Write([]byte{0, 8, 0, 0, 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-h.done:
		if err == nil {
			t.Fatal("handleUrbStream succeeded despite RET_SUBMIT write failure")
		}
	case <-time.After(time.Second):
		t.Fatal("handleUrbStream did not return after RET_SUBMIT write failure")
	}
	events := h.capture.events()
	if outTraceEvent(events, "X360USBIPOutIngress", "91") == nil {
		t.Fatalf("ingress event missing before failed response: %+v", events)
	}
	if outTraceEvent(events, "X360USBIPOutWriterAccepted", "91") != nil {
		t.Fatalf("writer-accepted event emitted after failed response: %+v", events)
	}
	if got := device.receivedPayloads(); len(got) != 1 || len(got[0]) != 8 {
		t.Fatalf("device did not receive failed-response request: % x", got)
	}
}

func TestUSBIPOutWriterAcceptedCanPrecedeBatchFlush(t *testing.T) {
	device := &usbipOutTraceProbe{trace: true}
	h := newUSBIPOutTraceHarness(t, device, device, false, time.Hour)
	payload := []byte{0, 8, 0, 3, 6, 0, 0, 0}
	cmd := usbip.CmdSubmit{Basic: usbip.HeaderBasic{Command: usbip.CmdSubmitCode, Seqnum: 95, Dir: usbip.DirOut, Ep: 1}, TransferBufferLen: uint32(len(payload))}
	if err := cmd.Write(h.client); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.Write(payload); err != nil {
		t.Fatal(err)
	}
	// The client deliberately does not read RET_SUBMIT. net.Pipe writes block
	// until a peer reads, so this event can arrive only because writeRet's
	// batchingWriter accepted the bytes into its buffer without flushing.
	h.waitWriterAccepted(t, "95")
	events := h.capture.events()
	if outTraceEvent(events, "X360USBIPOutIngress", "95") == nil {
		t.Fatalf("ingress event missing before writer acceptance: %+v", events)
	}
}

func TestUSBIPOutOptionalTracePreservesUntracedDeviceBehavior(t *testing.T) {
	t.Run("optional tracer with no active trace", func(t *testing.T) {
		device := &usbipOutTraceProbe{trace: false}
		h := newUSBIPOutTraceHarness(t, device, device, false, 0)
		payload := []byte{0, 8, 0, 7, 9, 0, 0, 0}
		if _, gotLength := h.submit(t, 101, payload); gotLength != uint32(len(payload)) {
			t.Fatalf("response length = %d, want %d", gotLength, len(payload))
		}
		if got := device.receivedPayloads(); len(got) != 1 || !reflect.DeepEqual(got[0], payload) {
			t.Fatalf("untraced device received = % x, want % x", got, payload)
		}
		if events := h.capture.events(); len(events) != 0 {
			t.Fatalf("inactive trace emitted events: %+v", events)
		}
	})

	t.Run("device without optional tracer", func(t *testing.T) {
		device := &usbipOutPlainDevice{descriptor: rootusb.Descriptor{}}
		h := newUSBIPOutTraceHarness(t, device, nil, false, 0)
		payload := []byte{1, 2, 3}
		if _, gotLength := h.submit(t, 102, payload); gotLength != uint32(len(payload)) {
			t.Fatalf("response length = %d, want %d", gotLength, len(payload))
		}
		device.mu.Lock()
		got := append([][]byte(nil), device.received...)
		device.mu.Unlock()
		if len(got) != 1 || !reflect.DeepEqual(got[0], payload) {
			t.Fatalf("plain device received = % x, want % x", got, payload)
		}
		if events := h.capture.events(); len(events) != 0 {
			t.Fatalf("device without optional tracer emitted events: %+v", events)
		}
	})
}
