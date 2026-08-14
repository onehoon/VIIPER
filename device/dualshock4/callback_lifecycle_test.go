package dualshock4

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Alia5/VIIPER/usbip"
)

func ds4TestOutputReport() []byte {
	report := make([]byte, 11)
	report[0] = ReportIDOutput
	report[4] = 0x11
	report[5] = 0x22
	report[6] = 0x33
	report[7] = 0x44
	report[8] = 0x55
	report[9] = 0x66
	report[10] = 0x77
	return report
}

func TestOutputCallbackClearAndReentry(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	dev.SetOutputCallback(func(OutputState) {
		calls.Add(1)
		dev.SetOutputCallback(nil)
	})
	report := ds4TestOutputReport()
	dev.HandleTransfer(context.Background(), EndpointOut, usbip.DirOut, report)
	if got := calls.Load(); got != 1 {
		t.Fatalf("callback calls = %d, want 1", got)
	}
	dev.HandleControl(0x21, 0x09, 0x0205, 0, 0, report)
	if got := calls.Load(); got != 1 {
		t.Fatalf("cleared callback calls = %d, want 1", got)
	}
}

func TestOutputCallbackClearDoesNotDrainInFlightCallback(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var calls atomic.Int64
	dev.SetOutputCallback(func(OutputState) {
		calls.Add(1)
		once.Do(func() { close(entered) })
		<-release
	})
	report := ds4TestOutputReport()
	done := make(chan struct{})
	go func() {
		dev.HandleTransfer(context.Background(), EndpointOut, usbip.DirOut, report)
		close(done)
	}()
	<-entered
	dev.SetOutputCallback(nil)
	close(release)
	<-done
	dev.HandleControl(0x21, 0x09, 0x0205, 0, 0, report)
	if got := calls.Load(); got != 1 {
		t.Fatalf("callback calls after clear = %d, want 1", got)
	}
}

func TestOutputCallbackSetterRaceWithTransferAndControl(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	callback := func(OutputState) { calls.Add(1) }
	report := ds4TestOutputReport()
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 10000; i++ {
			if i%3 == 0 {
				dev.SetOutputCallback(nil)
			} else {
				dev.SetOutputCallback(callback)
			}
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 10000; i++ {
			dev.HandleTransfer(context.Background(), EndpointOut, usbip.DirOut, report)
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 10000; i++ {
			dev.HandleControl(0x21, 0x09, 0x0205, 0, 0, report)
		}
	}()
	close(start)
	wg.Wait()
	dev.SetOutputCallback(nil)
	_ = calls.Load()
}
