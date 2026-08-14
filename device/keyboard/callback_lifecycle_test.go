package keyboard

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Alia5/VIIPER/usbip"
)

func TestLEDCallbackClearAndReentry(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	dev.SetLEDCallback(func(LEDState) {
		calls.Add(1)
		dev.SetLEDCallback(nil)
	})
	dev.HandleTransfer(context.Background(), 1, usbip.DirOut, []byte{LEDNumLock})
	if got := calls.Load(); got != 1 {
		t.Fatalf("callback calls = %d, want 1", got)
	}
	dev.HandleTransfer(context.Background(), 1, usbip.DirOut, []byte{LEDCapsLock})
	if got := calls.Load(); got != 1 {
		t.Fatalf("cleared callback calls = %d, want 1", got)
	}
}

func TestLEDCallbackClearDoesNotDrainInFlightCallback(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var calls atomic.Int64
	dev.SetLEDCallback(func(LEDState) {
		calls.Add(1)
		once.Do(func() { close(entered) })
		<-release
	})
	done := make(chan struct{})
	go func() {
		dev.HandleTransfer(context.Background(), 1, usbip.DirOut, []byte{LEDNumLock})
		close(done)
	}()
	<-entered
	dev.SetLEDCallback(nil)
	close(release)
	<-done
	dev.HandleTransfer(context.Background(), 1, usbip.DirOut, []byte{LEDCapsLock})
	if got := calls.Load(); got != 1 {
		t.Fatalf("callback calls after clear = %d, want 1", got)
	}
}

func TestLEDCallbackSetterRaceWithTransfer(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	callback := func(LEDState) { calls.Add(1) }
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 10000; i++ {
			if i%3 == 0 {
				dev.SetLEDCallback(nil)
			} else {
				dev.SetLEDCallback(callback)
			}
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 10000; i++ {
			dev.HandleTransfer(context.Background(), 1, usbip.DirOut, []byte{byte(i)})
		}
	}()
	close(start)
	wg.Wait()
	dev.SetLEDCallback(nil)
	_ = calls.Load()
}
