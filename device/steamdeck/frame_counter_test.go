package steamdeck

import (
	"encoding/binary"
	"sync"
	"testing"
)

func TestGETReportDoesNotAdvanceFrameOrConsumeNotification(t *testing.T) {
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
			if got := len(d.inputNotify); got != 1 {
				t.Fatalf("GET_REPORT consumed or duplicated the pending notification: channel length %d, want 1", got)
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

	if got := binary.LittleEndian.Uint32(d.currentInputReportInto(nil)[4:8]); got != ^uint32(0) {
		t.Fatalf("Frame before wrap = %d, want %d", got, ^uint32(0))
	}
	if got := binary.LittleEndian.Uint32(d.currentInputReportInto(nil)[4:8]); got != 0 {
		t.Fatalf("Frame after wrap = %d, want 0", got)
	}

	const readers = 16
	reports := make(chan []byte, readers)
	var wg sync.WaitGroup
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reports <- d.currentInputReportInto(nil)
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
