package dualsense

import (
	"context"
	"encoding/binary"
	"reflect"
	"sync"
	"testing"
	"time"

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

type outputReportPath struct {
	name    string
	control bool
	send    func(*DualSense, []byte) ([]byte, bool)
}

func outputReportPaths() []outputReportPath {
	return []outputReportPath{
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
}

type outputReportDeviceModel struct {
	name string
	new  func(*testing.T) *DualSense
}

func outputReportDeviceModels() []outputReportDeviceModel {
	return []outputReportDeviceModel{
		{
			name: "DualSense",
			new: func(t *testing.T) *DualSense {
				t.Helper()
				dev, err := New(nil)
				if err != nil {
					t.Fatal(err)
				}
				return dev
			},
		},
		{
			name: "DualSense Edge",
			new: func(t *testing.T) *DualSense {
				t.Helper()
				dev, err := NewEdge(nil)
				if err != nil {
					t.Fatal(err)
				}
				return dev
			},
		},
	}
}

func sendOutputReport(t *testing.T, path outputReportPath, dev *DualSense, report []byte) {
	t.Helper()
	response, handled := path.send(dev, report)
	if path.control && (!handled || response != nil) {
		t.Fatalf("HID SET_REPORT response = (%v, %v), want (nil, true)", response, handled)
	}
}

func sendMalformedOutputReport(t *testing.T, path outputReportPath, dev *DualSense, report []byte) {
	t.Helper()
	response, handled := path.send(dev, report)
	if path.control && (handled || response != nil) {
		t.Fatalf("short HID SET_REPORT response = (%v, %v), want (nil, false)", response, handled)
	}
}

func assertOutputStates(t *testing.T, got, want []OutputState) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("output callbacks = %+v, want %+v", got, want)
	}
}

func TestNeutralRumbleTerminationCompatibility(t *testing.T) {
	for _, model := range outputReportDeviceModels() {
		t.Run(model.name, func(t *testing.T) {
			for _, path := range outputReportPaths() {
				t.Run(path.name, func(t *testing.T) {
					for _, version := range []struct {
						name  string
						flag0 byte
						flag2 byte
					}{
						{name: "v1", flag0: outputFlag0CompatibleRumble},
						{name: "v2", flag2: outputFlag2CompatibleRumble},
					} {
						t.Run(version.name, func(t *testing.T) {
							dev := model.new(t)
							var got []OutputState
							dev.SetOutputCallback(func(state OutputState) { got = append(got, state) })

							start := makeUSBOutputReport(version.flag0, 0, version.flag2, 0x12, 0x34)
							neutral := makeUSBOutputReport(0, 0, 0, 0, 0)
							sendOutputReport(t, path, dev, neutral)
							assertOutputStates(t, got, nil)

							sendOutputReport(t, path, dev, start)
							assertOutputStates(t, got, []OutputState{{RumbleSmall: 0x12, RumbleLarge: 0x34}})

							sendOutputReport(t, path, dev, neutral)
							assertOutputStates(t, got, []OutputState{{RumbleSmall: 0x12, RumbleLarge: 0x34}, {}})

							sendOutputReport(t, path, dev, neutral)
							assertOutputStates(t, got, []OutputState{{RumbleSmall: 0x12, RumbleLarge: 0x34}, {}})

							start = makeUSBOutputReport(version.flag0, 0, version.flag2, 0x56, 0x78)
							sendOutputReport(t, path, dev, start)
							assertOutputStates(t, got, []OutputState{{RumbleSmall: 0x12, RumbleLarge: 0x34}, {}, {RumbleSmall: 0x56, RumbleLarge: 0x78}})
						})
					}
				})
			}
		})
	}
}

func TestConcurrentOutputReportsPreserveCallbackOrder(t *testing.T) {
	paths := outputReportPaths()
	start := makeUSBOutputReport(outputFlag0CompatibleRumble, 0, 0, 0x12, 0x34)
	neutral := makeUSBOutputReport(0, 0, 0, 0, 0)

	for _, model := range outputReportDeviceModels() {
		t.Run(model.name, func(t *testing.T) {
			for _, ingress := range []struct {
				name        string
				start, stop outputReportPath
			}{
				{name: "interrupt OUT then HID SET_REPORT", start: paths[0], stop: paths[1]},
				{name: "HID SET_REPORT then interrupt OUT", start: paths[1], stop: paths[0]},
			} {
				t.Run(ingress.name, func(t *testing.T) {
					dev := model.new(t)
					startEntered := make(chan struct{})
					releaseStart := make(chan struct{})
					var releaseOnce sync.Once
					var got []OutputState
					var gotMu sync.Mutex
					dev.SetOutputCallback(func(state OutputState) {
						if state.RumbleSmall != 0 || state.RumbleLarge != 0 {
							close(startEntered)
							<-releaseStart
						}
						gotMu.Lock()
						got = append(got, state)
						gotMu.Unlock()
					})

					startDone := make(chan struct{})
					go func() {
						ingress.start.send(dev, start)
						close(startDone)
					}()
					<-startEntered

					// Keep START's callback in flight while the opposing ingress handles STOP.
					// This timer only releases the test gate; it is not a timing assertion.
					timer := time.AfterFunc(100*time.Millisecond, func() {
						releaseOnce.Do(func() { close(releaseStart) })
					})
					ingress.stop.send(dev, neutral)
					timer.Stop()
					releaseOnce.Do(func() { close(releaseStart) })
					<-startDone

					gotMu.Lock()
					defer gotMu.Unlock()
					assertOutputStates(t, got, []OutputState{{RumbleSmall: 0x12, RumbleLarge: 0x34}, {}})
				})
			}
		})
	}
}

func TestOutputCallbackReplacementDuringDispatch(t *testing.T) {
	paths := outputReportPaths()
	start := makeUSBOutputReport(outputFlag0CompatibleRumble, 0, 0, 0x12, 0x34)
	neutral := makeUSBOutputReport(0, 0, 0, 0, 0)

	for _, model := range outputReportDeviceModels() {
		t.Run(model.name, func(t *testing.T) {
			dev := model.new(t)
			oldEntered := make(chan struct{})
			releaseOld := make(chan struct{})
			var oldOnce sync.Once
			var oldCalls, newCalls []OutputState
			dev.SetOutputCallback(func(state OutputState) {
				oldOnce.Do(func() {
					close(oldEntered)
					<-releaseOld
				})
				oldCalls = append(oldCalls, state)
			})

			startDone := make(chan struct{})
			go func() {
				paths[0].send(dev, start)
				close(startDone)
			}()
			<-oldEntered

			// Clearing/replacing the registration does not wait for the already
			// captured callback; it resets the rumble session for later reports.
			dev.SetOutputCallback(nil)
			dev.SetOutputCallback(func(state OutputState) { newCalls = append(newCalls, state) })
			close(releaseOld)
			<-startDone
			paths[1].send(dev, neutral)
			assertOutputStates(t, oldCalls, []OutputState{{RumbleSmall: 0x12, RumbleLarge: 0x34}})
			assertOutputStates(t, newCalls, nil)

			paths[1].send(dev, start)
			paths[0].send(dev, neutral)
			assertOutputStates(t, newCalls, []OutputState{{RumbleSmall: 0x12, RumbleLarge: 0x34}, {}})
		})
	}
}

func TestFeatureOnlyReportsDoNotStopActiveRumble(t *testing.T) {
	for _, model := range outputReportDeviceModels() {
		t.Run(model.name, func(t *testing.T) {
			for _, path := range outputReportPaths() {
				t.Run(path.name, func(t *testing.T) {
					dev := model.new(t)
					var got []OutputState
					dev.SetOutputCallback(func(state OutputState) { got = append(got, state) })

					start := makeUSBOutputReport(outputFlag0CompatibleRumble, 0, 0, 0x12, 0x34)
					sendOutputReport(t, path, dev, start)
					want := []OutputState{{RumbleSmall: 0x12, RumbleLarge: 0x34}}
					assertOutputStates(t, got, want)

					featureReports := [][]byte{
						makeUSBOutputReport(0, 0x04, 0, 0, 0),
						makeUSBOutputReport(0, 0x10, 0, 0x7F, 0xFE),
						makeUSBOutputReport(0x04, 0, 0, 0, 0),
						makeUSBOutputReport(0x08, 0, 0, 0, 0),
						makeUSBOutputReport(outputFlag0HapticsSelect, 0, 0, 0x91, 0x92),
						makeUSBOutputReport(0, 0, 0, 0xA1, 0xA2),
						makeUSBOutputReport(0, 0x04, 0, 0xB1, 0xB2),
					}
					for i, report := range featureReports {
						sendOutputReport(t, path, dev, report)
						assertOutputStates(t, got, want)
						if len(got) != 1 {
							t.Fatalf("feature-only report %d emitted a callback", i)
						}
					}

					neutral := makeUSBOutputReport(0, 0, 0, 0, 0)
					sendOutputReport(t, path, dev, neutral)
					want = append(want, OutputState{})
					sendOutputReport(t, path, dev, neutral)
					assertOutputStates(t, got, want)
				})
			}
		})
	}
}

func TestRumbleStateIsScopedToCallbackSessionAndDevice(t *testing.T) {
	for _, model := range outputReportDeviceModels() {
		t.Run(model.name, func(t *testing.T) {
			for _, path := range outputReportPaths() {
				t.Run(path.name, func(t *testing.T) {
					first := model.new(t)
					second := model.new(t)
					var firstSession, replacementSession, secondDevice []OutputState
					first.SetOutputCallback(func(state OutputState) { firstSession = append(firstSession, state) })
					second.SetOutputCallback(func(state OutputState) { secondDevice = append(secondDevice, state) })

					start := makeUSBOutputReport(outputFlag0CompatibleRumble, 0, 0, 0x12, 0x34)
					neutral := makeUSBOutputReport(0, 0, 0, 0, 0)
					sendOutputReport(t, path, first, start)
					assertOutputStates(t, firstSession, []OutputState{{RumbleSmall: 0x12, RumbleLarge: 0x34}})

					// An active device must not make another device emit a compatibility STOP.
					sendOutputReport(t, path, second, neutral)
					assertOutputStates(t, secondDevice, nil)

					// Replacing the callback starts a fresh rumble-state session.
					first.SetOutputCallback(func(state OutputState) { replacementSession = append(replacementSession, state) })
					sendOutputReport(t, path, first, neutral)
					assertOutputStates(t, replacementSession, nil)

					sendOutputReport(t, path, first, start)
					sendOutputReport(t, path, first, neutral)
					assertOutputStates(t, replacementSession, []OutputState{{RumbleSmall: 0x12, RumbleLarge: 0x34}, {}})

					first.SetOutputCallback(nil)
					var rearmedSession []OutputState
					first.SetOutputCallback(func(state OutputState) { rearmedSession = append(rearmedSession, state) })
					sendOutputReport(t, path, first, neutral)
					assertOutputStates(t, rearmedSession, nil)
					sendOutputReport(t, path, first, start)
					sendOutputReport(t, path, first, neutral)
					assertOutputStates(t, rearmedSession, []OutputState{{RumbleSmall: 0x12, RumbleLarge: 0x34}, {}})
				})
			}
		})
	}
}

func TestMalformedAndWrongIngressReportsDoNotChangeRumbleState(t *testing.T) {
	for _, model := range outputReportDeviceModels() {
		t.Run(model.name, func(t *testing.T) {
			for _, path := range outputReportPaths() {
				t.Run(path.name, func(t *testing.T) {
					dev := model.new(t)
					var got []OutputState
					dev.SetOutputCallback(func(state OutputState) { got = append(got, state) })

					start := makeUSBOutputReport(outputFlag0CompatibleRumble, 0, 0, 0x12, 0x34)
					neutral := makeUSBOutputReport(0, 0, 0, 0, 0)
					wrongEndpointStart := makeUSBOutputReport(outputFlag0CompatibleRumble, 0, 0, 0x56, 0x78)
					dev.HandleTransfer(context.Background(), EndpointOut+1, usbip.DirOut, wrongEndpointStart)
					dev.HandleTransfer(context.Background(), EndpointOut, usbip.DirIn, wrongEndpointStart)
					sendOutputReport(t, path, dev, neutral)
					assertOutputStates(t, got, nil)

					short := append([]byte(nil), start[:47]...)
					wrongID := append([]byte(nil), start...)
					wrongID[0]++
					sendMalformedOutputReport(t, path, dev, short)
					sendOutputReport(t, path, dev, wrongID)
					assertOutputStates(t, got, nil)
					sendOutputReport(t, path, dev, neutral)
					assertOutputStates(t, got, nil)

					// The established parser accepts full 64-byte reports.
					full := make([]byte, 64)
					copy(full, start)
					sendOutputReport(t, path, dev, full)
					want := []OutputState{{RumbleSmall: 0x12, RumbleLarge: 0x34}}
					assertOutputStates(t, got, want)

					malformedNeutral := append([]byte(nil), neutral[:47]...)
					wrongNeutralID := append([]byte(nil), neutral...)
					wrongNeutralID[0]++
					sendMalformedOutputReport(t, path, dev, malformedNeutral)
					sendOutputReport(t, path, dev, wrongNeutralID)
					dev.HandleTransfer(context.Background(), EndpointOut+1, usbip.DirOut, neutral)
					dev.HandleTransfer(context.Background(), EndpointOut, usbip.DirIn, neutral)
					assertOutputStates(t, got, want)

					sendOutputReport(t, path, dev, neutral)
					want = append(want, OutputState{})
					assertOutputStates(t, got, want)
				})
			}
		})
	}
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
			name:   "v2 compatible vibration updates motors again",
			report: makeUSBOutputReport(0, 0, 0x04, 0x55, 0x66),
			state:  &OutputState{RumbleSmall: 0x55, RumbleLarge: 0x66},
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
