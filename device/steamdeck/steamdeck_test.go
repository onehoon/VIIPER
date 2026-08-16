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
				assert.Equal(t, byte(steamdeck.DeckInputPayloadLen), got[3])
				assert.Equal(t, make([]byte, 7), got[8:15])
				assert.Equal(t, uint16(0), binary.LittleEndian.Uint16(got[36:38]))
				assert.Equal(t, uint16(0), binary.LittleEndian.Uint16(got[38:40]))
				assert.Equal(t, uint16(0), binary.LittleEndian.Uint16(got[40:42]))
				assert.Equal(t, uint16(0), binary.LittleEndian.Uint16(got[42:44]))
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
	cmd[3] = 7
	cmd[4] = steamdeck.IntensityMedium
	binary.LittleEndian.PutUint16(cmd[5:7], 500)
	binary.LittleEndian.PutUint16(cmd[7:9], 900)

	_, handled := dev.HandleControl(0x21, 0x09, uint16(0x0300), 0, uint16(len(cmd)), cmd)
	if !assert.True(t, handled) {
		return
	}
	rumble, ok := got.AsRumble()
	if !assert.True(t, ok) {
		return
	}
	assert.Equal(t, uint8(7), rumble.EventType)
	assert.Equal(t, uint8(steamdeck.IntensityMedium), rumble.Intensity)
	assert.Equal(t, uint16(500), rumble.LeftSpeed)
	assert.Equal(t, uint16(900), rumble.RightSpeed)
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
	cmd[4] = steamdeck.IntensityShort
	binary.LittleEndian.PutUint16(cmd[6:8], 250)
	binary.LittleEndian.PutUint16(cmd[8:10], 750)

	_, handled := dev.HandleControl(0x21, 0x09, uint16(0x0300), 0, uint16(len(cmd)), cmd)
	if !assert.True(t, handled) {
		return
	}
	rumble, ok := got.AsRumble()
	if !assert.True(t, ok) {
		return
	}
	assert.Equal(t, uint16(250), rumble.LeftSpeed)
	assert.Equal(t, uint16(750), rumble.RightSpeed)
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
	assert.Equal(t, byte(30), resp[1])
	assert.Equal(t, byte(steamdeck.AttributeProductID), resp[7])
	assert.Equal(t, uint32(steamdeck.DefaultPID), binary.LittleEndian.Uint32(resp[8:12]))
	assert.Equal(t, byte(steamdeck.AttributeConnectionIntervalUs), resp[27])
	assert.Equal(t, uint32(4000), binary.LittleEndian.Uint32(resp[28:32]))

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
