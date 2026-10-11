package usb

import (
	"context"
	"testing"
	"time"

	rootusb "github.com/Alia5/VIIPER/usb"
)

func TestInterruptINWaitDurationSeparatesDeckPollAndReportCadence(t *testing.T) {
	dev := &waitBudgetTestDevice{
		descriptor: rootusb.Descriptor{Interfaces: []rootusb.InterfaceConfig{{
			Endpoints: []rootusb.EndpointDescriptor{
				{BEndpointAddress: 0x81, BMAttributes: 0x03, BInterval: 10},
				{BEndpointAddress: 0x82, BMAttributes: 0x03, BInterval: 6},
				{BEndpointAddress: 0x83, BMAttributes: 0x03, BInterval: 1},
				{BEndpointAddress: 0x85, BMAttributes: 0x03, BInterval: 2},
			},
		}}},
		overrides: map[uint32]time.Duration{3: 4 * time.Millisecond},
	}
	if got := interruptINWaitDuration(dev, 3); got != 4*time.Millisecond {
		t.Fatalf("device-specific controller wait budget = %s, want 4ms", got)
	}
	if got := interruptINWaitDuration(dev, 1); got != 10*time.Millisecond {
		t.Fatalf("Deck keyboard fallback = %s, want descriptor interval 10ms", got)
	}
	if got := interruptINWaitDuration(dev, 2); got != 6*time.Millisecond {
		t.Fatalf("Deck mouse fallback = %s, want descriptor interval 6ms", got)
	}
	if got := interruptINWaitDuration(dev, 4); got != 0 {
		t.Fatalf("unknown Deck endpoint wait = %s, want existing zero fallback", got)
	}
	if got := interruptINWaitDuration(dev, 5); got != 2*time.Millisecond {
		t.Fatalf("unrelated device wait budget = %s, want descriptor interval 2ms", got)
	}
}

type waitBudgetTestDevice struct {
	descriptor rootusb.Descriptor
	overrides  map[uint32]time.Duration
}

func (*waitBudgetTestDevice) HandleTransfer(context.Context, uint32, uint32, []byte) []byte {
	return nil
}

func (d *waitBudgetTestDevice) GetDescriptor() *rootusb.Descriptor { return &d.descriptor }

func (*waitBudgetTestDevice) GetDeviceSpecificArgs() map[string]any { return nil }

func (d *waitBudgetTestDevice) InterruptINWaitBudget(ep uint32) time.Duration {
	return d.overrides[ep]
}
