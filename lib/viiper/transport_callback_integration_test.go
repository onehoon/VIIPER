package main

import (
	"bytes"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/device/steamcontroller"
	"github.com/Alia5/VIIPER/usbip"
)

func TestPublicRemoveWaitsForRealImportedCallbackAndAllowsReentry(t *testing.T) {
	hw, bus := newLifecycleTestServer(t, 9153)
	dev, err := steamcontroller.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9153, dev, false)
	hw.lifecycleMu.Unlock()
	if !ok {
		t.Fatal("device creation failed")
	}

	entered := make(chan struct{})
	allowReentry := make(chan struct{})
	clearStarted := make(chan struct{})
	release := make(chan struct{})
	reentryDone := make(chan struct{})
	var once sync.Once
	hw.onCallbackCleared = func(*deviceHandleWrapper) { close(clearStarted) }
	if !setSteamControllerOutputCallback(uintptr(h), func(steamcontroller.OutputState) {
		once.Do(func() { close(entered) })
		<-allowReentry
		_ = setSteamControllerOutputCallback(uintptr(h), nil)
		close(reentryDone)
		<-release
	}) {
		t.Fatal("callback registration failed")
	}

	serveDone := make(chan error, 1)
	go func() { serveDone <- hw.s.ListenAndServe() }()
	<-hw.s.Ready()
	conn, err := net.Dial("tcp", hw.s.Addr())
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
	var replyHeader [8]byte
	if _, err := io.ReadFull(conn, replyHeader[:]); err != nil {
		t.Fatal(err)
	}
	if got := uint16(replyHeader[2])<<8 | uint16(replyHeader[3]); got != usbip.OpRepImport {
		t.Fatalf("reply command = %#x, want OP_REP_IMPORT", got)
	}
	if got := uint32(replyHeader[4])<<24 | uint32(replyHeader[5])<<16 | uint32(replyHeader[6])<<8 | uint32(replyHeader[7]); got != 0 {
		t.Fatalf("reply status = %d, want success", got)
	}
	if _, err := io.CopyN(io.Discard, conn, int64(88+4*dev.GetDescriptor().NumInterfaces())); err != nil {
		t.Fatal(err)
	}

	payload := []byte{0x00, 0x08, 0x00, 0x11, 0x22, 0x00, 0x00, 0x00}
	cmd := usbip.CmdSubmit{Basic: usbip.HeaderBasic{Command: usbip.CmdSubmitCode, Seqnum: 1, Dir: usbip.DirOut, Ep: 3}, TransferBufferLen: uint32(len(payload))}
	if err := cmd.Write(conn); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("real imported callback did not start")
	}
	removeDone := make(chan bool, 1)
	go func() { removeDone <- removeSteamControllerDevice(uintptr(h)) }()
	select {
	case <-clearStarted:
	case <-time.After(time.Second):
		t.Fatal("RemoveSteamControllerDevice did not clear the callback")
	}
	close(allowReentry)
	select {
	case <-reentryDone:
	case <-time.After(time.Second):
		t.Fatal("callback re-entry deadlocked while Remove held lifecycle state")
	}
	select {
	case <-removeDone:
		t.Fatal("RemoveSteamControllerDevice returned while callback was blocked")
	default:
	}
	close(release)
	select {
	case ok := <-removeDone:
		if !ok {
			t.Fatal("RemoveSteamControllerDevice failed after callback release")
		}
	case <-time.After(time.Second):
		t.Fatal("RemoveSteamControllerDevice did not complete after callback release")
	}
	if lookupIdentityExists(uintptr(h)) {
		t.Fatal("removed handle remained valid")
	}
	_ = hw.s.Close()
	if err := <-serveDone; err != nil {
		t.Fatalf("server returned %v", err)
	}
}
