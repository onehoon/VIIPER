package dualsense

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/Alia5/VIIPER/usbip"
)

func makeUSBOutputReport(flag0, flag1, flag2, motorRight, motorLeft byte) []byte {
	report := make([]byte, 48)
	report[0] = ReportIDOutput
	report[1] = flag0
	report[2] = flag1
	report[3] = motorRight
	report[4] = motorLeft
	report[39] = flag2
	return report
}

func TestOutputReportRumbleValidityAcrossInputPaths(t *testing.T) {
	paths := []struct {
		name    string
		control bool
		send    func(*DualSense, []byte) ([]byte, bool)
	}{
		{
			name: "interrupt OUT",
			send: func(d *DualSense, report []byte) ([]byte, bool) {
				d.HandleTransfer(context.Background(), EndpointOut, usbip.DirOut, report)
				return nil, false
			},
		},
		{
			name:    "HID SET_REPORT",
			control: true,
			send: func(d *DualSense, report []byte) ([]byte, bool) {
				return d.HandleControl(hidClassOUT, hidSetReport, 0x0202, 0, uint16(len(report)), report)
			},
		},
	}

	steps := []struct {
		name   string
		report []byte
		state  *OutputState
	}{
		{
			name:   "fresh LED-only report is ignored",
			report: makeUSBOutputReport(0, 0x04, 0, 0xAA, 0xBB),
		},
		{
			name:   "v1 compatible vibration updates motors",
			report: makeUSBOutputReport(0x01, 0, 0, 0x11, 0x22),
			state:  &OutputState{RumbleSmall: 0x11, RumbleLarge: 0x22},
		},
		{
			name:   "LED-only after rumble is ignored",
			report: makeUSBOutputReport(0, 0x04, 0, 0, 0),
		},
		{
			name: "player LED-only report is ignored",
			report: func() []byte {
				report := makeUSBOutputReport(0, 0x10, 0, 0x7F, 0xFE)
				report[44] = 0x05
				return report
			}(),
		},
		{
			name:   "haptics select alone is ignored",
			report: makeUSBOutputReport(0x02, 0, 0, 0x91, 0x92),
		},
		{
			name:   "unflagged arbitrary motor bytes are ignored",
			report: makeUSBOutputReport(0, 0, 0, 0xA1, 0xA2),
		},
		{
			name:   "v2 compatible vibration updates motors",
			report: makeUSBOutputReport(0, 0, 0x04, 0x33, 0x44),
			state:  &OutputState{RumbleSmall: 0x33, RumbleLarge: 0x44},
		},
		{
			name:   "v1 explicit zero stops motors",
			report: makeUSBOutputReport(0x01, 0, 0, 0, 0),
			state:  &OutputState{},
		},
		{
			name:   "v2 explicit zero stops motors",
			report: makeUSBOutputReport(0, 0, 0x04, 0, 0),
			state:  &OutputState{},
		},
		{
			name: "combined rumble and LED update sends motors only",
			report: func() []byte {
				report := makeUSBOutputReport(0x01, 0x14, 0x04, 0x77, 0x88)
				report[44] = 0x05
				report[45], report[46], report[47] = 0x12, 0x34, 0x56
				return report
			}(),
			state: &OutputState{RumbleSmall: 0x77, RumbleLarge: 0x88},
		},
	}

	for _, path := range paths {
		t.Run(path.name, func(t *testing.T) {
			dev, err := New(nil)
			if err != nil {
				t.Fatal(err)
			}
			var callbacks []OutputState
			dev.SetOutputCallback(func(state OutputState) { callbacks = append(callbacks, state) })

			var wantCallbacks []OutputState
			for _, step := range steps {
				t.Run(step.name, func(t *testing.T) {
					response, handled := path.send(dev, step.report)
					if path.control && (!handled || response != nil) {
						t.Fatalf("HID SET_REPORT response = (%v, %v), want (nil, true)", response, handled)
					}
					if step.state != nil {
						wantCallbacks = append(wantCallbacks, *step.state)
					}
					if len(callbacks) != len(wantCallbacks) {
						t.Fatalf("callback count = %d, want %d", len(callbacks), len(wantCallbacks))
					}
					if len(wantCallbacks) > 0 && callbacks[len(wantCallbacks)-1] != wantCallbacks[len(wantCallbacks)-1] {
						t.Fatalf("callback state = %+v, want %+v", callbacks[len(wantCallbacks)-1], wantCallbacks[len(wantCallbacks)-1])
					}
				})
			}
		})
	}
}

func TestDualSenseEdgeRumbleOnlyCallbackAcrossReuse(t *testing.T) {
	paths := []struct {
		name string
		send func(*DualSense, []byte) ([]byte, bool)
	}{
		{
			name: "interrupt OUT",
			send: func(d *DualSense, report []byte) ([]byte, bool) {
				d.HandleTransfer(context.Background(), EndpointOut, usbip.DirOut, report)
				return nil, false
			},
		},
		{
			name: "HID SET_REPORT",
			send: func(d *DualSense, report []byte) ([]byte, bool) {
				return d.HandleControl(hidClassOUT, hidSetReport, 0x0202, 0, uint16(len(report)), report)
			},
		},
	}
	for _, path := range paths {
		t.Run(path.name, func(t *testing.T) {
			dev, err := NewEdge(nil)
			if err != nil {
				t.Fatal(err)
			}
			var previousSession []OutputState
			dev.SetOutputCallback(func(state OutputState) { previousSession = append(previousSession, state) })
			path.send(dev, makeUSBOutputReport(0x01, 0, 0, 0x61, 0x72))
			if len(previousSession) != 1 || previousSession[0] != (OutputState{RumbleSmall: 0x61, RumbleLarge: 0x72}) {
				t.Fatalf("previous-session callbacks = %+v, want one 61/72 rumble event", previousSession)
			}

			dev.SetOutputCallback(nil)
			var nextSession []OutputState
			dev.SetOutputCallback(func(state OutputState) { nextSession = append(nextSession, state) })
			_, handled := path.send(dev, makeUSBOutputReport(0, 0x04, 0, 0xAA, 0xBB))
			if path.name == "HID SET_REPORT" && !handled {
				t.Fatal("HID SET_REPORT did not acknowledge ignored LED-only output")
			}
			if len(nextSession) != 0 {
				t.Fatalf("LED-only callback count after rearm = %d, want 0", len(nextSession))
			}

			path.send(dev, makeUSBOutputReport(0, 0, 0x04, 0x12, 0x34))
			if len(nextSession) != 1 || nextSession[0] != (OutputState{RumbleSmall: 0x12, RumbleLarge: 0x34}) {
				t.Fatalf("callbacks after new valid rumble = %+v, want one 12/34 rumble event", nextSession)
			}
		})
	}
}

func TestDualSenseAndEdgeDescriptorStringsAreIsolated(t *testing.T) {
	normal, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	edge, err := NewEdge(nil)
	if err != nil {
		t.Fatal(err)
	}

	if got := normal.GetDescriptor().Strings[2]; got != "DualSense Wireless Controller" {
		t.Fatalf("normal product string after Edge creation = %q", got)
	}
	if got := normal.GetDescriptor().Device; got.IDVendor != 0x054C || got.IDProduct != 0x0CE6 {
		t.Fatalf("normal USB identity = %04x:%04x, want 054c:0ce6", got.IDVendor, got.IDProduct)
	}
	if got := edge.GetDescriptor().Device; got.IDVendor != 0x054C || got.IDProduct != 0x0DF2 {
		t.Fatalf("Edge USB identity = %04x:%04x, want 054c:0df2", got.IDVendor, got.IDProduct)
	}
	if got := edge.GetDescriptor().Strings[2]; got != "DualSense Edge Wireless Controller" {
		t.Fatalf("Edge product string = %q", got)
	}

	if _, err := New(nil); err != nil {
		t.Fatal(err)
	}
	if got := edge.GetDescriptor().Strings[2]; got != "DualSense Edge Wireless Controller" {
		t.Fatalf("Edge product string after later normal construction = %q", got)
	}
}

func TestDualSenseEdgeUSBInputReportContract(t *testing.T) {
	edge, err := NewEdge(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := edge.GetDescriptor().Device; got.IDVendor != 0x054C || got.IDProduct != 0x0DF2 {
		t.Fatalf("Edge USB identity = %04x:%04x, want 054c:0df2", got.IDVendor, got.IDProduct)
	}

	buttons := []struct {
		name  string
		value uint32
		want  byte
	}{
		{name: "left paddle", value: ButtonEdgeL4, want: 0x40},
		{name: "right paddle", value: ButtonEdgeR4, want: 0x80},
		{name: "both paddles and PS plus Cross", value: ButtonEdgeL4 | ButtonEdgeR4 | ButtonPS | ButtonCross, want: 0xC1},
	}
	for _, tc := range buttons {
		t.Run(tc.name, func(t *testing.T) {
			report := edgeInputReport(t, edge, &InputState{Buttons: tc.value})
			if len(report) != InputReportSize || report[0] != ReportIDInput {
				t.Fatalf("input report id/size = %02x/%d, want 01/%d", report[0], len(report), InputReportSize)
			}
			if report[10] != tc.want {
				t.Fatalf("button byte b[10] = %02x, want %02x", report[10], tc.want)
			}
			if tc.value&ButtonCross != 0 && report[8]&0xF0 != byte(ButtonCross) {
				t.Fatalf("Cross mapping b[8] = %02x, want high nibble %02x", report[8], byte(ButtonCross))
			}
		})
	}

	state := &InputState{
		LX: -128, LY: 127, RX: 0, RY: -1,
		DPad:  DPadUp | DPadLeft,
		GyroX: -1234, GyroY: 0x2345, GyroZ: -1,
		AccelX: 0x1234, AccelY: -2345, AccelZ: -8192,
	}
	report := edgeInputReport(t, edge, state)
	if report[1] != 0 || report[2] != 255 || report[3] != 128 || report[4] != 127 {
		t.Fatalf("stick encoding = %d/%d/%d/%d, want 0/255/128/127", report[1], report[2], report[3], report[4])
	}
	if report[8]&DPadMask != DPadUSBUpLeft {
		t.Fatalf("up-left D-Pad encoding = %d, want %d", report[8]&DPadMask, DPadUSBUpLeft)
	}
	for i, want := range []int16{state.GyroX, state.GyroY, state.GyroZ, state.AccelX, state.AccelY, state.AccelZ} {
		got := int16(binary.LittleEndian.Uint16(report[16+i*2 : 18+i*2]))
		if got != want {
			t.Fatalf("motion field %d = %d, want %d", i, got, want)
		}
	}

	neutral := edgeInputReport(t, edge, NewInputState())
	if neutral[1] != 128 || neutral[2] != 128 || neutral[3] != 128 || neutral[4] != 128 || neutral[8]&DPadMask != DPadUSBNeutral {
		t.Fatalf("neutral sticks/D-pad = %d/%d/%d/%d/%d, want 128/128/128/128/%d", neutral[1], neutral[2], neutral[3], neutral[4], neutral[8]&DPadMask, DPadUSBNeutral)
	}
}

func edgeInputReport(t *testing.T, edge *DualSense, state *InputState) []byte {
	t.Helper()
	edge.UpdateInputState(state)
	report := edge.HandleTransfer(context.Background(), 4, usbip.DirIn, nil)
	if report == nil {
		t.Fatal("DualSense Edge IN handler returned no report")
	}
	return report
}
