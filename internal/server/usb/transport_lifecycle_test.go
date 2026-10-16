package usb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	rootusb "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/Alia5/VIIPER/virtualbus"
)

type transportTestDevice struct{}

func (transportTestDevice) HandleTransfer(context.Context, uint32, uint32, []byte) []byte {
	return nil
}

func (transportTestDevice) GetDescriptor() *rootusb.Descriptor { return &rootusb.Descriptor{} }

func (transportTestDevice) GetDeviceSpecificArgs() map[string]any { return nil }

func TestIsClientDisconnectClassifiesExpectedCloseErrors(t *testing.T) {
	closedOp := &net.OpError{Op: "read", Net: "tcp", Err: net.ErrClosed}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "eof", err: io.EOF, want: true},
		{name: "closed network", err: net.ErrClosed, want: true},
		{name: "wrapped closed TCP operation", err: fmt.Errorf("read URB header: %w", closedOp), want: true},
		{name: "connection reset", err: &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, want: true},
		{name: "broken pipe", err: &net.OpError{Op: "write", Net: "tcp", Err: syscall.EPIPE}, want: true},
		{name: "unexpected I/O error", err: errors.New("unexpected transport failure"), want: false},
		{name: "protocol error", err: errors.New("protocol violation: invalid request"), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isClientDisconnect(tc.err); got != tc.want {
				t.Fatalf("isClientDisconnect(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
}

func TestUnknownImportBusIDRemainsDiagnosable(t *testing.T) {
	var logs bytes.Buffer
	s := New(ServerConfig{ConnectionTimeout: time.Hour}, slog.New(slog.NewTextHandler(&logs, nil)), nil)
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	result := make(chan error, 1)
	go func() { result <- s.handleConn(serverConn) }()

	var request bytes.Buffer
	if err := (&usbip.MgmtHeader{Version: usbip.Version, Command: usbip.OpReqImport}).Write(&request); err != nil {
		t.Fatal(err)
	}
	var busID [busIDSize]byte
	copy(busID[:], "missing-bus")
	_, _ = request.Write(busID[:])
	if _, err := clientConn.Write(request.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err == nil || !strings.Contains(err.Error(), "no device matches busid missing-bus") {
		t.Fatalf("unknown import error = %v, want diagnosable missing-bus failure", err)
	}
	if got := logs.String(); !strings.Contains(got, `msg="Import request"`) || !strings.Contains(got, "busid=missing-bus") {
		t.Fatalf("unknown import request log is not diagnosable: %s", got)
	}
}

func TestManagedCloseDrainsAcceptedConnections(t *testing.T) {
	s := New(ServerConfig{Addr: "127.0.0.1:0", ConnectionTimeout: time.Hour, ManagedTransportLifecycle: true}, slog.Default(), nil)
	serveDone := make(chan error, 1)
	go func() { serveDone <- s.ListenAndServe() }()
	<-s.Ready()

	conn, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	select {
	case <-s.managedConnectionRegistered:
	case <-time.After(time.Second):
		t.Fatal("accepted connection was not registered")
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got := len(s.managedConnections); got != 0 {
		t.Fatalf("managed connections after close = %d, want 0", got)
	}
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("ListenAndServe returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("accept loop did not terminate")
	}
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection remained readable after managed close")
	}
}

func TestBatchingWriterCloseJoinsFlushLoop(t *testing.T) {
	bw := newBatchingWriter(io.Discard, 128, time.Millisecond, 64)
	if _, err := bw.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-bw.doneCh:
	default:
		t.Fatal("flush loop still running after Close")
	}
}

func TestBatchingWriterCloseWithoutFlushWorker(t *testing.T) {
	bw := newBatchingWriter(io.Discard, 128, 0, 64)
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyServerDoesNotTrackManagedConnections(t *testing.T) {
	s := New(ServerConfig{Addr: "127.0.0.1:0", ConnectionTimeout: time.Hour}, slog.Default(), nil)
	if s.config.ManagedTransportLifecycle || s.config.DisableAutoBusCleanup {
		t.Fatal("zero-value server policy unexpectedly enabled managed transport")
	}
}

func TestManagedDeviceDrainRejectsLateBinding(t *testing.T) {
	s := New(ServerConfig{ManagedTransportLifecycle: true}, slog.Default(), nil)
	firstServer, firstClient := net.Pipe()
	defer firstClient.Close()
	defer firstServer.Close()
	s.transportMu.Lock()
	first := &managedConnection{conn: firstServer}
	s.managedConnections[first] = struct{}{}
	s.transportMu.Unlock()
	if err := s.bindManagedConnection(firstServer, nil); err != nil {
		t.Fatal(err)
	}

	drain := s.BeginDeviceDrain(nil)
	s.ForgetDeviceTransport(nil)

	secondServer, secondClient := net.Pipe()
	defer secondClient.Close()
	defer secondServer.Close()
	s.transportMu.Lock()
	second := &managedConnection{conn: secondServer}
	s.managedConnections[second] = struct{}{}
	s.transportMu.Unlock()
	if err := s.bindManagedConnection(secondServer, nil); err == nil {
		t.Fatal("late binding succeeded after device drain")
	}
	_ = firstServer.Close()
	go s.unbindManagedConnection(first)
	drain.Wait()
	s.unbindManagedConnection(second)
}

func TestManagedImportBindingReservationIsDrained(t *testing.T) {
	s := New(ServerConfig{ManagedTransportLifecycle: true}, slog.Default(), nil)
	bus, err := virtualbus.NewWithBusID(3043)
	if err != nil {
		t.Fatal(err)
	}
	defer s.RemoveBus(3043)
	dev := transportTestDevice{}
	if _, err := bus.Add(dev); err != nil {
		t.Fatal(err)
	}
	if err := s.AddBus(bus); err != nil {
		t.Fatal(err)
	}
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	mc := &managedConnection{conn: serverConn}
	s.transportMu.Lock()
	s.managedConnections[mc] = struct{}{}
	s.transportMu.Unlock()
	defer s.unbindManagedConnection(mc)

	chosen, _, _, err := s.bindManagedConnectionByBusID(serverConn, "3043-1")
	if err != nil || chosen != dev {
		t.Fatalf("managed import binding = %v, %v", chosen, err)
	}
	drain := s.BeginDeviceDrain(dev)
	done := make(chan struct{})
	go func() { drain.Wait(); close(done) }()
	select {
	case <-done:
		t.Fatal("drain completed while binding reservation was still active")
	default:
	}
	_ = serverConn.Close()
	s.unbindManagedConnection(mc)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("drain did not complete after binding unbound")
	}
}
