package xbox360

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Alia5/VIIPER/usbip"
)

func TestRumbleCallbackClearAndReentry(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	dev.SetRumbleCallback(func(XRumbleState) {
		calls.Add(1)
		dev.SetRumbleCallback(nil)
	})
	packet := []byte{0x00, 0x08, 0x00, 0x11, 0x22, 0x00, 0x00, 0x00}
	dev.HandleTransfer(context.Background(), 1, usbip.DirOut, packet)
	if got := calls.Load(); got != 1 {
		t.Fatalf("callback calls = %d, want 1", got)
	}
	dev.HandleTransfer(context.Background(), 1, usbip.DirOut, packet)
	if got := calls.Load(); got != 1 {
		t.Fatalf("cleared callback calls = %d, want 1", got)
	}
}

func TestRumbleCallbackClearDoesNotDrainInFlightCallback(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var calls atomic.Int64
	dev.SetRumbleCallback(func(XRumbleState) {
		calls.Add(1)
		once.Do(func() { close(entered) })
		<-release
	})
	packet := []byte{0x00, 0x08, 0x00, 0x11, 0x22, 0x00, 0x00, 0x00}
	done := make(chan struct{})
	go func() {
		dev.HandleTransfer(context.Background(), 1, usbip.DirOut, packet)
		close(done)
	}()
	<-entered
	dev.SetRumbleCallback(nil)
	close(release)
	<-done
	dev.HandleTransfer(context.Background(), 1, usbip.DirOut, packet)
	if got := calls.Load(); got != 1 {
		t.Fatalf("callback calls after clear = %d, want 1", got)
	}
}

func TestRumbleCallbackSetterRaceWithTransfer(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	callback := func(XRumbleState) { calls.Add(1) }
	packet := []byte{0x00, 0x08, 0x00, 0x11, 0x22, 0x00, 0x00, 0x00}
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 10000; i++ {
			if i%3 == 0 {
				dev.SetRumbleCallback(nil)
			} else {
				dev.SetRumbleCallback(callback)
			}
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 10000; i++ {
			dev.HandleTransfer(context.Background(), 1, usbip.DirOut, packet)
		}
	}()
	close(start)
	wg.Wait()
	dev.SetRumbleCallback(nil)
	_ = calls.Load()
}
