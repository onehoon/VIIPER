package usb

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	rootusb "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/Alia5/VIIPER/virtualbus"
)

var transportPolicyTestBusID atomic.Uint32

func init() {
	transportPolicyTestBusID.Store(45000)
}

type transportPolicyDevice struct {
	descriptor rootusb.Descriptor
	handle     func(context.Context, uint32, uint32, []byte) []byte
}

func (d *transportPolicyDevice) HandleTransfer(ctx context.Context, ep uint32, dir uint32, data []byte) []byte {
	return d.handle(ctx, ep, dir, data)
}

func (d *transportPolicyDevice) GetDescriptor() *rootusb.Descriptor { return &d.descriptor }

func (d *transportPolicyDevice) GetDeviceSpecificArgs() map[string]any { return nil }

func startTransportPolicyStream(t *testing.T, async bool, dev *transportPolicyDevice) (net.Conn, <-chan struct{}, <-chan error) {
	t.Helper()
	server := New(ServerConfig{
		Addr: "127.0.0.1:0", ConnectionTimeout: time.Hour,
		DisableAutoBusCleanup: true, AsyncNonEp0IN: async,
	}, slog.Default(), nil)
	busID := transportPolicyTestBusID.Add(1)
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
	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- server.handleUrbStream(serverConn, dev)
		close(done)
	}()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("URB stream did not stop during cleanup")
		}
		_ = server.RemoveBus(busID)
		_ = server.Close()
	})
	return clientConn, done, result
}

func policyTestDescriptor() rootusb.Descriptor {
	return rootusb.Descriptor{
		Device: rootusb.DeviceDescriptor{BNumConfigurations: 1, Speed: 3},
		Interfaces: []rootusb.InterfaceConfig{{
			Descriptor: rootusb.InterfaceDescriptor{BInterfaceNumber: 0, BNumEndpoints: 1},
			Endpoints:  []rootusb.EndpointDescriptor{{BEndpointAddress: 0x81, BMAttributes: 0x03, BInterval: 2}},
		}},
	}
}

func writePolicyTestSubmit(conn net.Conn, seq, dir, transferLen uint32, payload []byte) error {
	var request bytes.Buffer
	cmd := usbip.CmdSubmit{
		Basic:             usbip.HeaderBasic{Command: usbip.CmdSubmitCode, Seqnum: seq, Dir: dir, Ep: 1},
		TransferBufferLen: transferLen,
	}
	if err := cmd.Write(&request); err != nil {
		return err
	}
	_, _ = request.Write(payload)
	_, err := conn.Write(request.Bytes())
	return err
}

func readPolicyTestRet(conn net.Conn, payloadLen uint32) (uint32, int32, uint32, []byte, error) {
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	defer conn.SetReadDeadline(time.Time{})
	var header [retSubmitHeaderSize]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return 0, 0, 0, nil, err
	}
	command := binary.BigEndian.Uint32(header[0:4])
	seq := binary.BigEndian.Uint32(header[4:8])
	status := int32(binary.BigEndian.Uint32(header[20:24]))
	actualLen := binary.BigEndian.Uint32(header[24:28])
	if command != usbip.RetSubmitCode {
		return seq, status, actualLen, nil, errors.New("response is not RET_SUBMIT")
	}
	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return seq, status, actualLen, nil, err
	}
	return seq, status, actualLen, payload, nil
}

func TestSequentialNonEp0INBlocksLaterRequestUntilCompletion(t *testing.T) {
	inEntered := make(chan struct{})
	inRelease := make(chan struct{})
	outProcessed := make(chan struct{}, 1)
	var releaseOnce sync.Once
	dev := &transportPolicyDevice{descriptor: policyTestDescriptor()}
	dev.handle = func(_ context.Context, ep, dir uint32, _ []byte) []byte {
		if dir == usbip.DirIn && ep == 1 {
			close(inEntered)
			<-inRelease
			return []byte{0xA1}
		}
		if dir == usbip.DirOut {
			outProcessed <- struct{}{}
		}
		return nil
	}
	conn, done, result := startTransportPolicyStream(t, false, dev)
	t.Cleanup(func() { releaseOnce.Do(func() { close(inRelease) }) })
	if err := writePolicyTestSubmit(conn, 1, usbip.DirIn, 8, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-inEntered:
	case <-time.After(time.Second):
		t.Fatal("sequential IN transfer did not start")
	}

	var second bytes.Buffer
	secondCmd := usbip.CmdSubmit{
		Basic:             usbip.HeaderBasic{Command: usbip.CmdSubmitCode, Seqnum: 2, Dir: usbip.DirOut, Ep: 1},
		TransferBufferLen: 1,
	}
	if err := secondCmd.Write(&second); err != nil {
		t.Fatal(err)
	}
	second.WriteByte(0x7F)
	if err := conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	n, err := conn.Write(second.Bytes())
	if err == nil || n != 0 {
		t.Fatalf("later request write = (%d, %v), want blocked with no bytes consumed", n, err)
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("later request write error = %v, want deadline timeout", err)
	}
	select {
	case <-outProcessed:
		t.Fatal("later OUT request was processed before sequential IN completed")
	default:
	}

	releaseOnce.Do(func() { close(inRelease) })
	seq, status, actualLen, payload, err := readPolicyTestRet(conn, 1)
	if err != nil || seq != 1 || status != 0 || actualLen != 1 || !bytes.Equal(payload, []byte{0xA1}) {
		t.Fatalf("first RET_SUBMIT = seq %d status %d actualLen %d payload %x err %v", seq, status, actualLen, payload, err)
	}
	_ = conn.SetWriteDeadline(time.Time{})
	if err := writePolicyTestSubmit(conn, 2, usbip.DirOut, 1, []byte{0x7F}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-outProcessed:
	case <-time.After(time.Second):
		t.Fatal("later OUT request was not processed after sequential IN completed")
	}
	seq, status, actualLen, payload, err = readPolicyTestRet(conn, 0)
	if err != nil || seq != 2 || status != 0 || actualLen != 1 || len(payload) != 0 {
		t.Fatalf("second RET_SUBMIT = seq %d status %d actualLen %d payload %x err %v", seq, status, actualLen, payload, err)
	}
	_ = conn.Close()
	select {
	case <-done:
		if err := <-result; err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("URB stream returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("sequential URB stream did not stop")
	}
}

func TestAsyncNonEp0INAllowsLaterRequestBeforeCompletion(t *testing.T) {
	inEntered := make(chan struct{})
	inRelease := make(chan struct{})
	outProcessed := make(chan struct{}, 1)
	var releaseOnce sync.Once
	dev := &transportPolicyDevice{descriptor: policyTestDescriptor()}
	dev.handle = func(_ context.Context, ep, dir uint32, _ []byte) []byte {
		if dir == usbip.DirIn && ep == 1 {
			close(inEntered)
			<-inRelease
			return []byte{0xB2}
		}
		if dir == usbip.DirOut {
			outProcessed <- struct{}{}
		}
		return nil
	}
	conn, done, result := startTransportPolicyStream(t, true, dev)
	t.Cleanup(func() { releaseOnce.Do(func() { close(inRelease) }) })
	if err := writePolicyTestSubmit(conn, 1, usbip.DirIn, 8, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-inEntered:
	case <-time.After(time.Second):
		t.Fatal("async IN worker did not start")
	}
	if err := writePolicyTestSubmit(conn, 2, usbip.DirOut, 1, []byte{0x7F}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-outProcessed:
	case <-time.After(time.Second):
		t.Fatal("later OUT request was not processed while async IN remained blocked")
	}
	seq, status, actualLen, payload, err := readPolicyTestRet(conn, 0)
	if err != nil || seq != 2 || status != 0 || actualLen != 1 || len(payload) != 0 {
		t.Fatalf("later RET_SUBMIT = seq %d status %d actualLen %d payload %x err %v", seq, status, actualLen, payload, err)
	}

	releaseOnce.Do(func() { close(inRelease) })
	seq, status, actualLen, payload, err = readPolicyTestRet(conn, 1)
	if err != nil || seq != 1 || status != 0 || actualLen != 1 || !bytes.Equal(payload, []byte{0xB2}) {
		t.Fatalf("async IN RET_SUBMIT = seq %d status %d actualLen %d payload %x err %v", seq, status, actualLen, payload, err)
	}
	_ = conn.Close()
	select {
	case <-done:
		if err := <-result; err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("URB stream returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("async URB stream did not stop after worker completion")
	}
}

func TestSequentialNonEp0INIsOneShotAndNilIsEmptySuccess(t *testing.T) {
	var calls int
	var deadlines []bool
	var callsMu sync.Mutex
	dev := &transportPolicyDevice{descriptor: policyTestDescriptor()}
	dev.handle = func(ctx context.Context, ep, dir uint32, _ []byte) []byte {
		_, hasDeadline := ctx.Deadline()
		callsMu.Lock()
		calls++
		call := calls
		deadlines = append(deadlines, hasDeadline)
		callsMu.Unlock()
		if call == 1 {
			return []byte{0xC3}
		}
		return nil
	}
	conn, _, _ := startTransportPolicyStream(t, false, dev)
	if err := writePolicyTestSubmit(conn, 1, usbip.DirIn, 8, nil); err != nil {
		t.Fatal(err)
	}
	seq, status, actualLen, payload, err := readPolicyTestRet(conn, 1)
	if err != nil || seq != 1 || status != 0 || actualLen != 1 || !bytes.Equal(payload, []byte{0xC3}) {
		t.Fatalf("first sequential response = seq %d status %d actualLen %d payload %x err %v", seq, status, actualLen, payload, err)
	}
	if err := writePolicyTestSubmit(conn, 2, usbip.DirIn, 8, nil); err != nil {
		t.Fatal(err)
	}
	seq, status, actualLen, payload, err = readPolicyTestRet(conn, 0)
	if err != nil || seq != 2 || status != 0 || actualLen != 0 || len(payload) != 0 {
		t.Fatalf("nil sequential response = seq %d status %d actualLen %d payload %x err %v, want empty success", seq, status, actualLen, payload, err)
	}
	callsMu.Lock()
	defer callsMu.Unlock()
	if calls != 2 {
		t.Fatalf("processSubmit calls = %d, want exactly one per request and no retry", calls)
	}
	if len(deadlines) != 2 || deadlines[0] || deadlines[1] {
		t.Fatalf("sequential transfer deadlines = %v, want none", deadlines)
	}
}

func TestAsyncNonEp0INRetainsIntervalRetryAndCachedReplay(t *testing.T) {
	var calls int
	var callsMu sync.Mutex
	var deadlines []bool
	dev := &transportPolicyDevice{descriptor: policyTestDescriptor()}
	dev.handle = func(ctx context.Context, ep, dir uint32, _ []byte) []byte {
		callsMu.Lock()
		calls++
		call := calls
		_, hasDeadline := ctx.Deadline()
		deadlines = append(deadlines, hasDeadline)
		callsMu.Unlock()
		if call == 1 || call == 3 {
			<-ctx.Done()
			return nil
		}
		return []byte{0xD4}
	}
	conn, _, _ := startTransportPolicyStream(t, true, dev)
	if err := writePolicyTestSubmit(conn, 1, usbip.DirIn, 8, nil); err != nil {
		t.Fatal(err)
	}
	seq, status, actualLen, payload, err := readPolicyTestRet(conn, 1)
	if err != nil || seq != 1 || status != 0 || actualLen != 1 || !bytes.Equal(payload, []byte{0xD4}) {
		t.Fatalf("retried async response = seq %d status %d actualLen %d payload %x err %v", seq, status, actualLen, payload, err)
	}
	if err := writePolicyTestSubmit(conn, 2, usbip.DirIn, 8, nil); err != nil {
		t.Fatal(err)
	}
	seq, status, actualLen, payload, err = readPolicyTestRet(conn, 1)
	if err != nil || seq != 2 || status != 0 || actualLen != 1 || !bytes.Equal(payload, []byte{0xD4}) {
		t.Fatalf("cached async response = seq %d status %d actualLen %d payload %x err %v", seq, status, actualLen, payload, err)
	}
	callsMu.Lock()
	defer callsMu.Unlock()
	if calls != 3 {
		t.Fatalf("async processSubmit calls = %d, want timeout/retry plus cached replay (3 calls)", calls)
	}
	if len(deadlines) != 3 || !deadlines[0] || !deadlines[1] || !deadlines[2] {
		t.Fatalf("async transfer deadlines = %v, want interval deadlines on all attempts", deadlines)
	}
}
