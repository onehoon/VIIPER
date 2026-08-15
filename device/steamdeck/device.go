package steamdeck

import (
	"context"
	"encoding/binary"
	"sync"

	"github.com/Alia5/VIIPER/device"
	"github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usb/hid"
	"github.com/Alia5/VIIPER/usbip"
)

const (
	keyboardInterfaceNumber   = 0x00
	mouseInterfaceNumber      = 0x01
	controllerInterfaceNumber = 0x02

	keyboardEndpointNumber   = 0x01
	mouseEndpointNumber      = 0x02
	controllerEndpointNumber = 0x03
	genericEndpointNumber    = 0x01
)

var zeroMouseReport = []byte{0x00, 0x00, 0x00, 0x00}

var zeroKeyboardReport = []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}

type SteamDeck struct {
	inputState          *InputState
	mtx                 sync.Mutex
	featureMtx          sync.Mutex
	callbackMu          sync.RWMutex
	stateMu             sync.RWMutex
	outputFunc          func(OutputState)
	frame               uint32
	descriptor          usb.Descriptor
	controller          controllerState
	lastFeatureResponse []byte
}

type controllerState struct {
	mode         byte
	settings     map[uint8]uint16
	boardSerial  string
	unitSerial   string
	uniqueID     uint32
	boardRev     uint32
	firmwareTime uint32
}

var defaultSettings = map[uint8]uint16{
	SettingLeftTrackpadMode:    TrackpadModeNone,
	SettingRightTrackpadMode:   TrackpadModeNone,
	SettingLizardMode:          LizardModeOff,
	SettingSmoothAbsoluteMouse: 0,
	// Advertise raw gyro + raw accel only (no SendOrientation): we don't compute a live
	// orientation quaternion, so claiming orientation made Steam lean on a frozen identity
	// quat and ignore our raw angular velocity (gyro-to-stick collapsed to center). This
	// matches InputPlumber's Steam Deck target, which sends raw gyro+accel and no quaternion.
	SettingIMUMode:                    GyroModeSendRawAccel | GyroModeSendRawGyro,
	SettingLeftTrackpadClickPressure:  0xffff,
	SettingRightTrackpadClickPressure: 0xffff,
	SettingSteamWatchdogEnable:        1,
}

var maxSettings = map[uint8]uint16{
	SettingLeftTrackpadMode:           TrackpadModeNone,
	SettingRightTrackpadMode:          TrackpadModeNone,
	SettingLizardMode:                 LizardModeOn,
	SettingSmoothAbsoluteMouse:        1,
	SettingIMUMode:                    GyroModeSteering | GyroModeTilt | GyroModeSendRawAccel | GyroModeSendRawGyro,
	SettingLeftTrackpadClickPressure:  0xffff,
	SettingRightTrackpadClickPressure: 0xffff,
	SettingSteamWatchdogEnable:        1,
}

var settingsOrder = []uint8{
	SettingLeftTrackpadMode,
	SettingRightTrackpadMode,
	SettingLizardMode,
	SettingSmoothAbsoluteMouse,
	SettingIMUMode,
	SettingLeftTrackpadClickPressure,
	SettingRightTrackpadClickPressure,
	SettingSteamWatchdogEnable,
}

func newControllerState() controllerState {
	state := controllerState{
		settings:     cloneSettings(defaultSettings),
		boardSerial:  "SteamDeck-0001",
		unitSerial:   "SteamDeck-0001",
		uniqueID:     0x53444543,
		boardRev:     1,
		firmwareTime: 0x20260226,
		mode:         LizardModeOn,
	}
	return state
}

func cloneDescriptor() usb.Descriptor {
	d := defaultDescriptor
	strings := make(map[uint8]string, len(defaultDescriptor.Strings))
	for key, value := range defaultDescriptor.Strings {
		strings[key] = value
	}
	d.Strings = strings
	return d
}

func New(o *device.CreateOptions) (*SteamDeck, error) {
	d := &SteamDeck{
		descriptor: cloneDescriptor(),
		inputState: &InputState{},
		controller: newControllerState(),
	}
	if o != nil {
		if o.IDVendor != nil {
			d.descriptor.Device.IDVendor = *o.IDVendor
		}
		if o.IDProduct != nil {
			d.descriptor.Device.IDProduct = *o.IDProduct
		}
	}
	return d, nil
}

func (d *SteamDeck) SetOutputCallback(f func(OutputState)) {
	d.callbackMu.Lock()
	defer d.callbackMu.Unlock()
	d.outputFunc = f
}

func (d *SteamDeck) UpdateInputState(state *InputState) {
	d.setInputState(state)
}

func (d *SteamDeck) setInputState(state *InputState) {
	if state == nil {
		state = &InputState{}
	}
	updated := *state
	d.mtx.Lock()
	d.frame++
	updated.Frame = d.frame
	d.inputState = &updated
	d.mtx.Unlock()
}

func (d *SteamDeck) snapshotInputState() InputState {
	d.mtx.Lock()
	defer d.mtx.Unlock()
	return *d.inputState
}

func (d *SteamDeck) HandleTransfer(ctx context.Context, ep uint32, dir uint32, out []byte) []byte {
	if dir == usbip.DirIn {
		switch ep {
		case mouseEndpointNumber:
			return append([]byte(nil), zeroMouseReport...)
		case keyboardEndpointNumber:
			return append([]byte(nil), zeroKeyboardReport...)
		case controllerEndpointNumber:
			st := d.snapshotInputState()
			return st.buildReport(st.Frame, DeckInputPayloadLen)
		default:
			return nil
		}
	}
	if dir == usbip.DirOut && ep == controllerEndpointNumber {
		d.handleHostCommand(normalizeHostCommand(out, 0))
	}
	return nil
}

func (d *SteamDeck) HandleControl(bmRequestType, bRequest uint8, wValue, _ /* wIndex */, wLength uint16, data []byte) ([]byte, bool) {
	const (
		hidGetReport = 0x01
		hidSetReport = 0x09

		reportTypeInput   = 0x01
		reportTypeOutput  = 0x02
		reportTypeFeature = 0x03
	)

	reportType := uint8(wValue >> 8)
	reportID := uint8(wValue & 0xff)

	if bmRequestType == 0xa1 && bRequest == hidGetReport {
		switch reportType {
		case reportTypeInput:
			st := d.snapshotInputState()
			report := st.buildReport(st.Frame, DeckInputPayloadLen)
			if wLength > 0 && int(wLength) < len(report) {
				return report[:wLength], true
			}
			return report, true
		case reportTypeFeature:
			resp := d.getFeatureResponse(reportID)
			if resp == nil {
				return nil, false
			}
			if wLength > 0 && int(wLength) < len(resp) {
				return resp[:wLength], true
			}
			return resp, true
		}
	}

	if bmRequestType == 0x21 && bRequest == hidSetReport {
		if reportType == reportTypeOutput || reportType == reportTypeFeature {
			data = normalizeHostCommand(data, reportID)
			if reportType == reportTypeFeature {
				d.setFeatureResponse(data)
			}
			d.handleHostCommand(data)
			return nil, true
		}
	}

	return nil, false
}

func normalizeHostCommand(data []byte, reportID uint8) []byte {
	if len(data) == 0 && reportID != 0 {
		return []byte{reportID}
	}
	if len(data) > 1 && data[0] == 0x00 {
		data = data[1:]
	}
	if len(data) > InputReportLen {
		data = data[:InputReportLen]
	}
	return append([]byte(nil), data...)
}

func (d *SteamDeck) handleHostCommand(data []byte) {
	if len(data) == 0 {
		return
	}
	d.stateMu.Lock()
	switch data[0] {
	case FeatureSetControllerMode:
		if len(data) >= 3 {
			d.controller.mode = data[2]
		}
	case FeatureSetSettingsValues:
		d.applySettingsLocked(data)
	case FeatureLoadDefaultSettings, FeatureFactoryReset:
		d.controller = newControllerState()
	case FeatureClearSettingsValues:
		d.resetSettingsLocked()
	case FeatureClearDigitalMappings, FeatureSetDefaultMappings, FeatureSetDigitalMappings, FeatureResetIMU:
		// Accepted for protocol compatibility.
	}
	d.stateMu.Unlock()

	d.callbackMu.RLock()
	callback := d.outputFunc
	d.callbackMu.RUnlock()
	if callback == nil {
		return
	}
	callback(newOutputState(data))
}

func cloneSettings(src map[uint8]uint16) map[uint8]uint16 {
	dst := make(map[uint8]uint16, len(src))
	for setting, value := range src {
		dst[setting] = value
	}
	return dst
}

func (d *SteamDeck) resetSettingsLocked() {
	state := newControllerState()
	d.controller.settings = cloneSettings(state.settings)
	d.controller.mode = state.mode
}

func (d *SteamDeck) applySettingsLocked(data []byte) {
	if len(data) < 2 {
		return
	}
	payloadLen := int(data[1])
	if payloadLen > len(data)-2 {
		payloadLen = len(data) - 2
	}
	for offset := 2; offset+2 < 2+payloadLen; offset += 3 {
		setting := data[offset]
		value := binary.LittleEndian.Uint16(data[offset+1 : offset+3])
		d.controller.settings[setting] = value
		if setting == SettingLizardMode {
			d.controller.mode = byte(value)
		}
	}
}

func (d *SteamDeck) getFeatureResponse(reportID uint8) []byte {
	d.featureMtx.Lock()
	defer d.featureMtx.Unlock()
	if reportID != 0 {
		return d.featureResponse([]byte{reportID})
	}
	if len(d.lastFeatureResponse) == 0 {
		return nil
	}
	return append([]byte(nil), d.lastFeatureResponse...)
}

func (d *SteamDeck) setFeatureResponse(request []byte) {
	d.featureMtx.Lock()
	defer d.featureMtx.Unlock()
	resp := d.featureResponse(request)
	if resp == nil {
		d.lastFeatureResponse = nil
		return
	}
	d.lastFeatureResponse = append([]byte(nil), resp...)
}

func (d *SteamDeck) featureResponse(request []byte) []byte {
	if len(request) == 0 {
		return nil
	}
	reportID := request[0]
	resp := make([]byte, InputReportLen)
	resp[0] = reportID
	switch reportID {
	case FeatureSetSettingsValues, FeatureClearDigitalMappings, FeatureSetDefaultMappings, FeatureSetDigitalMappings,
		FeatureClearSettingsValues, FeatureSetControllerMode, FeatureLoadDefaultSettings, FeatureFactoryReset, FeatureResetIMU:
		copy(resp, request)
		return resp
	case FeatureGetDeviceInfo:
		d.stateMu.RLock()
		mode := d.controller.mode
		d.stateMu.RUnlock()
		resp[1] = InputReportLen
		binary.LittleEndian.PutUint16(resp[4:6], d.descriptor.Device.IDVendor)
		binary.LittleEndian.PutUint16(resp[6:8], d.descriptor.Device.IDProduct)
		resp[8] = 0x01
		resp[9] = mode
		copy(resp[16:], []byte(d.descriptor.Strings[2]))
		return resp
	case FeatureGetChipID:
		resp[1] = 14
		copy(resp[4:], []byte("STEAMDECK-0001"))
		return resp
	case FeatureGetAttributesValues:
		d.stateMu.RLock()
		resp[1] = d.fillAttributesLocked(resp[2:])
		d.stateMu.RUnlock()
		return resp
	case FeatureGetStringAttribute:
		d.stateMu.RLock()
		resp[1] = d.fillStringAttributeLocked(resp[2:], request)
		d.stateMu.RUnlock()
		return resp
	case FeatureGetDigitalMappings:
		resp[1] = 0
		return resp
	case FeatureGetSettingsValues:
		d.stateMu.RLock()
		resp[1] = fillSettings(resp[2:], d.controller.settings)
		d.stateMu.RUnlock()
		return resp
	case FeatureGetSettingsDefaults:
		resp[1] = fillSettings(resp[2:], defaultSettings)
		return resp
	case FeatureGetSettingsMaxs:
		resp[1] = fillSettings(resp[2:], maxSettings)
		return resp
	default:
		return nil
	}
}

func fillSettings(buf []byte, settings map[uint8]uint16) byte {
	offset := 0
	for _, setting := range settingsOrder {
		if offset+3 > len(buf) {
			break
		}
		buf[offset] = setting
		binary.LittleEndian.PutUint16(buf[offset+1:offset+3], settings[setting])
		offset += 3
	}
	return byte(offset)
}

func (d *SteamDeck) fillAttributesLocked(buf []byte) byte {
	entries := []struct {
		tag   byte
		value uint32
	}{
		{tag: AttributeUniqueID, value: d.controller.uniqueID},
		{tag: AttributeProductID, value: uint32(d.descriptor.Device.IDProduct)},
		{tag: AttributeCapabilities, value: CapabilityGamepad},
		{tag: AttributeFirmwareBuildTime, value: d.controller.firmwareTime},
		{tag: AttributeBoardRevision, value: d.controller.boardRev},
		{tag: AttributeConnectionIntervalUs, value: 4000},
	}

	offset := 0
	for _, entry := range entries {
		if offset+5 > len(buf) {
			break
		}
		buf[offset] = entry.tag
		binary.LittleEndian.PutUint32(buf[offset+1:offset+5], entry.value)
		offset += 5
	}
	return byte(offset)
}

func (d *SteamDeck) fillStringAttributeLocked(buf []byte, request []byte) byte {
	if len(buf) < 2 {
		return 0
	}
	attribute := byte(StringAttributeBoardSerial)
	if len(request) >= 3 {
		attribute = request[2]
	}
	buf[0] = attribute

	value := d.controller.boardSerial
	if attribute == byte(StringAttributeUnitSerial) {
		value = d.controller.unitSerial
	}
	count := copy(buf[1:], []byte(value))
	return byte(count)
}

func (d *SteamDeck) GetDescriptor() *usb.Descriptor {
	return &d.descriptor
}

func (d *SteamDeck) GetDeviceSpecificArgs() map[string]any {
	return map[string]any{"profile": DefaultProfile}
}

var reportDescriptor = hid.ReportDescriptor{
	Items: []hid.Item{
		hid.UsagePage{Page: 0xffff},
		hid.Usage{Usage: 0x01},
		hid.Collection{Kind: hid.CollectionApplication, Items: []hid.Item{
			hid.Usage{Usage: 0x02},
			hid.Usage{Usage: 0x03},
			hid.LogicalMinimum{Min: 0},
			hid.LogicalMaximum{Max: 255},
			hid.ReportSize{Bits: 8},
			hid.ReportCount{Count: 64},
			hid.Input{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
			hid.Usage{Usage: 0x06},
			hid.Usage{Usage: 0x07},
			hid.LogicalMinimum{Min: 0},
			hid.LogicalMaximum{Max: 255},
			hid.ReportSize{Bits: 8},
			hid.ReportCount{Count: 64},
			hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
		}},
	},
}

var mouseReportDescriptor = hid.ReportDescriptor{
	Items: []hid.Item{
		hid.UsagePage{Page: hid.UsagePageGenericDesktop},
		hid.Usage{Usage: hid.UsageMouse},
		hid.Collection{Kind: hid.CollectionApplication, Items: []hid.Item{
			hid.Usage{Usage: hid.UsagePointer},
			hid.Collection{Kind: hid.CollectionPhysical, Items: []hid.Item{
				hid.UsagePage{Page: hid.UsagePageButton},
				hid.UsageMinimum{Min: 0x01},
				hid.UsageMaximum{Max: 0x05},
				hid.LogicalMinimum{Min: 0},
				hid.LogicalMaximum{Max: 1},
				hid.ReportCount{Count: 5},
				hid.ReportSize{Bits: 1},
				hid.Input{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
				hid.ReportCount{Count: 1},
				hid.ReportSize{Bits: 3},
				hid.Input{Flags: hid.MainConst},
				hid.UsagePage{Page: hid.UsagePageGenericDesktop},
				hid.Usage{Usage: hid.UsageX},
				hid.Usage{Usage: hid.UsageY},
				hid.Usage{Usage: hid.UsageWheel},
				hid.LogicalMinimum{Min: -127},
				hid.LogicalMaximum{Max: 127},
				hid.ReportSize{Bits: 8},
				hid.ReportCount{Count: 3},
				hid.Input{Flags: hid.MainData | hid.MainVar | hid.MainRel},
			}},
		}},
	},
}

var keyboardReportDescriptor = hid.ReportDescriptor{
	Items: []hid.Item{
		hid.UsagePage{Page: hid.UsagePageGenericDesktop},
		hid.Usage{Usage: hid.UsageKeyboard},
		hid.Collection{Kind: hid.CollectionApplication, Items: []hid.Item{
			hid.UsagePage{Page: hid.UsagePageKeyboard},
			hid.UsageMinimum{Min: 0xE0},
			hid.UsageMaximum{Max: 0xE7},
			hid.LogicalMinimum{Min: 0},
			hid.LogicalMaximum{Max: 1},
			hid.ReportSize{Bits: 1},
			hid.ReportCount{Count: 8},
			hid.Input{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
			hid.ReportSize{Bits: 8},
			hid.ReportCount{Count: 1},
			hid.Input{Flags: hid.MainConst},
			hid.ReportSize{Bits: 8},
			hid.ReportCount{Count: 6},
			hid.LogicalMinimum{Min: 0},
			hid.LogicalMaximum{Max: 255},
			hid.UsageMinimum{Min: 0x00},
			hid.UsageMaximum{Max: 0xFF},
			hid.Input{Flags: hid.MainData | hid.MainArray},
			hid.UsagePage{Page: hid.UsagePageLEDs},
			hid.UsageMinimum{Min: 0x01},
			hid.UsageMaximum{Max: 0x05},
			hid.LogicalMinimum{Min: 0},
			hid.LogicalMaximum{Max: 1},
			hid.ReportCount{Count: 5},
			hid.ReportSize{Bits: 1},
			hid.Output{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
			hid.ReportCount{Count: 1},
			hid.ReportSize{Bits: 3},
			hid.Output{Flags: hid.MainConst},
		}},
	},
}

func makeHIDFunction(report hid.ReportDescriptor) *usb.HIDFunction {
	return &usb.HIDFunction{
		Descriptor: usb.HIDDescriptor{
			BcdHID:       0x0111,
			BCountryCode: 0x00,
			Descriptors:  []usb.HIDSubDescriptor{{Type: usb.ReportDescType}},
		},
		ReportDescriptor: report,
	}
}

var defaultDescriptor = usb.Descriptor{
	Device: usb.DeviceDescriptor{
		BcdUSB:             0x0200,
		BDeviceClass:       0x00,
		BDeviceSubClass:    0x00,
		BDeviceProtocol:    0x00,
		BMaxPacketSize0:    0x40,
		IDVendor:           DefaultVID,
		IDProduct:          DefaultPID,
		BcdDevice:          0x0100,
		IManufacturer:      0x01,
		IProduct:           0x02,
		ISerialNumber:      0x03,
		BNumConfigurations: 0x01,
		Speed:              2,
	},
	Configuration: usb.ConfigurationDescriptor{
		BConfigurationValue: 0x01,
		BMAttributes:        0xa0,
		BMaxPower:           250,
	},
	Interfaces: []usb.InterfaceConfig{
		{
			Descriptor: usb.InterfaceDescriptor{
				BInterfaceNumber:   keyboardInterfaceNumber,
				BAlternateSetting:  0x00,
				BNumEndpoints:      0x01,
				BInterfaceClass:    0x03,
				BInterfaceSubClass: 0x01,
				BInterfaceProtocol: 0x01,
				IInterface:         0x03,
			},
			HID: makeHIDFunction(keyboardReportDescriptor),
			Endpoints: []usb.EndpointDescriptor{{
				BEndpointAddress: 0x80 | keyboardEndpointNumber,
				BMAttributes:     0x03,
				WMaxPacketSize:   0x0008,
				BInterval:        0x0a,
			}},
		},
		{
			Descriptor: usb.InterfaceDescriptor{
				BInterfaceNumber:   mouseInterfaceNumber,
				BAlternateSetting:  0x00,
				BNumEndpoints:      0x01,
				BInterfaceClass:    0x03,
				BInterfaceSubClass: 0x00,
				BInterfaceProtocol: 0x02,
				IInterface:         0x04,
			},
			HID: makeHIDFunction(mouseReportDescriptor),
			Endpoints: []usb.EndpointDescriptor{{
				BEndpointAddress: 0x80 | mouseEndpointNumber,
				BMAttributes:     0x03,
				WMaxPacketSize:   0x0004,
				BInterval:        0x06,
			}},
		},
		{
			Descriptor: usb.InterfaceDescriptor{
				BInterfaceNumber:   controllerInterfaceNumber,
				BAlternateSetting:  0x00,
				BNumEndpoints:      0x01,
				BInterfaceClass:    0x03,
				BInterfaceSubClass: 0x00,
				BInterfaceProtocol: 0x00,
				IInterface:         0x05,
			},
			HID: makeHIDFunction(reportDescriptor),
			Endpoints: []usb.EndpointDescriptor{{
				BEndpointAddress: 0x80 | controllerEndpointNumber,
				BMAttributes:     0x03,
				WMaxPacketSize:   0x0040,
				BInterval:        0x06,
			}},
		},
	},
	Strings: map[uint8]string{
		0: "\x04\x09",
		1: "Valve Software",
		2: "Steam Deck Controller",
		3: "Keyboard",
		4: "Mouse",
		5: "Valve",
	},
}
