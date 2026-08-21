package steamdeck_test

import (
	"context"
	"encoding/binary"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	viiperTesting "github.com/Alia5/VIIPER/_testing"
	"github.com/Alia5/VIIPER/device"
	"github.com/Alia5/VIIPER/device/steamdeck"
	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/internal/server/api/handler"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/Alia5/VIIPER/viiperclient"
	"github.com/Alia5/VIIPER/virtualbus"
	"github.com/stretchr/testify/assert"

	_ "github.com/Alia5/VIIPER/internal/registry"
)

const defaultControllerEndpoint = 3

func TestInputReports(t *testing.T) {
	tests := []struct {
		name     string
		state    steamdeck.InputState
		validate func(t *testing.T, got []byte)
	}{
		{
			name:  "neutral defaults",
			state: steamdeck.InputState{},
			validate: func(t *testing.T, got []byte) {
				assert.Len(t, got, steamdeck.InputReportLen)
				assert.Equal(t, byte(0x01), got[0])
				assert.Equal(t, byte(0x00), got[1])
				assert.Equal(t, byte(steamdeck.InputReportID), got[2])
				assert.Equal(t, byte(steamdeck.InputReportLen), got[3])
				assert.Equal(t, make([]byte, 8), got[8:16])
				// Pads, sticks, triggers, and force fields must all be zero on a
				// neutral report; the quaternion-zero behavior is intentional
				// (see TestMotionFieldWireOrderMatchesSteamConsumers) and unchanged here.
				assert.Equal(t, make([]byte, 8), got[16:24])
				assert.Equal(t, uint16(0), binary.LittleEndian.Uint16(got[36:38]))
				assert.Equal(t, uint16(0), binary.LittleEndian.Uint16(got[38:40]))
				assert.Equal(t, uint16(0), binary.LittleEndian.Uint16(got[40:42]))
				assert.Equal(t, uint16(0), binary.LittleEndian.Uint16(got[42:44]))
				assert.Equal(t, make([]byte, 20), got[44:64])
			},
		},
		{
			name: "buttons and axes",
			state: steamdeck.InputState{
				A: true, Y: true, L1: true, R2Digital: true,
				Menu: true, Steam: true, DPadLeft: true,
				L3: true, RPadTouch: true, LPadPress: true,
				R3: true, RStickTouch: true, L4: true, QuickAccess: true,
				LPadX: -1000, LPadY: 2000,
				LTrigger: 1234, RTrigger: 4321,
				LStickX: -2222, LStickY: 3333,
			},
			validate: func(t *testing.T, got []byte) {
				assert.Equal(t, byte(0x99), got[8])
				assert.Equal(t, byte(0x64), got[9])
				assert.Equal(t, byte(0x52), got[10])
				assert.Equal(t, byte(0x04), got[11])
				assert.Equal(t, byte(0x82), got[13])
				assert.Equal(t, byte(0x04), got[14])
				assert.Equal(t, uint16(0xfc18), binary.LittleEndian.Uint16(got[16:18]))
				assert.Equal(t, uint16(2000), binary.LittleEndian.Uint16(got[18:20]))
				assert.Equal(t, uint16(1234), binary.LittleEndian.Uint16(got[44:46]))
				assert.Equal(t, uint16(4321), binary.LittleEndian.Uint16(got[46:48]))
				assert.Equal(t, uint16(0xf752), binary.LittleEndian.Uint16(got[48:50]))
				assert.Equal(t, uint16(3333), binary.LittleEndian.Uint16(got[50:52]))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dev, err := steamdeck.New(nil)
			if !assert.NoError(t, err) {
				return
			}
			dev.UpdateInputState(&tt.state)
			got := dev.HandleTransfer(context.Background(), defaultControllerEndpoint, usbip.DirIn, nil)
			tt.validate(t, got)
		})
	}
}
func TestMotionFieldWireOrderMatchesSteamConsumers(t *testing.T) {
	dev, err := steamdeck.New(nil)
	if !assert.NoError(t, err) {
		return
	}

	dev.UpdateInputState(&steamdeck.InputState{
		AccelX:    0x0111,
		AccelY:    0x0222,
		AccelZ:    0x0333,
		Pitch:     0x0444,
		Yaw:       0x0555,
		Roll:      0x0666,
		GyroQuatW: 0x0777,
		GyroQuatX: 0x0888,
		GyroQuatY: 0x0999,
		GyroQuatZ: 0x0aaa,
	})

	got := dev.HandleTransfer(context.Background(), defaultControllerEndpoint, usbip.DirIn, nil)

	// Linux hid-steam and SDL both decode the Steam Deck IMU report as X, Z, -Y.
	// Keep the raw field order stable so higher-level HC translations can target it precisely.
	assert.Equal(t, uint16(0x0111), binary.LittleEndian.Uint16(got[24:26]))
	assert.Equal(t, uint16(0x0222), binary.LittleEndian.Uint16(got[26:28]))
	assert.Equal(t, uint16(0x0333), binary.LittleEndian.Uint16(got[28:30]))
	assert.Equal(t, uint16(0x0444), binary.LittleEndian.Uint16(got[30:32]))
	assert.Equal(t, uint16(0x0555), binary.LittleEndian.Uint16(got[32:34]))
	assert.Equal(t, uint16(0x0666), binary.LittleEndian.Uint16(got[34:36]))
	assert.Equal(t, uint16(0x0777), binary.LittleEndian.Uint16(got[36:38]))
	assert.Equal(t, uint16(0x0888), binary.LittleEndian.Uint16(got[38:40]))
	assert.Equal(t, uint16(0x0999), binary.LittleEndian.Uint16(got[40:42]))
	assert.Equal(t, uint16(0x0aaa), binary.LittleEndian.Uint16(got[42:44]))
}

func steamDeckInputReport(t *testing.T, state *steamdeck.InputState) []byte {
	t.Helper()
	dev, err := steamdeck.New(nil)
	if !assert.NoError(t, err) {
		return nil
	}
	dev.UpdateInputState(state)
	return dev.HandleTransfer(context.Background(), defaultControllerEndpoint, usbip.DirIn, nil)
}

func TestSteamDeckReportHeaderInvariants(t *testing.T) {
	got := steamDeckInputReport(t, &steamdeck.InputState{})
	if !assert.NotNil(t, got) {
		return
	}
	assert.Equal(t, byte(0x01), got[0])
	assert.Equal(t, byte(0x00), got[1])
	assert.Equal(t, byte(steamdeck.InputReportID), got[2])
	// Byte 3 declares the full 64-byte input report length. See the
	// InputReportLen comment in const.go for why this is 64 and not the
	// 56-byte span Linux hid-steam currently decodes.
	assert.Equal(t, byte(steamdeck.InputReportLen), got[3])
}

// TestSteamDeckButtonWireSemantics freezes each individual button/digital
// control at its current protocol-defined byte and bit, one control at a
// time, so an accidental swap between two neighboring bits (or a bit leaking
// into an unrelated byte) fails clearly instead of hiding inside a mixed
// bitmask assertion. Basic non-gyro Steam Deck input has been hardware
// validated on MSI Claw EX; that validation covers the overall input path
// rather than certifying every individual bit in this table, so this
// comment does not claim per-control hardware validation.
//
// The button region currently spans bytes 8-14 (byte 12 is unused/reserved);
// every case asserts both the exact expected byte and that all other bytes
// in that region remain zero.
func TestSteamDeckButtonWireSemantics(t *testing.T) {
	buttonRegion := []int{8, 9, 10, 11, 12, 13, 14}

	cases := []struct {
		name   string
		set    func(*steamdeck.InputState)
		offset int
		mask   byte
	}{
		// Byte 8
		{"A", func(s *steamdeck.InputState) { s.A = true }, 8, 0x80},
		{"X", func(s *steamdeck.InputState) { s.X = true }, 8, 0x40},
		{"B", func(s *steamdeck.InputState) { s.B = true }, 8, 0x20},
		{"Y", func(s *steamdeck.InputState) { s.Y = true }, 8, 0x10},
		{"L1", func(s *steamdeck.InputState) { s.L1 = true }, 8, 0x08},
		{"R1", func(s *steamdeck.InputState) { s.R1 = true }, 8, 0x04},
		{"L2Digital", func(s *steamdeck.InputState) { s.L2Digital = true }, 8, 0x02},
		{"R2Digital", func(s *steamdeck.InputState) { s.R2Digital = true }, 8, 0x01},

		// Byte 9 -- L5/Menu/Steam/Options, plus D-pad tested individually
		// (not as a combined mask) per each direction below.
		{"L5", func(s *steamdeck.InputState) { s.L5 = true }, 9, 0x80},
		// Menu (0x40) is the Steam Deck MENU/Start semantic; Options (0x10) is
		// the VIEW/Back semantic. See TestSteamDeckMenuAndViewWireSemantics
		// for the dedicated regression on this pair.
		{"Menu", func(s *steamdeck.InputState) { s.Menu = true }, 9, 0x40},
		{"Steam", func(s *steamdeck.InputState) { s.Steam = true }, 9, 0x20},
		{"Options", func(s *steamdeck.InputState) { s.Options = true }, 9, 0x10},
		{"DPadDown", func(s *steamdeck.InputState) { s.DPadDown = true }, 9, 0x08},
		{"DPadLeft", func(s *steamdeck.InputState) { s.DPadLeft = true }, 9, 0x04},
		{"DPadRight", func(s *steamdeck.InputState) { s.DPadRight = true }, 9, 0x02},
		{"DPadUp", func(s *steamdeck.InputState) { s.DPadUp = true }, 9, 0x01},

		// Byte 10
		{"L3", func(s *steamdeck.InputState) { s.L3 = true }, 10, 0x40},
		{"RPadTouch", func(s *steamdeck.InputState) { s.RPadTouch = true }, 10, 0x10},
		{"LPadTouch", func(s *steamdeck.InputState) { s.LPadTouch = true }, 10, 0x08},
		{"RPadPress", func(s *steamdeck.InputState) { s.RPadPress = true }, 10, 0x04},
		{"LPadPress", func(s *steamdeck.InputState) { s.LPadPress = true }, 10, 0x02},
		{"R5", func(s *steamdeck.InputState) { s.R5 = true }, 10, 0x01},

		// Byte 11
		{"R3", func(s *steamdeck.InputState) { s.R3 = true }, 11, 0x04},

		// Byte 13
		{"RStickTouch", func(s *steamdeck.InputState) { s.RStickTouch = true }, 13, 0x80},
		{"LStickTouch", func(s *steamdeck.InputState) { s.LStickTouch = true }, 13, 0x40},
		{"R4", func(s *steamdeck.InputState) { s.R4 = true }, 13, 0x04},
		{"L4", func(s *steamdeck.InputState) { s.L4 = true }, 13, 0x02},

		// Byte 14
		{"QuickAccess", func(s *steamdeck.InputState) { s.QuickAccess = true }, 14, 0x04},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := steamdeck.InputState{}
			tc.set(&state)
			got := steamDeckInputReport(t, &state)
			if !assert.NotNil(t, got) {
				return
			}
			assert.Equal(t, tc.mask, got[tc.offset], "expected %s to set byte %d to 0x%02x", tc.name, tc.offset, tc.mask)
			for _, b := range buttonRegion {
				if b == tc.offset {
					continue
				}
				assert.Equalf(t, byte(0), got[b], "%s unexpectedly set byte %d", tc.name, b)
			}
		})
	}
}

// TestSteamDeckMenuAndViewWireSemantics is a focused regression proving the
// currently correct VIIPER Steam Deck wire semantics for the two center
// buttons: Menu is the MENU/Start button (byte 9, 0x40) and Options is the
// VIEW/Back button (byte 9, 0x10). VIIPER's field names are unchanged by this
// test; a consumer that maps Start/Back must map onto these fields correctly
// rather than the other way around.
func TestSteamDeckMenuAndViewWireSemantics(t *testing.T) {
	menu := steamDeckInputReport(t, &steamdeck.InputState{Menu: true})
	if !assert.NotNil(t, menu) {
		return
	}
	assert.Equal(t, byte(0x40), menu[9], "Menu (MENU/Start semantic) must set byte 9 bit 0x40")

	options := steamDeckInputReport(t, &steamdeck.InputState{Options: true})
	if !assert.NotNil(t, options) {
		return
	}
	assert.Equal(t, byte(0x10), options[9], "Options (VIEW/Back semantic) must set byte 9 bit 0x10")
}

// TestSteamDeckAnalogAndCoordinateWireOffsets verifies the little-endian byte
// offsets for analog triggers, sticks, and trackpad coordinates using
// distinct, non-aliasing signed/unsigned values chosen so that an X/Y or
// left/right swap would fail obviously.
func TestSteamDeckAnalogAndCoordinateWireOffsets(t *testing.T) {
	state := steamdeck.InputState{
		LTrigger: 1000,
		RTrigger: 54321,
		LStickX:  -12345,
		LStickY:  6789,
		RStickX:  -6789,
		RStickY:  12345,
		LPadX:    -100,
		LPadY:    200,
		RPadX:    -300,
		RPadY:    400,
	}
	got := steamDeckInputReport(t, &state)
	if !assert.NotNil(t, got) {
		return
	}

	assert.Equal(t, uint16(1000), binary.LittleEndian.Uint16(got[44:46]), "LTrigger offset")
	assert.Equal(t, uint16(54321), binary.LittleEndian.Uint16(got[46:48]), "RTrigger offset")

	// Analog trigger travel must not implicitly set the independent digital
	// trigger click flags (byte 8, 0x02/0x01).
	assert.Zero(t, got[8]&0x02, "LTrigger must not set L2Digital")
	assert.Zero(t, got[8]&0x01, "RTrigger must not set R2Digital")

	assert.Equal(t, int16(-12345), int16(binary.LittleEndian.Uint16(got[48:50])), "LStickX offset")
	assert.Equal(t, int16(6789), int16(binary.LittleEndian.Uint16(got[50:52])), "LStickY offset")
	assert.Equal(t, int16(-6789), int16(binary.LittleEndian.Uint16(got[52:54])), "RStickX offset")
	assert.Equal(t, int16(12345), int16(binary.LittleEndian.Uint16(got[54:56])), "RStickY offset")

	assert.Equal(t, int16(-100), int16(binary.LittleEndian.Uint16(got[16:18])), "LPadX offset")
	assert.Equal(t, int16(200), int16(binary.LittleEndian.Uint16(got[18:20])), "LPadY offset")
	assert.Equal(t, int16(-300), int16(binary.LittleEndian.Uint16(got[20:22])), "RPadX offset")
	assert.Equal(t, int16(400), int16(binary.LittleEndian.Uint16(got[22:24])), "RPadY offset")
}

// TestSteamDeckForceWireOffsets verifies the distinct little-endian offsets
// for the pad/stick force-sense fields, kept separate per field so a swap
// among them cannot hide behind a generic "tail bytes changed" assertion.
// Values are chosen so no field aliases another, which would hide an offset
// mixup.
//
// LStickForce/RStickForce occupy report bytes 60:64. This is the same tail
// InputPlumber's physical Steam Deck report model decodes as
// l_stick_force/r_stick_force (documented there as thumbstick
// capacitive-sensor data) and that Handheld Companion's virtual Steam Deck
// target also builds. Linux hid-steam's "56 bytes" description and SDL's
// current SteamDeckStatePacket_t both stop at RPadForce, but that reflects
// what those two consumers currently decode/expose, not proof that the
// wire's final four bytes are unused -- see the InputReportLen comment in
// const.go and the tail comment in buildReport for the full evidence trail.
func TestSteamDeckForceWireOffsets(t *testing.T) {
	state := steamdeck.InputState{
		LPadForce:   0x1122,
		RPadForce:   0x3344,
		LStickForce: 0x5566,
		RStickForce: 0x7788,
	}
	got := steamDeckInputReport(t, &state)
	if !assert.NotNil(t, got) {
		return
	}

	assert.Equal(t, uint16(0x1122), binary.LittleEndian.Uint16(got[56:58]), "LPadForce offset")
	assert.Equal(t, uint16(0x3344), binary.LittleEndian.Uint16(got[58:60]), "RPadForce offset")
	assert.Equal(t, uint16(0x5566), binary.LittleEndian.Uint16(got[60:62]), "LStickForce offset")
	assert.Equal(t, uint16(0x7788), binary.LittleEndian.Uint16(got[62:64]), "RStickForce offset")
}

// TestSteamDeckFullReportContract locks in the full 64-byte Steam Deck input
// report header: transport length, report ID, and the byte-3 declared
// length, which must be 64 (not the 56-byte span Linux hid-steam currently
// decodes -- see the InputReportLen comment in const.go).
func TestSteamDeckFullReportContract(t *testing.T) {
	got := steamDeckInputReport(t, &steamdeck.InputState{})
	if !assert.NotNil(t, got) {
		return
	}
	assert.Len(t, got, steamdeck.InputReportLen, "transport report is a 64-byte buffer")
	assert.Equal(t, byte(0x01), got[0])
	assert.Equal(t, byte(0x00), got[1])
	assert.Equal(t, byte(steamdeck.InputReportID), got[2])
	assert.Equal(t, byte(64), got[3], "byte 3 declares the full 64-byte report length")
}

// TestSteamDeckStickForceIndependentFromStickClick proves LStickForce/
// RStickForce (report bytes 60:64) are decoded completely independently
// from the L3/R3 digital stick-click bits (byte 10 bit 0x40 and byte 11 bit
// 0x04 respectively). VIIPER must expose generic protocol state here and
// must not bake a click-derived force policy into the device layer -- that
// kind of synthesis (as Handheld Companion's consumer-side mapper currently
// does) belongs in a downstream consumer, not in VIIPER.
func TestSteamDeckStickForceIndependentFromStickClick(t *testing.T) {
	clicked := steamDeckInputReport(t, &steamdeck.InputState{
		L3: true, R3: true,
		LStickForce: 0, RStickForce: 0,
	})
	if !assert.NotNil(t, clicked) {
		return
	}
	assert.Equal(t, byte(0x40), clicked[10], "L3 must still set its own digital bit")
	assert.Equal(t, byte(0x04), clicked[11], "R3 must still set its own digital bit")
	assert.Equal(t, make([]byte, 4), clicked[60:64], "stick-force tail stays zero when clicks are set but force is not")

	forced := steamDeckInputReport(t, &steamdeck.InputState{
		L3: false, R3: false,
		LStickForce: 0x1234, RStickForce: 0x5678,
	})
	if !assert.NotNil(t, forced) {
		return
	}
	assert.Zero(t, forced[10]&0x40, "L3 digital bit must stay clear when only force is set")
	assert.Zero(t, forced[11]&0x04, "R3 digital bit must stay clear when only force is set")
	assert.Equal(t, uint16(0x1234), binary.LittleEndian.Uint16(forced[60:62]), "LStickForce must be set independent of L3")
	assert.Equal(t, uint16(0x5678), binary.LittleEndian.Uint16(forced[62:64]), "RStickForce must be set independent of R3")
}

// TestSteamDeckReserved15Passthrough freezes the current direct passthrough
// of InputState.Reserved15 to byte 15. No semantics are asserted for this
// byte beyond its existing wire position.
func TestSteamDeckReserved15Passthrough(t *testing.T) {
	got := steamDeckInputReport(t, &steamdeck.InputState{Reserved15: 0xAB})
	if !assert.NotNil(t, got) {
		return
	}
	assert.Equal(t, byte(0xAB), got[15])
}

// TestSteamDeckNonMotionReportRoundTrips proves a fully populated non-motion
// InputState survives a MarshalBinary/UnmarshalBinary round trip unchanged.
// Motion fields are covered separately by
// TestMotionFieldWireOrderMatchesSteamConsumers and are intentionally not
// exercised here.
func TestSteamDeckNonMotionReportRoundTrips(t *testing.T) {
	original := steamdeck.InputState{
		A: true, DPadUp: true, Menu: true, Options: true, R4: true, QuickAccess: true,
		Reserved15: 0x2A,
		LPadX:      -100, LPadY: 200, RPadX: -300, RPadY: 400,
		LTrigger: 1000, RTrigger: 54321,
		LStickX: -12345, LStickY: 6789, RStickX: -6789, RStickY: 12345,
		LPadForce: 1111, RPadForce: 2222,
		LStickForce: 3333, RStickForce: 4444,
	}

	data, err := original.MarshalBinary()
	if !assert.NoError(t, err) {
		return
	}

	var decoded steamdeck.InputState
	if !assert.NoError(t, decoded.UnmarshalBinary(data)) {
		return
	}

	assert.Equal(t, original.A, decoded.A)
	assert.Equal(t, original.DPadUp, decoded.DPadUp)
	assert.Equal(t, original.Menu, decoded.Menu)
	assert.Equal(t, original.Options, decoded.Options)
	assert.Equal(t, original.R4, decoded.R4)
	assert.Equal(t, original.QuickAccess, decoded.QuickAccess)
	assert.Equal(t, original.Reserved15, decoded.Reserved15)
	assert.Equal(t, original.LPadX, decoded.LPadX)
	assert.Equal(t, original.LPadY, decoded.LPadY)
	assert.Equal(t, original.RPadX, decoded.RPadX)
	assert.Equal(t, original.RPadY, decoded.RPadY)
	assert.Equal(t, original.LTrigger, decoded.LTrigger)
	assert.Equal(t, original.RTrigger, decoded.RTrigger)
	assert.Equal(t, original.LStickX, decoded.LStickX)
	assert.Equal(t, original.LStickY, decoded.LStickY)
	assert.Equal(t, original.RStickX, decoded.RStickX)
	assert.Equal(t, original.RStickY, decoded.RStickY)
	assert.Equal(t, original.LPadForce, decoded.LPadForce)
	assert.Equal(t, original.RPadForce, decoded.RPadForce)
	assert.Equal(t, original.LStickForce, decoded.LStickForce)
	assert.Equal(t, original.RStickForce, decoded.RStickForce)
}

func TestFeedbackCommands(t *testing.T) {
	dev, err := steamdeck.New(nil)
	if !assert.NoError(t, err) {
		return
	}

	var got steamdeck.OutputState
	dev.SetOutputCallback(func(out steamdeck.OutputState) {
		got = out
	})

	cmd := make([]byte, steamdeck.InputReportLen)
	cmd[0] = steamdeck.FeatureTriggerRumbleCommand
	cmd[1] = 9
	cmd[2] = 7
	binary.LittleEndian.PutUint16(cmd[3:5], 0x0321)
	binary.LittleEndian.PutUint16(cmd[5:7], 500)
	binary.LittleEndian.PutUint16(cmd[7:9], 900)
	leftGain, rightGain := int8(-40), int8(60)
	cmd[9] = byte(leftGain)
	cmd[10] = byte(rightGain)

	_, handled := dev.HandleControl(0x21, 0x09, uint16(0x0300), 0, uint16(len(cmd)), cmd)
	if !assert.True(t, handled) {
		return
	}
	rumble, ok := got.AsRumble()
	if !assert.True(t, ok) {
		return
	}
	assert.Equal(t, uint8(7), rumble.RumbleType)
	assert.Equal(t, uint16(0x0321), rumble.Intensity)
	assert.Equal(t, uint16(500), rumble.LeftSpeed)
	assert.Equal(t, uint16(900), rumble.RightSpeed)
	assert.Equal(t, int8(-40), rumble.LeftGain)
	assert.Equal(t, int8(60), rumble.RightGain)
}

func TestOutputCallbackPreservesCompatibilityNormalizationAndLength(t *testing.T) {
	var legacy steamdeck.OutputState
	assert.Equal(t, steamdeck.InputReportLen, legacy.Length())
	if err := legacy.UnmarshalBinary(make([]byte, steamdeck.InputReportLen)); err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, steamdeck.InputReportLen, legacy.Length())

	dev, err := steamdeck.New(nil)
	if !assert.NoError(t, err) {
		return
	}
	var outputs []steamdeck.OutputState
	dev.SetOutputCallback(func(out steamdeck.OutputState) {
		outputs = append(outputs, out)
	})

	dev.HandleTransfer(context.Background(), defaultControllerEndpoint, usbip.DirOut, []byte{0x00})
	dev.HandleTransfer(context.Background(), defaultControllerEndpoint, usbip.DirOut, []byte{0x00, steamdeck.FeatureClearDigitalMappings})
	dev.HandleControl(0x21, 0x09, uint16(0x0300|steamdeck.FeatureClearDigitalMappings), 0, 0, nil)

	if !assert.Len(t, outputs, 3) {
		return
	}
	assert.Equal(t, []byte{0x00}, outputs[0].Data[:outputs[0].Length()])
	assert.Equal(t, 1, outputs[0].Length())
	assert.Equal(t, []byte{steamdeck.FeatureClearDigitalMappings}, outputs[1].Data[:outputs[1].Length()])
	assert.Equal(t, []byte{steamdeck.FeatureClearDigitalMappings}, outputs[2].Data[:outputs[2].Length()])

	oversized := make([]byte, steamdeck.InputReportLen+7)
	oversized[0] = steamdeck.FeatureClearDigitalMappings
	dev.HandleTransfer(context.Background(), defaultControllerEndpoint, usbip.DirOut, oversized)
	assert.Equal(t, steamdeck.InputReportLen, outputs[3].Length())
}

func TestOutputCallbackSelfClearDoesNotDeadlock(t *testing.T) {
	dev, err := steamdeck.New(nil)
	if !assert.NoError(t, err) {
		return
	}
	var calls atomic.Int32
	dev.SetOutputCallback(func(steamdeck.OutputState) {
		calls.Add(1)
		dev.SetOutputCallback(nil)
	})
	dev.HandleTransfer(context.Background(), defaultControllerEndpoint, usbip.DirOut, []byte{0x99})
	dev.HandleTransfer(context.Background(), defaultControllerEndpoint, usbip.DirOut, []byte{0x99})
	assert.Equal(t, int32(1), calls.Load())
}

func TestOutputCallbackClearPreventsLaterCapture(t *testing.T) {
	dev, err := steamdeck.New(nil)
	if !assert.NoError(t, err) {
		return
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	callbackDone := make(chan struct{})
	var calls atomic.Int32
	dev.SetOutputCallback(func(steamdeck.OutputState) {
		calls.Add(1)
		close(entered)
		<-release
		close(callbackDone)
	})

	dispatchDone := make(chan struct{})
	go func() {
		dev.HandleTransfer(context.Background(), defaultControllerEndpoint, usbip.DirOut, []byte{0x99})
		close(dispatchDone)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("callback did not start")
	}

	clearDone := make(chan struct{})
	go func() {
		dev.SetOutputCallback(nil)
		close(clearDone)
	}()
	select {
	case <-clearDone:
	case <-time.After(time.Second):
		t.Fatal("callback clear did not return while callback was running")
	}

	dev.HandleTransfer(context.Background(), defaultControllerEndpoint, usbip.DirOut, []byte{0x98})
	assert.Equal(t, int32(1), calls.Load())
	close(release)
	select {
	case <-callbackDone:
	case <-time.After(time.Second):
		t.Fatal("captured callback did not finish")
	}
	select {
	case <-dispatchDone:
	case <-time.After(time.Second):
		t.Fatal("first dispatch did not finish")
	}
}

func TestSteamDeckRuntimeStateAndCallbackRaces(t *testing.T) {
	dev, err := steamdeck.New(nil)
	if !assert.NoError(t, err) {
		return
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				settings := []byte{0x00, steamdeck.FeatureSetSettingsValues, 0x03, steamdeck.SettingMousePointerEnabled, byte(j), 0x00}
				dev.HandleControl(0x21, 0x09, 0x0300, 0, uint16(len(settings)), settings)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				dev.HandleControl(0xa1, 0x01, uint16(0x0300|steamdeck.FeatureGetSettingsValues), 0, steamdeck.InputReportLen, nil)
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				dev.SetOutputCallback(func(steamdeck.OutputState) {})
				dev.HandleTransfer(context.Background(), defaultControllerEndpoint, usbip.DirOut, []byte{0x99})
				dev.SetOutputCallback(nil)
			}
		}()
	}
	wg.Wait()
}

func TestFeedbackCommandsWithLeadingReportID(t *testing.T) {
	dev, err := steamdeck.New(nil)
	if !assert.NoError(t, err) {
		return
	}

	var got steamdeck.OutputState
	dev.SetOutputCallback(func(out steamdeck.OutputState) {
		got = out
	})

	cmd := make([]byte, steamdeck.InputReportLen+1)
	cmd[0] = 0x00
	cmd[1] = steamdeck.FeatureTriggerRumbleCommand
	cmd[2] = 9
	cmd[3] = 3
	binary.LittleEndian.PutUint16(cmd[4:6], 0x0456)
	binary.LittleEndian.PutUint16(cmd[6:8], 250)
	binary.LittleEndian.PutUint16(cmd[8:10], 750)
	leadingLeftGain, leadingRightGain := int8(-10), int8(90)
	cmd[10] = byte(leadingLeftGain)
	cmd[11] = byte(leadingRightGain)

	_, handled := dev.HandleControl(0x21, 0x09, uint16(0x0300), 0, uint16(len(cmd)), cmd)
	if !assert.True(t, handled) {
		return
	}
	rumble, ok := got.AsRumble()
	if !assert.True(t, ok) {
		return
	}
	assert.Equal(t, uint8(3), rumble.RumbleType)
	assert.Equal(t, uint16(0x0456), rumble.Intensity)
	assert.Equal(t, uint16(250), rumble.LeftSpeed)
	assert.Equal(t, uint16(750), rumble.RightSpeed)
	assert.Equal(t, int8(-10), rumble.LeftGain)
	assert.Equal(t, int8(90), rumble.RightGain)
}

func TestRumbleCommandDecodesEachFieldFromDistinctOffsets(t *testing.T) {
	// Golden packet chosen so each field lands in a distinct, non-aliasing
	// range: RumbleType != any byte of Intensity/speeds, Intensity > 0xFF so
	// a truncated uint8 read would fail, LeftSpeed != RightSpeed, and the
	// gains have opposite signs so a signed/unsigned or offset mixup shows up.
	var out steamdeck.OutputState
	out.Data[0] = steamdeck.FeatureTriggerRumbleCommand
	out.Data[1] = 9
	out.Data[2] = 0xAB                                   // RumbleType
	binary.LittleEndian.PutUint16(out.Data[3:5], 0x1234) // Intensity
	binary.LittleEndian.PutUint16(out.Data[5:7], 0x2222) // LeftSpeed
	binary.LittleEndian.PutUint16(out.Data[7:9], 0x3333) // RightSpeed
	fieldLeftGain, fieldRightGain := int8(-100), int8(100)
	out.Data[9] = byte(fieldLeftGain)   // LeftGain
	out.Data[10] = byte(fieldRightGain) // RightGain

	rumble, ok := out.AsRumble()
	if !assert.True(t, ok) {
		return
	}
	assert.Equal(t, uint8(0xAB), rumble.RumbleType)
	assert.Equal(t, uint16(0x1234), rumble.Intensity)
	assert.Equal(t, uint16(0x2222), rumble.LeftSpeed)
	assert.Equal(t, uint16(0x3333), rumble.RightSpeed)
	assert.Equal(t, int8(-100), rumble.LeftGain)
	assert.Equal(t, int8(100), rumble.RightGain)
}

func TestOutputCallbackReceivesNonHapticCommands(t *testing.T) {
	dev, err := steamdeck.New(nil)
	if !assert.NoError(t, err) {
		return
	}

	var got steamdeck.OutputState
	dev.SetOutputCallback(func(out steamdeck.OutputState) {
		got = out
	})

	cmd := []byte{0x00, steamdeck.FeatureClearDigitalMappings}
	_, handled := dev.HandleControl(0x21, 0x09, uint16(0x0300), 0, uint16(len(cmd)), cmd)
	if !assert.True(t, handled) {
		return
	}

	assert.Equal(t, byte(steamdeck.FeatureClearDigitalMappings), got.CommandID())
}

func TestOutputParsers(t *testing.T) {
	var out steamdeck.OutputState

	out.Data[0] = steamdeck.FeatureTriggerHapticPulse
	binary.LittleEndian.PutUint16(out.Data[3:5], 0x01f4)
	binary.LittleEndian.PutUint16(out.Data[5:7], 0x01f4)
	binary.LittleEndian.PutUint16(out.Data[7:9], 0x001e)
	out.Data[9] = 0x00
	pulse, ok := out.AsHapticPulse()
	if !assert.True(t, ok) {
		return
	}
	assert.Equal(t, uint16(0x01f4), pulse.Duration)
	assert.Equal(t, uint16(0x001e), pulse.Count)

	out = steamdeck.OutputState{}
	out.Data[0] = steamdeck.FeaturePlayAudio
	out.Data[1] = 3
	copy(out.Data[2:5], []byte{0xaa, 0xbb, 0xcc})
	audio, ok := out.AsPlayAudio()
	if !assert.True(t, ok) {
		return
	}
	assert.Equal(t, []byte{0xaa, 0xbb, 0xcc}, audio.Payload)
}

func captureOutputState(t *testing.T, dev *steamdeck.SteamDeck, cmd []byte) steamdeck.OutputState {
	t.Helper()
	var got steamdeck.OutputState
	dev.SetOutputCallback(func(out steamdeck.OutputState) {
		got = out
	})
	_, handled := dev.HandleControl(0x21, 0x09, uint16(0x0300), 0, uint16(len(cmd)), cmd)
	if !assert.True(t, handled) {
		return got
	}
	return got
}

func TestRumbleRequiresActualCapturedLength(t *testing.T) {
	dev, err := steamdeck.New(nil)
	if !assert.NoError(t, err) {
		return
	}

	valid := []byte{steamdeck.FeatureTriggerRumbleCommand, 9, 5, 0, 0, 0xC8, 0, 0x2C, 1, 0xFB, 5}
	if !assert.Len(t, valid, 11) {
		return
	}
	rumble, ok := captureOutputState(t, dev, valid).AsRumble()
	if !assert.True(t, ok) {
		return
	}
	assert.Equal(t, uint8(5), rumble.RumbleType)
	assert.Equal(t, uint16(200), rumble.LeftSpeed)
	assert.Equal(t, uint16(300), rumble.RightSpeed)
	assert.Equal(t, int8(-5), rumble.LeftGain)
	assert.Equal(t, int8(5), rumble.RightGain)

	truncated := valid[:10]
	_, ok = captureOutputState(t, dev, truncated).AsRumble()
	assert.False(t, ok)

	commandIDOnly := []byte{steamdeck.FeatureTriggerRumbleCommand}
	_, ok = captureOutputState(t, dev, commandIDOnly).AsRumble()
	assert.False(t, ok)

	wrongCommand := append([]byte{steamdeck.FeatureTriggerHapticCommand}, valid[1:]...)
	_, ok = captureOutputState(t, dev, wrongCommand).AsRumble()
	assert.False(t, ok)
}

func TestHapticRequiresActualCapturedLength(t *testing.T) {
	dev, err := steamdeck.New(nil)
	if !assert.NoError(t, err) {
		return
	}

	gain := int8(-7)
	valid := []byte{steamdeck.FeatureTriggerHapticCommand, 4, 1, 2, 3, byte(gain)}
	if !assert.Len(t, valid, 6) {
		return
	}
	haptic, ok := captureOutputState(t, dev, valid).AsHaptic()
	if !assert.True(t, ok) {
		return
	}
	assert.Equal(t, uint8(1), haptic.Side)
	assert.Equal(t, uint8(2), haptic.Type)
	assert.Equal(t, uint8(3), haptic.Intensity)
	assert.Equal(t, int8(-7), haptic.Gain)

	truncated := valid[:5]
	_, ok = captureOutputState(t, dev, truncated).AsHaptic()
	assert.False(t, ok)
}

func TestHapticPulseRequiresActualCapturedLength(t *testing.T) {
	dev, err := steamdeck.New(nil)
	if !assert.NoError(t, err) {
		return
	}

	valid := make([]byte, 10)
	valid[0] = steamdeck.FeatureTriggerHapticPulse
	valid[1] = 8
	valid[2] = 1
	binary.LittleEndian.PutUint16(valid[3:5], 0x01f4)
	binary.LittleEndian.PutUint16(valid[5:7], 0x0064)
	binary.LittleEndian.PutUint16(valid[7:9], 0x001e)
	valid[9] = 9
	pulse, ok := captureOutputState(t, dev, valid).AsHapticPulse()
	if !assert.True(t, ok) {
		return
	}
	assert.Equal(t, uint16(0x01f4), pulse.Duration)
	assert.Equal(t, uint16(0x0064), pulse.Interval)
	assert.Equal(t, uint16(0x001e), pulse.Count)
	assert.Equal(t, uint8(9), pulse.Gain)

	truncated := valid[:9]
	_, ok = captureOutputState(t, dev, truncated).AsHapticPulse()
	assert.False(t, ok)
}

func TestPlayAudioRequiresActualCapturedLength(t *testing.T) {
	dev, err := steamdeck.New(nil)
	if !assert.NoError(t, err) {
		return
	}

	exact := []byte{steamdeck.FeaturePlayAudio, 3, 0xAA, 0xBB, 0xCC}
	audio, ok := captureOutputState(t, dev, exact).AsPlayAudio()
	if !assert.True(t, ok) {
		return
	}
	assert.Equal(t, []byte{0xAA, 0xBB, 0xCC}, audio.Payload)

	declaredLargerThanActual := []byte{steamdeck.FeaturePlayAudio, 3, 0xAA, 0xBB}
	_, ok = captureOutputState(t, dev, declaredLargerThanActual).AsPlayAudio()
	assert.False(t, ok)

	shorterThanHeader := []byte{steamdeck.FeaturePlayAudio}
	_, ok = captureOutputState(t, dev, shorterThanHeader).AsPlayAudio()
	assert.False(t, ok)

	beyondFixedCapacity := []byte{steamdeck.FeaturePlayAudio, 200}
	_, ok = captureOutputState(t, dev, beyondFixedCapacity).AsPlayAudio()
	assert.False(t, ok)
}

func TestLeadingReportIDNormalizationPreservesActualLength(t *testing.T) {
	dev, err := steamdeck.New(nil)
	if !assert.NoError(t, err) {
		return
	}

	normGain := int8(-1)
	validWithLeadingReportID := []byte{0x00, steamdeck.FeatureTriggerHapticCommand, 4, 1, 2, 3, byte(normGain)}
	got := captureOutputState(t, dev, validWithLeadingReportID)
	assert.Equal(t, 6, got.Length())
	haptic, ok := got.AsHaptic()
	if !assert.True(t, ok) {
		return
	}
	assert.Equal(t, uint8(1), haptic.Side)

	shortWithLeadingReportID := []byte{0x00, steamdeck.FeatureTriggerHapticCommand, 4, 1, 2, 3}
	got = captureOutputState(t, dev, shortWithLeadingReportID)
	assert.Equal(t, 5, got.Length())
	_, ok = got.AsHaptic()
	assert.False(t, ok)
}

func TestLegacyDirectlyConstructedOutputStateUsesFixedBufferSentinel(t *testing.T) {
	var out steamdeck.OutputState
	assert.Equal(t, steamdeck.InputReportLen, out.Length())

	out.Data[0] = steamdeck.FeatureTriggerHapticCommand
	out.Data[2] = 1
	out.Data[3] = 2
	out.Data[4] = 3
	legacyGain := int8(-1)
	out.Data[5] = byte(legacyGain)
	haptic, ok := out.AsHaptic()
	if !assert.True(t, ok) {
		return
	}
	assert.Equal(t, uint8(1), haptic.Side)
}

func TestFeatureResponses(t *testing.T) {
	dev, err := steamdeck.New(nil)
	if !assert.NoError(t, err) {
		return
	}

	resp, handled := dev.HandleControl(0xa1, 0x01, uint16(0x0300|steamdeck.FeatureGetAttributesValues), 0, steamdeck.InputReportLen, nil)
	if !assert.True(t, handled) {
		return
	}
	assert.Equal(t, byte(steamdeck.FeatureGetAttributesValues), resp[0])
	expected := []byte{
		steamdeck.FeatureGetAttributesValues, 0x2d,
		0x01, 0x05, 0x12, 0x00, 0x00,
		0x02, 0x00, 0x00, 0x00, 0x00,
		0x0a, 0x2b, 0x12, 0xa9, 0x62,
		0x04, 0xb7, 0x61, 0x7c, 0x67,
		0x09, 0x2e, 0x00, 0x00, 0x00,
		0x0b, 0xa0, 0x0f, 0x00, 0x00,
		0x0d, 0x00, 0x00, 0x00, 0x00,
		0x0c, 0x00, 0x00, 0x00, 0x00,
		0x0e, 0x00, 0x00, 0x00, 0x00,
	}
	assert.Equal(t, expected, resp[:len(expected)])
	assert.Equal(t, make([]byte, steamdeck.InputReportLen-len(expected)), resp[len(expected):])

	customPID := uint16(0x4321)
	custom, err := steamdeck.New(&device.CreateOptions{IDProduct: &customPID})
	if !assert.NoError(t, err) {
		return
	}
	customResp, handled := custom.HandleControl(0xa1, 0x01, uint16(0x0300|steamdeck.FeatureGetAttributesValues), 0, steamdeck.InputReportLen, nil)
	if !assert.True(t, handled) {
		return
	}
	assert.Equal(t, uint32(customPID), binary.LittleEndian.Uint32(customResp[3:7]))

	resp, handled = dev.HandleControl(0xa1, 0x01, uint16(0x0300|steamdeck.FeatureGetStringAttribute), 0, steamdeck.InputReportLen, nil)
	if !assert.True(t, handled) {
		return
	}
	assert.Equal(t, byte(steamdeck.FeatureGetStringAttribute), resp[0])
	assert.Equal(t, byte(len("SteamDeck-0001")), resp[1])
	assert.Equal(t, byte(steamdeck.StringAttributeBoardSerial), resp[2])
	assert.Contains(t, string(resp[3:3+int(resp[1])]), "SteamDeck")

	resp, handled = dev.HandleControl(0xa1, 0x01, uint16(0x0300|steamdeck.FeatureGetSettingsValues), 0, steamdeck.InputReportLen, nil)
	if !assert.True(t, handled) {
		return
	}
	assert.Equal(t, byte(steamdeck.FeatureGetSettingsValues), resp[0])
	assert.NotZero(t, resp[1])
	assert.Equal(t, byte(steamdeck.SettingLeftTrackpadMode), resp[2])
	assert.Equal(t, uint16(steamdeck.TrackpadModeNone), binary.LittleEndian.Uint16(resp[3:5]))
	assert.Contains(t, resp[2:2+int(resp[1])], byte(steamdeck.SettingIMUMode))

	resp, handled = dev.HandleControl(0xa1, 0x01, uint16(0x0300|steamdeck.FeatureGetSettingsMaxs), 0, steamdeck.InputReportLen, nil)
	if !assert.True(t, handled) {
		return
	}
	assert.Equal(t, byte(steamdeck.FeatureGetSettingsMaxs), resp[0])
	assert.NotZero(t, resp[1])
	assert.Contains(t, resp[2:2+int(resp[1])], byte(steamdeck.SettingIMUMode))
}

func TestFeatureResponsesViaSetThenGetReportZero(t *testing.T) {
	dev, err := steamdeck.New(nil)
	if !assert.NoError(t, err) {
		return
	}

	request := []byte{0x00, steamdeck.FeatureGetStringAttribute, 0x15, steamdeck.StringAttributeUnitSerial}
	_, handled := dev.HandleControl(0x21, 0x09, uint16(0x0300), 0, uint16(len(request)), request)
	if !assert.True(t, handled) {
		return
	}

	resp, handled := dev.HandleControl(0xa1, 0x01, uint16(0x0300), 0, steamdeck.InputReportLen, nil)
	if !assert.True(t, handled) {
		return
	}
	assert.Equal(t, byte(steamdeck.FeatureGetStringAttribute), resp[0])
	assert.Equal(t, byte(steamdeck.StringAttributeUnitSerial), resp[2])
	assert.Contains(t, string(resp[3:3+int(resp[1])-1]), "SteamDeck")

	request = []byte{0x00, steamdeck.FeatureGetDeviceInfo}
	_, handled = dev.HandleControl(0x21, 0x09, uint16(0x0300), 0, uint16(len(request)), request)
	if !assert.True(t, handled) {
		return
	}

	resp, handled = dev.HandleControl(0xa1, 0x01, uint16(0x0300), 0, steamdeck.InputReportLen, nil)
	if !assert.True(t, handled) {
		return
	}
	assert.Equal(t, byte(steamdeck.FeatureGetDeviceInfo), resp[0])
	assert.Equal(t, uint16(steamdeck.DefaultVID), binary.LittleEndian.Uint16(resp[4:6]))
	assert.Equal(t, uint16(steamdeck.DefaultPID), binary.LittleEndian.Uint16(resp[6:8]))
}

func TestSetSettingsAndControllerMode(t *testing.T) {
	dev, err := steamdeck.New(nil)
	if !assert.NoError(t, err) {
		return
	}

	settings := []byte{
		0x00,
		steamdeck.FeatureSetSettingsValues,
		0x06,
		steamdeck.SettingLeftTrackpadMode, byte(steamdeck.TrackpadModeNone), 0x00,
		steamdeck.SettingMousePointerEnabled, byte(steamdeck.MousePointerDisabled), 0x00,
	}
	_, handled := dev.HandleControl(0x21, 0x09, uint16(0x0300), 0, uint16(len(settings)), settings)
	if !assert.True(t, handled) {
		return
	}

	mode := []byte{0x00, steamdeck.FeatureSetControllerMode, 0x01, steamdeck.LizardModeOff}
	_, handled = dev.HandleControl(0x21, 0x09, uint16(0x0300), 0, uint16(len(mode)), mode)
	if !assert.True(t, handled) {
		return
	}

	resp, handled := dev.HandleControl(0xa1, 0x01, uint16(0x0300|steamdeck.FeatureGetDeviceInfo), 0, steamdeck.InputReportLen, nil)
	if !assert.True(t, handled) {
		return
	}
	assert.Equal(t, byte(steamdeck.LizardModeOff), resp[9])
}

func TestSettingMousePointerEnabledIsPlainSetting(t *testing.T) {
	dev, err := steamdeck.New(nil)
	if !assert.NoError(t, err) {
		return
	}

	settings := []byte{
		0x00,
		steamdeck.FeatureSetSettingsValues,
		0x03,
		steamdeck.SettingMousePointerEnabled, byte(steamdeck.MousePointerEnabled), 0x00,
	}
	_, handled := dev.HandleControl(0x21, 0x09, uint16(0x0300), 0, uint16(len(settings)), settings)
	if !assert.True(t, handled) {
		return
	}

	resp, handled := dev.HandleControl(0xa1, 0x01, uint16(0x0300|steamdeck.FeatureGetSettingsValues), 0, steamdeck.InputReportLen, nil)
	if !assert.True(t, handled) {
		return
	}
	assert.Equal(t, byte(steamdeck.FeatureGetSettingsValues), resp[0])
	payload := resp[2 : 2+int(resp[1])]
	if !assert.Contains(t, payload, byte(steamdeck.SettingMousePointerEnabled)) {
		return
	}
	for offset := 0; offset+3 <= len(payload); offset += 3 {
		if payload[offset] != byte(steamdeck.SettingMousePointerEnabled) {
			continue
		}
		value := binary.LittleEndian.Uint16(payload[offset+1 : offset+3])
		assert.Equal(t, uint16(steamdeck.MousePointerEnabled), value)
		return
	}
	t.Fatal("SettingMousePointerEnabled triple not found in response")
}

func TestSettingMousePointerEnabledDoesNotChangeControllerMode(t *testing.T) {
	dev, err := steamdeck.New(nil)
	if !assert.NoError(t, err) {
		return
	}

	before, handled := dev.HandleControl(0xa1, 0x01, uint16(0x0300|steamdeck.FeatureGetDeviceInfo), 0, steamdeck.InputReportLen, nil)
	if !assert.True(t, handled) {
		return
	}
	initialMode := before[9]

	settings := []byte{
		0x00,
		steamdeck.FeatureSetSettingsValues,
		0x03,
		steamdeck.SettingMousePointerEnabled, initialMode ^ 0x01, 0x00,
	}
	_, handled = dev.HandleControl(0x21, 0x09, uint16(0x0300), 0, uint16(len(settings)), settings)
	if !assert.True(t, handled) {
		return
	}

	after, handled := dev.HandleControl(0xa1, 0x01, uint16(0x0300|steamdeck.FeatureGetDeviceInfo), 0, steamdeck.InputReportLen, nil)
	if !assert.True(t, handled) {
		return
	}
	assert.Equal(t, initialMode, after[9])
}

func TestSetControllerModeStillChangesMode(t *testing.T) {
	dev, err := steamdeck.New(nil)
	if !assert.NoError(t, err) {
		return
	}

	mode := []byte{0x00, steamdeck.FeatureSetControllerMode, 0x01, steamdeck.LizardModeOff}
	_, handled := dev.HandleControl(0x21, 0x09, uint16(0x0300), 0, uint16(len(mode)), mode)
	if !assert.True(t, handled) {
		return
	}

	resp, handled := dev.HandleControl(0xa1, 0x01, uint16(0x0300|steamdeck.FeatureGetDeviceInfo), 0, steamdeck.InputReportLen, nil)
	if !assert.True(t, handled) {
		return
	}
	assert.Equal(t, byte(steamdeck.LizardModeOff), resp[9])
}

func TestAPIStreamAndUSBInput(t *testing.T) {
	s := viiperTesting.NewTestServer(t)
	defer s.UsbServer.Close()
	defer s.ApiServer.Close()

	r := s.ApiServer.Router()
	r.Register("bus/{id}/add", handler.BusDeviceAdd(s.UsbServer, s.ApiServer))
	r.RegisterStream("bus/{busId}/{deviceid}", api.DeviceStreamHandler(s.UsbServer))

	if err := s.ApiServer.Start(); err != nil {
		t.Fatalf("Failed to start API server: %v", err)
	}

	b, err := virtualbus.NewWithBusID(1)
	if err != nil {
		t.Fatalf("Failed to create virtual bus: %v", err)
	}
	defer b.Close()
	_ = s.UsbServer.AddBus(b)

	client := viiperclient.New(s.ApiServer.Addr())
	stream, _, err := client.AddDeviceAndConnect(context.Background(), b.BusID(), "steamdeck", nil)
	if !assert.NoError(t, err) {
		return
	}
	defer stream.Close()

	usbipClient := viiperTesting.NewUsbIpClient(t, s.UsbServer.Addr())
	devs, err := usbipClient.ListDevices()
	if !assert.NoError(t, err) || !assert.Len(t, devs, 1) {
		return
	}
	imp, err := usbipClient.AttachDevice(devs[0].BusID)
	if !assert.NoError(t, err) {
		return
	}
	if imp != nil && imp.Conn != nil {
		defer imp.Conn.Close()
	}

	seq := uint32(0)
	readInputReport := func(timeout time.Duration) ([]byte, error) {
		seq++
		cmd := usbip.CmdSubmit{
			Basic:             usbip.HeaderBasic{Command: usbip.CmdSubmitCode, Seqnum: seq, Devid: 0, Dir: usbip.DirIn, Ep: defaultControllerEndpoint},
			TransferBufferLen: 255,
		}
		_ = imp.Conn.SetDeadline(time.Now().Add(timeout))
		if err := cmd.Write(imp.Conn); err != nil {
			return nil, err
		}
		var retHdr [48]byte
		if err := usbip.ReadExactly(imp.Conn, retHdr[:]); err != nil {
			return nil, err
		}
		actual := binary.BigEndian.Uint32(retHdr[24:28])
		data := make([]byte, int(actual))
		if actual > 0 {
			if err := usbip.ReadExactly(imp.Conn, data); err != nil {
				return nil, err
			}
		}
		_ = imp.Conn.SetDeadline(time.Time{})
		return data, nil
	}

	state := steamdeck.InputState{A: true, QuickAccess: true, LTrigger: 222, LStickX: -1234}
	if !assert.NoError(t, stream.WriteBinary(&state)) {
		return
	}
	got, err := readInputReport(750 * time.Millisecond)
	if !assert.NoError(t, err) {
		return
	}
	assert.Len(t, got, steamdeck.InputReportLen)
	assert.Equal(t, byte(0x80), got[8]&0x80)
	assert.Equal(t, byte(0x04), got[14]&0x04)
	assert.Equal(t, uint16(222), binary.LittleEndian.Uint16(got[44:46]))
	assert.Equal(t, uint16(0xfb2e), binary.LittleEndian.Uint16(got[48:50]))

	cmd := make([]byte, steamdeck.InputReportLen)
	cmd[0] = steamdeck.FeatureTriggerHapticCommand
	cmd[1] = 13
	cmd[2] = steamdeck.PadSideLeft
	cmd[3] = steamdeck.CommandTypeClick
	cmd[4] = steamdeck.IntensityLong
	cmd[5] = 0xf9
	dev := b.GetAllDeviceMetas()[0].Dev
	dev.HandleTransfer(context.Background(), defaultControllerEndpoint, usbip.DirOut, cmd)

	var feedback [steamdeck.InputReportLen]byte
	_ = stream.SetReadDeadline(time.Now().Add(750 * time.Millisecond))
	_, err = io.ReadFull(stream, feedback[:])
	if !assert.NoError(t, err) {
		return
	}
	assert.Equal(t, byte(steamdeck.FeatureTriggerHapticCommand), feedback[0])
	assert.Equal(t, byte(steamdeck.CommandTypeClick), feedback[3])
}
