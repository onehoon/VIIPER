package usb

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

func TestCachedINResponseSnapshotSurvivesOverwriteBeforeConsumption(t *testing.T) {
	var respMu sync.Mutex
	responses := make(map[uint32][]byte)
	first := bytes.Repeat([]byte{0x31}, 64)
	storeINResponse(&respMu, responses, 3, first)

	scratch := make([]byte, 64)
	snapshotReady := make(chan struct{})
	consume := make(chan struct{})
	consumed := make(chan []byte, 1)
	go func() {
		selected, ok := copyCachedINResponse(&respMu, responses, 3, scratch)
		if !ok {
			consumed <- nil
			return
		}
		close(snapshotReady)
		<-consume
		consumed <- append([]byte(nil), selected...)
	}()
	select {
	case <-snapshotReady:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for cached response snapshot")
	}

	// Model a later fresh URB overwriting the reused cache while the replay
	// response has been selected but has not yet reached the writer.
	storeINResponse(&respMu, responses, 3, bytes.Repeat([]byte{0x72}, 64))
	close(consume)
	got := <-consumed
	if !bytes.Equal(got, first) {
		t.Fatalf("selected replay changed after cache overwrite: got prefix % x, want % x", got[:min(8, len(got))], first[:8])
	}
	if len(got) != len(first) {
		t.Fatalf("selected replay length = %d, want %d", len(got), len(first))
	}
}

func TestCachedINResponseLargerThanScratchIsCopiedWithoutTruncation(t *testing.T) {
	var respMu sync.Mutex
	responses := make(map[uint32][]byte)
	want := make([]byte, 100)
	for i := range want {
		want[i] = byte(i)
	}
	storeINResponse(&respMu, responses, 1, want)

	scratch := make([]byte, 64)
	got, ok := copyCachedINResponse(&respMu, responses, 1, scratch)
	if !ok {
		t.Fatal("cached response was not found")
	}
	if len(got) != len(want) || !bytes.Equal(got, want) {
		t.Fatalf("large cached response length/data mismatch: got %d bytes, want %d", len(got), len(want))
	}
	storeINResponse(&respMu, responses, 1, bytes.Repeat([]byte{0xaa}, len(want)))
	if !bytes.Equal(got, want) {
		t.Fatal("large cached response aliases mutable cache storage")
	}
}

func TestStoreINResponseReusesCacheCapacity(t *testing.T) {
	var respMu sync.Mutex
	responses := make(map[uint32][]byte)
	storeINResponse(&respMu, responses, 2, []byte{1, 2, 3, 4})
	first := &responses[2][0]
	storeINResponse(&respMu, responses, 2, []byte{5, 6, 7})
	if got := &responses[2][0]; got != first {
		t.Fatal("cache storage was not reused for a shorter report")
	}
	if !bytes.Equal(responses[2], []byte{5, 6, 7}) {
		t.Fatalf("cached report = % x, want 05 06 07", responses[2])
	}
}
