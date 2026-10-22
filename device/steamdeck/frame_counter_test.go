package steamdeck

import (
	"context"
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/usbip"
)

func TestGETReportDoesNotAdvanceFrameOrStartControllerClock(t *testing.T) {
	d, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}

	assertReadOnly := func() {
		t.Helper()
		for range 2 {
			report, handled := d.HandleControl(0xa1, 0x01, 0x0100, 0, InputReportLen, nil)
			if !handled || len(report) != InputReportLen {
				t.Fatalf("GET_REPORT handled/length = %v/%d, want true/%d", handled, len(report), InputReportLen)
			}
			if got := binary.LittleEndian.Uint32(report[4:8]); got != 0 {
				t.Fatalf("GET_REPORT Frame = %d before controller IN, want 0", got)
			}
			d.mtx.Lock()
			clockStarted := !d.reportClockOrigin.IsZero()
			d.mtx.Unlock()
			if clockStarted {
				t.Fatal("GET_REPORT started the controller report clock")
			}
		}
	}
	assertReadOnly()
	d.UpdateInputState(&InputState{Pitch: 1234, Frame: 0xdeadbeef})
	assertReadOnly()
}

func TestControllerINFrameWrapsAndSerializesConcurrentReports(t *testing.T) {
	d, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}

	d.mtx.Lock()
	d.frame = ^uint32(0) - 1
	d.mtx.Unlock()

	read := func() []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		return d.HandleTransfer(ctx, controllerEndpointNumber, usbip.DirIn, nil)
	}
	if got := binary.LittleEndian.Uint32(read()[4:8]); got != ^uint32(0) {
		t.Fatalf("Frame before wrap = %d, want %d", got, ^uint32(0))
	}
	if got := binary.LittleEndian.Uint32(read()[4:8]); got != 0 {
		t.Fatalf("Frame after wrap = %d, want 0", got)
	}

	const readers = 16
	reports := make(chan []byte, readers)
	var wg sync.WaitGroup
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reports <- read()
		}()
	}
	wg.Wait()
	close(reports)

	frames := make([]bool, readers+1)
	for report := range reports {
		frame := binary.LittleEndian.Uint32(report[4:8])
		if frame == 0 || frame > readers {
			t.Fatalf("concurrent report Frame = %d, want a unique value in 1..%d", frame, readers)
		}
		if frames[frame] {
			t.Fatalf("duplicate concurrent report Frame %d", frame)
		}
		frames[frame] = true
	}
	for frame := 1; frame <= readers; frame++ {
		seen := frames[frame]
		if !seen {
			t.Fatalf("concurrent report Frame %d was not generated", frame)
		}
	}
}

func TestSetStateDoesNotResetControllerReportGrid(t *testing.T) {
	d, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	read := func() []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		return d.HandleTransfer(ctx, controllerEndpointNumber, usbip.DirIn, nil)
	}
	if got := binary.LittleEndian.Uint32(read()[4:8]); got != 1 {
		t.Fatalf("first controller report Frame = %d, want 1", got)
	}
	d.mtx.Lock()
	origin, lastSlot := d.reportClockOrigin, d.reportClockLastSlot
	d.mtx.Unlock()
	if origin.IsZero() || lastSlot == 0 {
		t.Fatalf("first report did not initialize the grid: origin=%v slot=%d", origin, lastSlot)
	}

	d.UpdateInputState(&InputState{Pitch: 777})
	if got := binary.LittleEndian.Uint32(read()[4:8]); got != 2 {
		t.Fatalf("second controller report Frame = %d, want 2", got)
	}
	d.mtx.Lock()
	gotOrigin, gotSlot := d.reportClockOrigin, d.reportClockLastSlot
	d.mtx.Unlock()
	if !gotOrigin.Equal(origin) {
		t.Fatalf("SetState changed report-clock origin from %v to %v", origin, gotOrigin)
	}
	if gotSlot <= lastSlot {
		t.Fatalf("report slot did not advance: before=%d after=%d", lastSlot, gotSlot)
	}
}

func TestInvalidAndCancelledControllerReadsDoNotConsumeGridSlots(t *testing.T) {
	d, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.HandleInterruptIN(context.Background(), controllerEndpointNumber, make([]byte, InputReportLen-1)); got != nil {
		t.Fatalf("short controller buffer returned %d bytes, want nil", len(got))
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := d.HandleTransfer(cancelled, controllerEndpointNumber, usbip.DirIn, nil); got != nil {
		t.Fatalf("cancelled controller read returned %d bytes, want nil", len(got))
	}
	d.mtx.Lock()
	if !d.reportClockOrigin.IsZero() || d.reportClockLastSlot != 0 || d.frame != 0 {
		t.Fatalf("invalid reads initialized or consumed the clock: origin=%v slot=%d frame=%d", d.reportClockOrigin, d.reportClockLastSlot, d.frame)
	}
	d.mtx.Unlock()

	read := func(ctx context.Context) []byte {
		t.Helper()
		return d.HandleTransfer(ctx, controllerEndpointNumber, usbip.DirIn, nil)
	}
	firstCtx, cancelFirst := context.WithTimeout(context.Background(), time.Second)
	first := read(firstCtx)
	cancelFirst()
	if got := binary.LittleEndian.Uint32(first[4:8]); got != 1 {
		t.Fatalf("first valid report Frame = %d, want 1", got)
	}
	d.mtx.Lock()
	lastSlot := d.reportClockLastSlot
	d.mtx.Unlock()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Millisecond))
	if got := read(expired); got != nil {
		t.Fatalf("expired controller read returned report % x, want nil", got)
	}
	cancelExpired()
	d.mtx.Lock()
	gotSlot, gotFrame := d.reportClockLastSlot, d.frame
	d.mtx.Unlock()
	if gotSlot != lastSlot || gotFrame != 1 {
		t.Fatalf("expired read consumed state: slot %d->%d, Frame=%d", lastSlot, gotSlot, gotFrame)
	}
	secondCtx, cancelSecond := context.WithTimeout(context.Background(), time.Second)
	second := read(secondCtx)
	cancelSecond()
	if got := binary.LittleEndian.Uint32(second[4:8]); got != 2 {
		t.Fatalf("next valid report Frame = %d, want 2", got)
	}
}
