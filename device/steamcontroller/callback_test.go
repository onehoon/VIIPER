package steamcontroller

import (
	"sync/atomic"
	"testing"
)

func TestOutputCallbackClearPreventsLaterCapture(t *testing.T) {
	d, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	var calls atomic.Int32
	d.SetOutputCallback(func(OutputState) {
		calls.Add(1)
		close(entered)
		<-release
		close(finished)
	})

	go d.handleHostCommand([]byte{FeatureTriggerHapticCommand, 0x22})
	<-entered
	d.SetOutputCallback(nil)
	d.handleHostCommand([]byte{FeatureTriggerHapticCommand, 0x33})
	close(release)
	<-finished
	if got := calls.Load(); got != 1 {
		t.Fatalf("callback calls = %d, want 1", got)
	}
}
