package usb

import (
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

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
