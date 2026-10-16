package xbox360

import (
	"bytes"
	"context"
	"testing"

	"github.com/Alia5/VIIPER/device"
	"github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
)

func TestInterruptINBufferReuseAndLegacyAllocation(t *testing.T) {
	d, err := New((*device.CreateOptions)(nil))
	if err != nil {
		t.Fatal(err)
	}
	buffered, ok := any(d).(usb.InterruptInBufferDevice)
	if !ok {
		t.Fatal("Xbox360 does not implement usb.InterruptInBufferDevice")
	}

	scratch := bytes.Repeat([]byte{0xcc}, 72)
	suffix := append([]byte(nil), scratch[xbox360InputReportLen:]...)
	checkBuffered := func(want []byte) []byte {
		t.Helper()
		got := buffered.HandleInterruptIN(context.Background(), 1, scratch)
		if len(got) != xbox360InputReportLen || cap(got) != xbox360InputReportLen {
			t.Fatalf("buffer report len/cap = %d/%d, want %d/%d", len(got), cap(got), xbox360InputReportLen, xbox360InputReportLen)
		}
		if &got[0] != &scratch[0] {
			t.Fatal("buffer report does not alias the supplied scratch prefix")
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("buffer report = % x, want % x", got, want)
		}
		if !bytes.Equal(scratch[xbox360InputReportLen:], suffix) {
			t.Fatal("buffer writer modified bytes after the report")
		}
		return got
	}

	neutral := []byte{0x00, 0x14, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	checkBuffered(neutral)
	activeState := InputState{Buttons: 0x0000beef, LT: 0x12, RT: 0x34, LX: -2, LY: 0x1234, RX: -1, RY: 1}
	active := []byte{0, 0x14, 0xef, 0xbe, 0x12, 0x34, 0xfe, 0xff, 0x34, 0x12, 0xff, 0xff, 1, 0, 0, 0, 0, 0, 0, 0}
	d.UpdateInputState(activeState)
	checkBuffered(active)
	d.UpdateInputState(InputState{})
	checkBuffered(neutral)
	d.UpdateInputState(activeState)
	checkBuffered(active)

	d.UpdateInputState(InputState{})
	legacy := d.HandleTransfer(context.Background(), 1, usbip.DirIn, nil)
	if len(legacy) != xbox360InputReportLen || &legacy[0] == &scratch[0] {
		t.Fatalf("legacy report length/ownership invalid: len=%d", len(legacy))
	}
	legacySnapshot := append([]byte(nil), legacy...)
	d.UpdateInputState(activeState)
	legacySecond := d.HandleTransfer(context.Background(), 1, usbip.DirIn, nil)
	if len(legacySecond) != xbox360InputReportLen || &legacySecond[0] == &legacy[0] || &legacySecond[0] == &scratch[0] {
		t.Fatal("successive legacy HandleTransfer reports share storage")
	}
	if !bytes.Equal(legacySecond, active) {
		t.Fatalf("second legacy report = % x, want active golden % x", legacySecond, active)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := buffered.HandleInterruptIN(ctx, 1, scratch); got != nil {
		t.Fatalf("cancelled IN returned %d bytes, want nil", len(got))
	}
	if !bytes.Equal(legacy, legacySnapshot) {
		t.Fatal("later buffered report modified legacy allocated storage")
	}
	if got := buffered.HandleInterruptIN(context.Background(), 2, scratch); got != nil {
		t.Fatalf("unknown endpoint returned % x, want nil", got)
	}
}
