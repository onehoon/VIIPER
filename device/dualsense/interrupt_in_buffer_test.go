package dualsense

import (
	"bytes"
	"context"
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
)

func TestInterruptINBufferReuseAndLegacyAndEP0Ownership(t *testing.T) {
	d, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	d.SetMetaState(MetaState{BatteryStatus: 0x5a})
	buffered, ok := any(d).(usb.InterruptInBufferDevice)
	if !ok {
		t.Fatal("DualSense does not implement usb.InterruptInBufferDevice")
	}
	scratch := bytes.Repeat([]byte{0xcc}, InputReportSize+5)
	suffix := append([]byte(nil), scratch[InputReportSize:]...)
	checkReport := func(want []byte, seq byte) {
		t.Helper()
		got := buffered.HandleInterruptIN(context.Background(), 4, scratch)
		if len(got) != InputReportSize || cap(got) != InputReportSize {
			t.Fatalf("buffer report len/cap = %d/%d, want %d/%d", len(got), cap(got), InputReportSize, InputReportSize)
		}
		if &got[0] != &scratch[0] {
			t.Fatal("buffer report does not alias supplied scratch prefix")
		}
		want[7] = seq
		copy(want[28:32], got[28:32]) // timestamp is sampled from the live clock
		if !bytes.Equal(got, want) {
			t.Fatalf("buffer report = % x, want % x", got, want)
		}
		if !bytes.Equal(scratch[InputReportSize:], suffix) {
			t.Fatal("report writer modified trailing sentinel")
		}
	}

	neutral := neutralInputGolden(1)
	checkReport(neutral, 1)
	active := &InputState{
		LX: -128, LY: 127, RX: 0, RY: -1,
		Buttons: ButtonCross | ButtonPS | ButtonEdgeL4 | ButtonEdgeR4,
		DPad:    DPadUp | DPadLeft,
		L2:      0x12, R2: 0x34,
		Touch1X: 0x0123, Touch1Y: 0x0345, Touch1Active: true,
		Touch2X: 0xffff, Touch2Y: 0x0432,
		GyroX: -1234, GyroY: 0x2345, GyroZ: -1,
		AccelX: 0x1234, AccelY: -2345, AccelZ: -8192,
	}
	activeGolden := []byte{
		0x01, 0x00, 0xff, 0x80, 0x7f, 0x12, 0x34, 0x00,
		0x27, 0x00, 0xc1, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x2e, 0xfb, 0x45, 0x23, 0xff, 0xff, 0x34, 0x12,
		0xd7, 0xf6, 0x00, 0xe0, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x23, 0x51, 0x34, 0x80, 0x80, 0x27,
		0x43, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x10, 0x00, 0x00, 0x00, 0x5a, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	d.UpdateInputState(active)
	checkReport(activeGolden, 2)

	d.UpdateInputState(NewInputState())
	checkReport(neutralInputGolden(3), 3)
	d.UpdateInputState(active)
	checkReport(activeGolden, 4)

	d.UpdateInputState(active)
	legacy := d.HandleTransfer(context.Background(), 4, usbip.DirIn, nil)
	if len(legacy) != InputReportSize || &legacy[0] == &scratch[0] {
		t.Fatalf("legacy report length/ownership invalid: len=%d", len(legacy))
	}
	legacySnapshot := append([]byte(nil), legacy...)
	d.UpdateInputState(NewInputState())
	checkReport(neutralInputGolden(6), 6)
	d.UpdateInputState(active)
	legacySecond := d.HandleTransfer(context.Background(), 4, usbip.DirIn, nil)
	if len(legacySecond) != InputReportSize || &legacySecond[0] == &scratch[0] || &legacySecond[0] == &legacy[0] || legacySecond[7] != 7 {
		t.Fatal("successive legacy HandleTransfer reports do not own independent storage")
	}
	legacySecondSnapshot := append([]byte(nil), legacySecond...)
	if !bytes.Equal(legacy, legacySnapshot) {
		t.Fatal("second legacy report modified the first allocated report")
	}
	setupValue := uint16(reportTypeInput)<<8 | uint16(ReportIDInput)
	ep0, handled := d.HandleControl(hidClassIN, hidGetReport, setupValue, 0, InputReportSize, nil)
	if !handled || len(ep0) != InputReportSize || &ep0[0] == &scratch[0] || &ep0[0] == &legacy[0] || &ep0[0] == &legacySecond[0] {
		t.Fatalf("EP0 GET_REPORT handled/length/ownership = %v/%d", handled, len(ep0))
	}
	if ep0[7] != 8 {
		t.Fatalf("EP0 report sequence = %d, want 8", ep0[7])
	}
	ep0Snapshot := append([]byte(nil), ep0...)
	if !bytes.Equal(legacy, legacySnapshot) {
		t.Fatal("later buffer report modified legacy allocated bytes")
	}
	if !bytes.Equal(ep0, ep0Snapshot) {
		t.Fatal("later buffer report modified EP0 allocated bytes")
	}
	if !bytes.Equal(legacySecond, legacySecondSnapshot) {
		t.Fatal("later buffer report modified second legacy allocated bytes")
	}
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := buffered.HandleInterruptIN(cancelCtx, 4, scratch); got != nil {
		t.Fatalf("cancelled IN returned %d bytes, want nil", len(got))
	}
	d.UpdateInputState(NewInputState())
	deadlineCtx, cancelDeadline := context.WithDeadline(context.Background(), time.Now().Add(-time.Millisecond))
	deadlineReport := buffered.HandleInterruptIN(deadlineCtx, 4, scratch)
	cancelDeadline()
	if len(deadlineReport) != InputReportSize || deadlineReport[7] != 9 {
		t.Fatalf("deadline IN length/sequence = %d/%d, want %d/9", len(deadlineReport), deadlineReport[7], InputReportSize)
	}
	if got := buffered.HandleInterruptIN(context.Background(), 3, scratch); got != nil {
		t.Fatalf("unknown endpoint returned % x, want nil", got)
	}
}

func TestEdgeInterruptINAndEP0SequenceNumbersAreDistinctUnderOverlap(t *testing.T) {
	d, err := NewEdge(nil)
	if err != nil {
		t.Fatal(err)
	}
	buffered, ok := any(d).(usb.InterruptInBufferDevice)
	if !ok {
		t.Fatal("DualSense Edge does not implement usb.InterruptInBufferDevice")
	}
	var initialScratch [InputReportSize + 5]byte
	for i := range initialScratch {
		initialScratch[i] = 0xcc
	}
	initial := buffered.HandleInterruptIN(context.Background(), 4, initialScratch[:])
	if len(initial) != InputReportSize || cap(initial) != InputReportSize || &initial[0] != &initialScratch[0] {
		t.Fatalf("initial Edge report len/cap/ownership = %d/%d/%v", len(initial), cap(initial), len(initial) > 0 && &initial[0] == &initialScratch[0])
	}
	for _, b := range initialScratch[InputReportSize:] {
		if b != 0xcc {
			t.Fatal("Edge report writer changed bytes after its 64-byte prefix")
		}
	}
	d.UpdateInputState(NewInputState())
	deadlineCtx, cancelDeadline := context.WithDeadline(context.Background(), time.Now().Add(-time.Millisecond))
	deadlineReport := buffered.HandleInterruptIN(deadlineCtx, 4, initialScratch[:])
	cancelDeadline()
	if len(deadlineReport) != InputReportSize {
		t.Fatalf("Edge deadline report length = %d, want %d", len(deadlineReport), InputReportSize)
	}
	cancelledEdge, err := NewEdge(nil)
	if err != nil {
		t.Fatal(err)
	}
	cancelledBuffered, ok := any(cancelledEdge).(usb.InterruptInBufferDevice)
	if !ok {
		t.Fatal("DualSense Edge does not implement usb.InterruptInBufferDevice")
	}
	var cancelScratch [InputReportSize]byte
	if got := cancelledBuffered.HandleInterruptIN(context.Background(), 4, cancelScratch[:]); len(got) != InputReportSize {
		t.Fatalf("initial Edge report length = %d, want %d", len(got), InputReportSize)
	}
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := cancelledBuffered.HandleInterruptIN(cancelCtx, 4, cancelScratch[:]); got != nil {
		t.Fatalf("cancelled Edge read returned %d bytes, want nil", len(got))
	}
	const rounds = 100
	for i := 0; i < rounds; i++ {
		d.UpdateInputState(NewInputState())
		start := make(chan struct{})
		var wg sync.WaitGroup
		var interruptReport, controlReport []byte
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			var dst [InputReportSize + 5]byte
			for i := range dst {
				dst[i] = 0xcc
			}
			interruptReport = buffered.HandleInterruptIN(context.Background(), 4, dst[:])
			if len(interruptReport) != InputReportSize || cap(interruptReport) != InputReportSize || &interruptReport[0] != &dst[0] {
				return
			}
			for _, b := range dst[InputReportSize:] {
				if b != 0xcc {
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			value := uint16(reportTypeInput)<<8 | uint16(ReportIDInput)
			controlReport, _ = d.HandleControl(hidClassIN, hidGetReport, value, 0, InputReportSize, nil)
		}()
		close(start)
		wg.Wait()
		if len(interruptReport) != InputReportSize || len(controlReport) != InputReportSize {
			t.Fatalf("round %d report lengths = %d/%d", i, len(interruptReport), len(controlReport))
		}
		if interruptReport[7] == controlReport[7] {
			t.Fatalf("round %d concurrent report sequence numbers both equal %d", i, interruptReport[7])
		}
		if interruptReport[7]+1 != controlReport[7] && controlReport[7]+1 != interruptReport[7] {
			t.Fatalf("round %d concurrent sequence numbers = %d/%d, want adjacent values", i, interruptReport[7], controlReport[7])
		}
	}
}

func neutralInputGolden(seq byte) []byte {
	accelX, accelY, accelZ := DefaultAccelRaw()
	want := make([]byte, InputReportSize)
	want[0] = ReportIDInput
	want[1], want[2], want[3], want[4] = 128, 128, 128, 128
	want[7] = seq
	want[8] = DPadUSBNeutral
	binary.LittleEndian.PutUint16(want[22:24], uint16(accelX))
	binary.LittleEndian.PutUint16(want[24:26], uint16(accelY))
	binary.LittleEndian.PutUint16(want[26:28], uint16(accelZ))
	want[33] = TouchInactiveMask
	want[37] = TouchInactiveMask
	want[49] = 0x10
	want[53] = 0x5a
	return want
}
