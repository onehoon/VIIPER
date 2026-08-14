package dualsense

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Alia5/VIIPER/usbip"
)

func dualsenseTestOutputReport() []byte {
	report := make([]byte, 48)
	report[0] = ReportIDOutput
	report[2] = 0x04
	report[3] = 0x11
	report[4] = 0x22
	report[45] = 0x33
	report[46] = 0x44
	report[47] = 0x55
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
	report := dualsenseTestOutputReport()
	dev.HandleTransfer(context.Background(), EndpointOut, usbip.DirOut, report)
	if got := calls.Load(); got != 1 {
		t.Fatalf("callback calls = %d, want 1", got)
	}
	dev.HandleControl(0x21, 0x09, 0x0202, 0, 0, report)
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
	dev.SetOutputCallback(func(OutputState) {
		once.Do(func() { close(entered) })
		<-release
	})
	report := dualsenseTestOutputReport()
	done := make(chan struct{})
	go func() {
		dev.HandleTransfer(context.Background(), EndpointOut, usbip.DirOut, report)
		close(done)
	}()
	<-entered
	dev.SetOutputCallback(nil)
	close(release)
	<-done
	dev.HandleControl(0x21, 0x09, 0x0202, 0, 0, report)
}

func TestOutputCallbackSetterRaceWithTransferAndControl(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	callback := func(OutputState) { calls.Add(1) }
	report := dualsenseTestOutputReport()
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
			dev.HandleControl(0x21, 0x09, 0x0202, 0, 0, report)
		}
	}()
	close(start)
	wg.Wait()
	dev.SetOutputCallback(nil)
	_ = calls.Load()
}
