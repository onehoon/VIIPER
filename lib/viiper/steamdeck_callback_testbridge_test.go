//go:build cgo && viiper_testbridge

package main

import (
	"context"
	"reflect"
	"testing"

	"github.com/Alia5/VIIPER/device/steamdeck"
	"github.com/Alia5/VIIPER/usbip"
)

func TestSteamDeckExportedCOutputCallbackCopiesExactPayload(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9240)
	deck, err := steamdeck.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9240, deck, false)
	hw.lifecycleMu.Unlock()
	if !ok {
		t.Fatal("Steam Deck creation failed")
	}

	resetTestSteamDeckCOutputCallback()
	if !registerTestSteamDeckCOutputCallback(uintptr(h)) {
		t.Fatal("exported callback registration failed")
	}
	deck.HandleTransfer(context.Background(), 3, usbip.DirOut, []byte{0x00, 0x99, 0x01})
	handle, length, payload := testSteamDeckCOutputSnapshot()
	if handle != uintptr(h) || length != 2 || !reflect.DeepEqual(payload, []byte{0x99, 0x01}) {
		t.Fatalf("C callback handle=%x length=%d payload=%x", handle, length, payload)
	}
	for _, input := range [][]byte{
		make([]byte, steamdeck.InputReportLen),
		make([]byte, steamdeck.InputReportLen+1),
	} {
		for i := range input {
			input[i] = byte(i)
		}
		input[0] = 0x99
		resetTestSteamDeckCOutputCallback()
		deck.HandleTransfer(context.Background(), 3, usbip.DirOut, input)
		_, length, payload = testSteamDeckCOutputSnapshot()
		if length != steamdeck.InputReportLen || !reflect.DeepEqual(payload, input[:steamdeck.InputReportLen]) {
			t.Fatalf("bounded C callback length=%d payload=%x", length, payload)
		}
	}
	if !clearTestSteamDeckCOutputCallback(uintptr(h)) {
		t.Fatal("exported NULL callback clear failed")
	}

	resetTestSteamDeckCOutputCallback()
	deck.HandleTransfer(context.Background(), 3, usbip.DirOut, make([]byte, steamdeck.InputReportLen+1))
	_, length, payload = testSteamDeckCOutputSnapshot()
	if length != 0 || len(payload) != 0 {
		t.Fatalf("callback invoked after clear: length=%d payload=%x", length, payload)
	}
}
