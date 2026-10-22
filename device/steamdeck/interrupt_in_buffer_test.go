package steamdeck

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/device"
	"github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
)

func TestInterruptINBufferReuseEndpointsAndLegacyAllocation(t *testing.T) {
	d, err := New((*device.CreateOptions)(nil))
	if err != nil {
		t.Fatal(err)
	}
	buffered, ok := any(d).(usb.InterruptInBufferDevice)
	if !ok {
		t.Fatal("Steam Deck does not implement usb.InterruptInBufferDevice")
	}
	scratch := bytes.Repeat([]byte{0xcc}, 72)
	suffix := append([]byte(nil), scratch[InputReportLen:]...)
	checkReport := func(ep uint32, want []byte) []byte {
		t.Helper()
		wait := time.Second
		if ep == mouseEndpointNumber || ep == keyboardEndpointNumber {
			wait = 20 * time.Millisecond
		}
		ctx, cancel := context.WithTimeout(context.Background(), wait)
		defer cancel()
		got := buffered.HandleInterruptIN(ctx, ep, scratch)
		if len(got) != len(want) || cap(got) != len(want) {
			t.Fatalf("endpoint %d report len/cap = %d/%d, want %d/%d", ep, len(got), cap(got), len(want), len(want))
		}
		if &got[0] != &scratch[0] {
			t.Fatalf("endpoint %d report does not alias scratch", ep)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("endpoint %d report mismatch", ep)
		}
		if !bytes.Equal(scratch[InputReportLen:], suffix) {
			t.Fatal("buffer writer modified trailing sentinel")
		}
		return got
	}

	if got := checkReport(mouseEndpointNumber, zeroMouseReport); !bytes.Equal(got, []byte{0, 0, 0, 0}) {
		t.Fatal("mouse endpoint was not the fixed four-byte zero report")
	}
	checkReport(keyboardEndpointNumber, zeroKeyboardReport)
	beforeControllerIN, handled := d.HandleControl(0xa1, 0x01, 0x0100, 0, InputReportLen, nil)
	if !handled || len(beforeControllerIN) != InputReportLen {
		t.Fatalf("GET_REPORT before controller IN handled/length = %v/%d, want true/%d", handled, len(beforeControllerIN), InputReportLen)
	}
	if got := binaryFrame(beforeControllerIN); got != 0 {
		t.Fatalf("auxiliary IN endpoints advanced controller Frame to %d, want 0", got)
	}
	neutral := (&InputState{}).buildReport(1)
	checkReport(controllerEndpointNumber, neutral)

	active := InputState{
		A: true, X: true, B: true, Y: true, L1: true, R1: true, L2Digital: true, R2Digital: true,
		L5: true, Menu: true, Steam: true, Options: true, DPadDown: true, DPadLeft: true, DPadRight: true, DPadUp: true,
		L3: true, RPadTouch: true, LPadTouch: true, RPadPress: true, LPadPress: true, R5: true, R3: true,
		RStickTouch: true, LStickTouch: true, R4: true, L4: true, QuickAccess: true,
		Reserved15: 0xa5,
		LPadX:      -32768, LPadY: -1, RPadX: 0x1234, RPadY: 32767,
		AccelX: -1, AccelY: 0x2345, AccelZ: -32768,
		Pitch: 1, Yaw: -2, Roll: 0x3456,
		GyroQuatW: 0x4567, GyroQuatX: -1, GyroQuatY: 0x6789, GyroQuatZ: -32768,
		LTrigger: 0, RTrigger: 0xffff,
		LStickX: -1, LStickY: -32768, RStickX: 0x2345, RStickY: 32767,
		LPadForce: 0x1112, RPadForce: 0x3334, LStickForce: 0x5556, RStickForce: 0x7778,
	}
	activeGolden := []byte{
		0x01, 0x00, 0x09, 0x40, 0x01, 0x00, 0x00, 0x00,
		0xff, 0xff, 0x5f, 0x04, 0x00, 0xc6, 0x04, 0xa5,
		0x00, 0x80, 0xff, 0xff, 0x34, 0x12, 0xff, 0x7f,
		0xff, 0xff, 0x45, 0x23, 0x00, 0x80, 0x01, 0x00,
		0xfe, 0xff, 0x56, 0x34, 0x67, 0x45, 0xff, 0xff,
		0x89, 0x67, 0x00, 0x80, 0x00, 0x00, 0xff, 0xff,
		0xff, 0xff, 0x00, 0x80, 0x45, 0x23, 0xff, 0x7f,
		0x12, 0x11, 0x34, 0x33, 0x56, 0x55, 0x78, 0x77,
	}
	activeGoldenForFrame := func(frame uint32) []byte {
		want := append([]byte(nil), activeGolden...)
		binary.LittleEndian.PutUint32(want[4:8], frame)
		return want
	}
	d.UpdateInputState(&active)
	checkReport(controllerEndpointNumber, activeGoldenForFrame(2))

	d.UpdateInputState(&InputState{})
	shortCtx, cancelShort := context.WithDeadline(context.Background(), time.Now().Add(-time.Millisecond))
	shortReport := buffered.HandleInterruptIN(shortCtx, controllerEndpointNumber, scratch[:InputReportLen-1])
	cancelShort()
	if shortReport != nil {
		t.Fatalf("short controller buffer returned %d bytes, want nil", len(shortReport))
	}
	getReport, handled := d.HandleControl(0xa1, 0x01, 0x0100, 0, InputReportLen, nil)
	if !handled || len(getReport) != InputReportLen || binaryFrame(getReport) != 2 {
		var gotFrame uint32
		if len(getReport) >= 8 {
			gotFrame = binaryFrame(getReport)
		}
		t.Fatalf("short controller buffer advanced Frame to %d (handled=%v, length=%d), want 2", gotFrame, handled, len(getReport))
	}
	deadlineAfterShort, cancelDeadlineAfterShort := context.WithTimeout(context.Background(), time.Second)
	neutralWant := buffered.HandleInterruptIN(deadlineAfterShort, controllerEndpointNumber, scratch)
	cancelDeadlineAfterShort()
	if len(neutralWant) != InputReportLen || binaryFrame(neutralWant) != 3 {
		t.Fatalf("report after short buffer length/frame = %d/%d, want %d/3", len(neutralWant), binaryFrame(neutralWant), InputReportLen)
	}
	d.UpdateInputState(&active)
	checkReport(controllerEndpointNumber, activeGoldenForFrame(4))

	// A cancelled/expired URB does not consume a report slot or Frame.
	d.UpdateInputState(&active)
	deadlineCtx, cancelDeadline := context.WithDeadline(context.Background(), time.Now().Add(-time.Millisecond))
	deadlineReport := buffered.HandleInterruptIN(deadlineCtx, controllerEndpointNumber, scratch)
	cancelDeadline()
	if deadlineReport != nil {
		t.Fatalf("expired controller request returned report % x, want nil", deadlineReport)
	}
	getReport, handled = d.HandleControl(0xa1, 0x01, 0x0100, 0, InputReportLen, nil)
	if !handled || binaryFrame(getReport) != 4 {
		t.Fatalf("expired controller request advanced GET_REPORT Frame to %d, want 4", binaryFrame(getReport))
	}
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := buffered.HandleInterruptIN(cancelCtx, controllerEndpointNumber, scratch); got != nil {
		t.Fatalf("cancelled controller read returned %d bytes, want nil", len(got))
	}

	d.UpdateInputState(&active)
	legacyCtx, cancelLegacy := context.WithTimeout(context.Background(), time.Second)
	legacy := d.HandleTransfer(legacyCtx, controllerEndpointNumber, usbip.DirIn, nil)
	cancelLegacy()
	if len(legacy) != InputReportLen || binaryFrame(legacy) != 5 || &legacy[0] == &scratch[0] {
		t.Fatalf("legacy report length/ownership invalid: len=%d", len(legacy))
	}
	legacySnapshot := append([]byte(nil), legacy...)
	d.UpdateInputState(&InputState{B: true})
	legacySecondCtx, cancelLegacySecond := context.WithTimeout(context.Background(), time.Second)
	legacySecond := d.HandleTransfer(legacySecondCtx, controllerEndpointNumber, usbip.DirIn, nil)
	cancelLegacySecond()
	if len(legacySecond) != InputReportLen || binaryFrame(legacySecond) != 6 || &legacySecond[0] == &legacy[0] || &legacySecond[0] == &scratch[0] {
		t.Fatal("successive legacy HandleTransfer reports share storage")
	}
	if !bytes.Equal(legacy, legacySnapshot) {
		t.Fatal("second legacy report modified the first allocated report")
	}
	legacySecondSnapshot := append([]byte(nil), legacySecond...)
	ep0, handled := d.HandleControl(0xa1, 0x01, 0x0100, 0, InputReportLen, nil)
	if !handled || len(ep0) != InputReportLen || &ep0[0] == &scratch[0] || &ep0[0] == &legacy[0] || &ep0[0] == &legacySecond[0] {
		t.Fatalf("EP0 GET_REPORT handled/length/ownership = %v/%d", handled, len(ep0))
	}
	ep0Snapshot := append([]byte(nil), ep0...)
	d.UpdateInputState(&InputState{})
	checkReport(controllerEndpointNumber, (&InputState{}).buildReport(7))
	if !bytes.Equal(legacy, legacySnapshot) {
		t.Fatal("later buffer report modified legacy allocated storage")
	}
	if !bytes.Equal(ep0, ep0Snapshot) {
		t.Fatal("later buffer report modified EP0 allocated storage")
	}
	if !bytes.Equal(legacySecond, legacySecondSnapshot) {
		t.Fatal("later buffer report modified second legacy allocated storage")
	}
	if got := buffered.HandleInterruptIN(context.Background(), 4, scratch); got != nil {
		t.Fatalf("unknown endpoint returned % x, want nil", got)
	}
	d.HandleTransfer(context.Background(), controllerEndpointNumber, usbip.DirOut, []byte{FeatureTriggerHapticCommand})
	finalReport, handled := d.HandleControl(0xa1, 0x01, 0x0100, 0, InputReportLen, nil)
	if !handled || len(finalReport) != InputReportLen {
		t.Fatalf("GET_REPORT after non-IN requests handled/length = %v/%d, want true/%d", handled, len(finalReport), InputReportLen)
	}
	if got := binaryFrame(finalReport); got != 7 {
		t.Fatalf("non-IN controller requests changed Frame to %d, want 7", got)
	}
}

type doneObservedContext struct {
	context.Context
	doneCalled chan struct{}
}

func (c *doneObservedContext) Done() <-chan struct{} {
	select {
	case c.doneCalled <- struct{}{}:
	default:
	}
	return c.Context.Done()
}

func TestAuxiliaryINWaitsForDeadlineOnBothHandlerPaths(t *testing.T) {
	tests := []struct {
		name string
		ep   uint32
		want []byte
	}{
		{name: "mouse", ep: mouseEndpointNumber, want: zeroMouseReport},
		{name: "keyboard", ep: keyboardEndpointNumber, want: zeroKeyboardReport},
	}

	for _, tt := range tests {
		for _, useBuffer := range []bool{false, true} {
			path := "HandleTransfer"
			if useBuffer {
				path = "HandleInterruptIN"
			}
			t.Run(tt.name+"/"+path, func(t *testing.T) {
				d, err := New((*device.CreateOptions)(nil))
				if err != nil {
					t.Fatal(err)
				}
				baseCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
				started := time.Now()
				var got []byte
				if useBuffer {
					got = d.HandleInterruptIN(baseCtx, tt.ep, bytes.Repeat([]byte{0xcc}, 64))
				} else {
					got = d.HandleTransfer(baseCtx, tt.ep, usbip.DirIn, nil)
				}
				if elapsed := time.Since(started); elapsed < 50*time.Millisecond {
					t.Fatalf("endpoint %d completed after %v, want it to wait for its context deadline", tt.ep, elapsed)
				}
				if !bytes.Equal(got, tt.want) {
					t.Fatalf("endpoint %d report = % x, want % x", tt.ep, got, tt.want)
				}
			})
		}
	}
}

func TestAuxiliaryINCancellationReturnsNoReportOnBothHandlerPaths(t *testing.T) {
	tests := []struct {
		name string
		ep   uint32
	}{
		{name: "mouse", ep: mouseEndpointNumber},
		{name: "keyboard", ep: keyboardEndpointNumber},
	}

	for _, tt := range tests {
		for _, useBuffer := range []bool{false, true} {
			path := "HandleTransfer"
			if useBuffer {
				path = "HandleInterruptIN"
			}
			t.Run(tt.name+"/"+path, func(t *testing.T) {
				d, err := New((*device.CreateOptions)(nil))
				if err != nil {
					t.Fatal(err)
				}
				baseCtx, cancel := context.WithCancel(context.Background())
				defer cancel()
				ctx := &doneObservedContext{Context: baseCtx, doneCalled: make(chan struct{}, 1)}
				result := make(chan []byte, 1)
				go func() {
					if useBuffer {
						result <- d.HandleInterruptIN(ctx, tt.ep, bytes.Repeat([]byte{0xcc}, 64))
						return
					}
					result <- d.HandleTransfer(ctx, tt.ep, usbip.DirIn, nil)
				}()

				select {
				case <-ctx.doneCalled:
				case got := <-result:
					t.Fatalf("endpoint %d returned before waiting for cancellation: % x", tt.ep, got)
				case <-time.After(time.Second):
					t.Fatalf("endpoint %d handler did not begin waiting on its context", tt.ep)
				}
				cancel()
				select {
				case got := <-result:
					if got != nil {
						t.Fatalf("cancelled endpoint %d returned report % x, want nil", tt.ep, got)
					}
				case <-time.After(time.Second):
					t.Fatalf("cancelled endpoint %d handler did not exit", tt.ep)
				}
			})
		}
	}
}

func binaryFrame(report []byte) uint32 {
	return uint32(report[4]) | uint32(report[5])<<8 | uint32(report[6])<<16 | uint32(report[7])<<24
}
