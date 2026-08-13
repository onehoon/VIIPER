// Package main implements a CGo shared library (c-shared) that exposes
// VIIPER's virtual USB device functionality through a flat C API.
//
// Build:
//
//	GOOS=windows GOARCH=amd64 CGO_ENABLED=1 go build -buildmode=c-shared -o libviiper.dll ./clib/
//	GOOS=linux   GOARCH=amd64 CGO_ENABLED=1 go build -buildmode=c-shared -o libviiper.so  ./clib/
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef void (*viiper_feedback_fn)(uint32_t bus_id, uint32_t device_id, const uint8_t* data, int len, void* user_data);

// Bridge function to call the feedback callback from Go.
static inline void bridge_feedback_fn(viiper_feedback_fn fn, uint32_t bus_id, uint32_t device_id, const uint8_t* data, int len, void* user_data) {
	fn(bus_id, device_id, data, len, user_data);
}
*/
import "C"

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	// Blank-import the registry package to trigger all device init() registrations.
	_ "github.com/Alia5/VIIPER/internal/registry"

	"github.com/Alia5/VIIPER/device"
	"github.com/Alia5/VIIPER/device/dualsense"
	"github.com/Alia5/VIIPER/device/dualshock4"
	"github.com/Alia5/VIIPER/device/ns2pro"
	"github.com/Alia5/VIIPER/device/steamcontroller"
	"github.com/Alia5/VIIPER/device/steamdeck"
	"github.com/Alia5/VIIPER/device/xbox360"
	"github.com/Alia5/VIIPER/internal/server/api"
	usbsrv "github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/Alia5/VIIPER/virtualbus"
)

// ---------------------------------------------------------------------------
// Global state
// ---------------------------------------------------------------------------

var (
	mu        sync.Mutex
	server    *usbsrv.Server
	lastError string

	// devices tracks metadata for each device we've added.
	devices = make(map[deviceKey]*deviceInfo)

	// feedbackCallbacks stores registered C callbacks for devices.
	feedbackCallbacks = make(map[deviceKey]*feedbackReg)

	// attachedDevices tracks which device keys have already been attached to Windows.
	attachedDevices = make(map[deviceKey]bool)
)

type deviceKey struct {
	busID uint32
	devID uint32
}

type deviceInfo struct {
	dev      usb.Device
	typeName string // resolved registry name: xbox360, dualshock4, dualsense, dualsenseedge, steamdeck, steamcontroller, ns2pro
}

// deviceAlias describes a user-friendly device-type name and how it maps
// onto a registry entry, an optional profile string, and optional VID/PID
// overrides. The overrides are only applied when the caller does not pass
// explicit VID/PID values via viiper_device_add_ex.
type deviceAlias struct {
	registryName   string
	profile        string
	vidOverride    *uint16
	pidOverride    *uint16
	deprecationMsg string // logged once-per-process via warnDeprecatedAlias when this alias is used.
}

func u16Ptr(v uint16) *uint16 { return &v }

// Handheld PIDs in Valve's 0x12Fx range used by Steam Input on
// third-party handhelds. The 0x28DE VID is Valve Corporation.
var (
	pidValveVendor   = u16Ptr(0x28DE)
	pidSteamDeck     = u16Ptr(0x1205)
	pidMSIClaw       = u16Ptr(0x12FA)
	pidLenovoLegion2 = u16Ptr(0x12FB)
	pidZotacZone     = u16Ptr(0x12FC)
	pidASUSRogAlly   = u16Ptr(0x12FD)
	pidLenovoLegion  = u16Ptr(0x12FE)
	pidLenovoLegionS = u16Ptr(0x12FF)
)

// deviceTypeAliases maps user-friendly names to their registry type + profile.
// If a name is not in this map, it's used as-is against the device registry.
var deviceTypeAliases = map[string]deviceAlias{
	// ========== Steam Deck & Handheld Platforms ==========
	// Steam Deck and Valve 0x12Fx-range third-party handhelds. All route to
	// the steamdeck device with a VID/PID override per platform.
	"steamdeck":   {registryName: "steamdeck"},
	"steam-deck":  {registryName: "steamdeck"},
	"deck":        {registryName: "steamdeck"},
	"msi-claw":    {registryName: "steamdeck", vidOverride: pidValveVendor, pidOverride: pidMSIClaw},
	"legion-go":   {registryName: "steamdeck", vidOverride: pidValveVendor, pidOverride: pidLenovoLegion},
	"legion-go-2": {registryName: "steamdeck", vidOverride: pidValveVendor, pidOverride: pidLenovoLegion2},
	"legion-go-s": {registryName: "steamdeck", vidOverride: pidValveVendor, pidOverride: pidLenovoLegionS},
	"rog-ally":    {registryName: "steamdeck", vidOverride: pidValveVendor, pidOverride: pidASUSRogAlly},
	"zotac-zone":  {registryName: "steamdeck", vidOverride: pidValveVendor, pidOverride: pidZotacZone},

	// Deprecated steamdeck-family names. Pre-rewrite these routed to the broken
	// device/steamcontroller profile; they now resolve to the real steamdeck.
	"steamdeck-generic": {registryName: "steamdeck", deprecationMsg: "'steamdeck-generic' is deprecated; use 'steamdeck' (or a specific handheld alias like 'legion-go-2')"},
	"steam-generic":     {registryName: "steamdeck", deprecationMsg: "'steam-generic' is deprecated; use 'steamdeck' (or a specific handheld alias like 'legion-go-2')"},

	// ========== Steam Controller V1 (Gordon) ==========
	// Wired Gordon. Canonical name: "gordon".
	"gordon":              {registryName: "steamcontroller"},
	"steam-controller-v1": {registryName: "steamcontroller"},
	"steam-controller":    {registryName: "steamcontroller", deprecationMsg: "'steam-controller' now refers to the wired Steam Controller V1 (Gordon); use 'steamdeck' for the Steam Deck handheld"},
	"steamcontroller-v1":  {registryName: "steamcontroller"},

	// ========== Sony DualSense ==========
	// Sony DualSense (regular, not Edge).
	"dualsense":     {registryName: "dualsense"},
	"ds5":           {registryName: "dualsense"},
	"playstation-5": {registryName: "dualsense"},
	"ps5":           {registryName: "dualsense"},

	// Sony DualSense Edge.
	"dualsenseedge":  {registryName: "dualsenseedge"},
	"ds5-edge":       {registryName: "dualsenseedge"},
	"dualsense-edge": {registryName: "dualsenseedge"},

	// ========== Sony DualShock 4 ==========
	// PlayStation 4 controller.
	"dualshock4":     {registryName: "dualshock4"},
	"ds4":            {registryName: "dualshock4"},
	"ps4":            {registryName: "dualshock4"},
	"ps4-controller": {registryName: "dualshock4"},
	"playstation-4":  {registryName: "dualshock4"},

	// ========== Xbox 360 Controller ==========
	"xbox360":  {registryName: "xbox360"},
	"xbox-360": {registryName: "xbox360"},
	"x360":     {registryName: "xbox360"},
	"360":      {registryName: "xbox360"},

	// ========== Nintendo Switch Pro ==========
	// Official Pro Controller (non-wireless gateway).
	"ns2pro":          {registryName: "ns2pro"},
	"switch-pro":      {registryName: "ns2pro"},
	"ns-pro":          {registryName: "ns2pro"},
	"pro-controller":  {registryName: "ns2pro"},
	"nintendo-switch": {registryName: "ns2pro"},
}

// deprecationOnce ensures each deprecation warning fires at most once per
// process lifetime, regardless of how many times the alias is used.
var deprecationOnce sync.Map // map[string]*sync.Once

func warnDeprecatedAlias(name, msg string) {
	once, _ := deprecationOnce.LoadOrStore(name, &sync.Once{})
	once.(*sync.Once).Do(func() { slog.Warn(msg, "alias", name) })
}

// applyAlias resolves a user-typed device-type name through deviceTypeAliases
// and populates registryName + opts (profile and any VID/PID overrides not
// already set by the caller).
func applyAlias(tn string, opts *device.CreateOptions) string {
	alias, ok := deviceTypeAliases[tn]
	if !ok {
		return tn
	}
	if alias.profile != "" {
		opts.DeviceSpecific = `{"profile":"` + alias.profile + `"}`
	}
	if alias.vidOverride != nil && opts.IDVendor == nil {
		v := *alias.vidOverride
		opts.IDVendor = &v
	}
	if alias.pidOverride != nil && opts.IDProduct == nil {
		p := *alias.pidOverride
		opts.IDProduct = &p
	}
	if alias.deprecationMsg != "" {
		warnDeprecatedAlias(tn, alias.deprecationMsg)
	}
	return alias.registryName
}

func attachDeviceToWindows(bid, did uint32) error {
	key := deviceKey{busID: bid, devID: did}
	if attachedDevices[key] {
		return nil
	}

	port := server.GetListenPort()
	if port == 0 {
		return fmt.Errorf("server listen port not available")
	}

	exportMeta := &usbip.ExportMeta{BusID: bid, DevID: did}
	logger := slog.Default()

	mu.Unlock()
	err := api.AttachLocalhostClient(context.Background(), exportMeta, port, true, logger)
	if err != nil {
		slog.Warn("auto-attach via IOCTL failed, trying usbip.exe", "error", err)
		err = api.AttachLocalhostClient(context.Background(), exportMeta, port, false, logger)
	}
	mu.Lock()

	if err == nil {
		attachedDevices[key] = true
	}

	return err
}

// hiddenDeviceTypes are device types from the registry that should not appear
// in the user-facing device type list (non-controller devices).
var hiddenDeviceTypes = map[string]bool{
	"keyboard": true,
	"mouse":    true,
}

type feedbackReg struct {
	fn       C.viiper_feedback_fn
	userData unsafe.Pointer
}

// ---------------------------------------------------------------------------
// Error helpers
// ---------------------------------------------------------------------------

func setError(err error) C.int {
	if err != nil {
		lastError = err.Error()
		return -1
	}
	lastError = ""
	return 0
}

// viiper_free_string frees a C string previously returned by viiper_last_error
// or viiper_list_device_types. The caller must call this to avoid memory leaks.
//
//export viiper_free_string
func viiper_free_string(s *C.char) {
	if s != nil {
		C.free(unsafe.Pointer(s))
	}
}

//export viiper_last_error
func viiper_last_error() *C.char {
	mu.Lock()
	defer mu.Unlock()
	if lastError == "" {
		return nil
	}
	return C.CString(lastError)
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

//export viiper_init
func viiper_init(listenAddr *C.char) C.int {
	mu.Lock()
	defer mu.Unlock()

	if server != nil {
		return setError(fmt.Errorf("already initialized"))
	}

	addr := C.GoString(listenAddr)
	if addr == "" {
		addr = "0.0.0.0:3241"
	}

	// Set up file-based logging next to the DLL for protocol debugging.
	if exe, err := os.Executable(); err == nil {
		logPath := filepath.Join(filepath.Dir(exe), "viiper_go_debug.log")
		if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644); err == nil {
			handler := slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo})
			slog.SetDefault(slog.New(handler))
		}
	}
	logger := slog.Default()

	cfg := usbsrv.ServerConfig{
		Addr:                    addr,
		ConnectionTimeout:       30 * time.Second,
		BusCleanupTimeout:       5 * time.Second,
		WriteBatchFlushInterval: 1 * time.Millisecond,
	}

	server = usbsrv.New(cfg, logger, nil)

	go func() {
		if err := server.ListenAndServe(); err != nil {
			slog.Error("USBIP server error", "error", err)
		}
	}()

	// Wait for the server to be ready (listener bound).
	<-server.Ready()

	return 0
}

//export viiper_shutdown
func viiper_shutdown() {
	mu.Lock()
	defer mu.Unlock()

	if server == nil {
		return
	}

	// Remove all buses (which removes all devices).
	for _, busID := range server.ListBuses() {
		_ = server.RemoveBus(busID)
	}

	_ = server.Close()
	server = nil

	// Clear device tracking.
	devices = make(map[deviceKey]*deviceInfo)
	attachedDevices = make(map[deviceKey]bool)
	feedbackCallbacks = make(map[deviceKey]*feedbackReg)
}

// ---------------------------------------------------------------------------
// Bus management
// ---------------------------------------------------------------------------

//export viiper_bus_create
func viiper_bus_create(busID C.uint32_t) C.int {
	mu.Lock()
	defer mu.Unlock()

	if server == nil {
		return setError(fmt.Errorf("not initialized"))
	}

	bus, err := virtualbus.NewWithBusID(uint32(busID))
	if err != nil {
		return setError(err)
	}

	if err := server.AddBus(bus); err != nil {
		_ = bus.Close()
		return setError(err)
	}

	return 0
}

//export viiper_bus_remove
func viiper_bus_remove(busID C.uint32_t) C.int {
	mu.Lock()
	defer mu.Unlock()

	if server == nil {
		return setError(fmt.Errorf("not initialized"))
	}

	bid := uint32(busID)

	// Clean up device and feedback tracking for this bus.
	for k := range devices {
		if k.busID == bid {
			delete(devices, k)
			delete(feedbackCallbacks, k)
			delete(attachedDevices, k)
		}
	}

	if err := server.RemoveBus(bid); err != nil {
		return setError(err)
	}

	return 0
}

// ---------------------------------------------------------------------------
// Device management
// ---------------------------------------------------------------------------

//export viiper_device_add
func viiper_device_add(busID C.uint32_t, typeName *C.char, outDeviceID *C.uint32_t) C.int {
	mu.Lock()
	defer mu.Unlock()

	if server == nil {
		return setError(fmt.Errorf("not initialized"))
	}

	bid := uint32(busID)
	tn := strings.ToLower(C.GoString(typeName))

	// Resolve aliases — populates profile, VID/PID overrides, prints any
	// deprecation warning once-per-process.
	var opts device.CreateOptions
	registryName := applyAlias(tn, &opts)

	reg := api.GetRegistration(registryName)
	if reg == nil {
		return setError(fmt.Errorf("unknown device type: %s", tn))
	}

	dev, err := reg.CreateDevice(&opts)
	if err != nil {
		return setError(fmt.Errorf("create device: %w", err))
	}

	bus := server.GetBus(bid)
	if bus == nil {
		return setError(fmt.Errorf("bus %d not found", bid))
	}

	_, err = bus.Add(dev)
	if err != nil {
		return setError(fmt.Errorf("add device to bus: %w", err))
	}

	// Find the device ID that was assigned.
	metas := bus.GetAllDeviceMetas()
	var devID uint32
	for _, m := range metas {
		if m.Dev == dev {
			devID = m.Meta.DevID
			break
		}
	}

	// Track the device using the registry name for input dispatch.
	key := deviceKey{busID: bid, devID: devID}
	devices[key] = &deviceInfo{
		dev:      dev,
		typeName: registryName,
	}

	if outDeviceID != nil {
		*outDeviceID = C.uint32_t(devID)
	}

	// Auto-attach the device via usbip-win2 so it appears in Windows.
	// Done synchronously so the device is fully attached before the caller
	// starts sending input.
	if err := attachDeviceToWindows(bid, devID); err != nil {
		slog.Error("auto-attach failed", "error", err)
		// Device was added but attach failed — not a fatal error.
		// The caller can retry with viiper_device_attach.
	}

	return 0
}

// viiper_device_add_ex is like viiper_device_add but allows overriding the USB VID and PID.
// Pass 0 for vid or pid to use the default for the device type/profile.
//
//export viiper_device_add_ex
func viiper_device_add_ex(busID C.uint32_t, typeName *C.char, vid C.uint16_t, pid C.uint16_t, outDeviceID *C.uint32_t) C.int {
	mu.Lock()
	defer mu.Unlock()

	if server == nil {
		return setError(fmt.Errorf("not initialized"))
	}

	bid := uint32(busID)
	tn := strings.ToLower(C.GoString(typeName))

	// Explicit _ex arguments win over alias VID/PID overrides — apply them
	// first so applyAlias's "only-set-if-unset" rule preserves them.
	var opts device.CreateOptions
	if vid != 0 {
		v := uint16(vid)
		opts.IDVendor = &v
	}
	if pid != 0 {
		p := uint16(pid)
		opts.IDProduct = &p
	}
	registryName := applyAlias(tn, &opts)

	reg := api.GetRegistration(registryName)
	if reg == nil {
		return setError(fmt.Errorf("unknown device type: %s", tn))
	}

	dev, err := reg.CreateDevice(&opts)
	if err != nil {
		return setError(fmt.Errorf("create device: %w", err))
	}

	bus := server.GetBus(bid)
	if bus == nil {
		return setError(fmt.Errorf("bus %d not found", bid))
	}

	_, err = bus.Add(dev)
	if err != nil {
		return setError(fmt.Errorf("add device to bus: %w", err))
	}

	metas := bus.GetAllDeviceMetas()
	var devID uint32
	for _, m := range metas {
		if m.Dev == dev {
			devID = m.Meta.DevID
			break
		}
	}

	key := deviceKey{busID: bid, devID: devID}
	devices[key] = &deviceInfo{
		dev:      dev,
		typeName: registryName,
	}

	if outDeviceID != nil {
		*outDeviceID = C.uint32_t(devID)
	}

	if err := attachDeviceToWindows(bid, devID); err != nil {
		slog.Error("auto-attach failed", "error", err)
	}

	return 0
}

// viiper_device_attach explicitly attaches a device to Windows via the usbip-win2 client.
// This is called automatically by viiper_device_add, but can be called separately if needed.
//
//export viiper_device_attach
func viiper_device_attach(busID C.uint32_t, deviceID C.uint32_t) C.int {
	mu.Lock()
	defer mu.Unlock()

	if server == nil {
		return setError(fmt.Errorf("not initialized"))
	}

	bid := uint32(busID)
	did := uint32(deviceID)

	err := attachDeviceToWindows(bid, did)
	if err != nil {
		return setError(fmt.Errorf("attach device: %w", err))
	}

	return 0
}

//export viiper_device_remove
func viiper_device_remove(busID C.uint32_t, deviceID C.uint32_t) C.int {
	mu.Lock()
	defer mu.Unlock()

	if server == nil {
		return setError(fmt.Errorf("not initialized"))
	}

	bid := uint32(busID)
	did := uint32(deviceID)

	key := deviceKey{busID: bid, devID: did}
	delete(devices, key)
	delete(feedbackCallbacks, key)
	delete(attachedDevices, key)

	didStr := fmt.Sprintf("%d", did)
	if err := server.RemoveDeviceByID(bid, didStr); err != nil {
		return setError(err)
	}

	return 0
}

//export viiper_list_device_types
func viiper_list_device_types() *C.char {
	types := api.ListDeviceTypes()

	// Filter out non-controller devices and add user-friendly aliases.
	var filtered []string
	seen := make(map[string]bool, len(types))
	for _, t := range types {
		if hiddenDeviceTypes[t] {
			continue
		}
		filtered = append(filtered, t)
		seen[t] = true
	}
	for alias := range deviceTypeAliases {
		if !seen[alias] {
			filtered = append(filtered, alias)
			seen[alias] = true
		}
	}

	data, err := json.Marshal(filtered)
	if err != nil {
		return C.CString("[]")
	}
	return C.CString(string(data))
}

// ---------------------------------------------------------------------------
// Input state
// ---------------------------------------------------------------------------

//export viiper_device_set_input
func viiper_device_set_input(busID C.uint32_t, deviceID C.uint32_t, data *C.uint8_t, length C.int) C.int {
	mu.Lock()
	defer mu.Unlock()

	if server == nil {
		return setError(fmt.Errorf("not initialized"))
	}

	key := deviceKey{busID: uint32(busID), devID: uint32(deviceID)}
	info, ok := devices[key]
	if !ok {
		return setError(fmt.Errorf("device %d-%d not found", busID, deviceID))
	}

	buf := C.GoBytes(unsafe.Pointer(data), length)

	switch info.typeName {
	case "xbox360":
		xdev, ok := info.dev.(*xbox360.Xbox360)
		if !ok {
			return setError(fmt.Errorf("device type mismatch"))
		}
		var state xbox360.InputState
		if err := state.UnmarshalBinary(buf); err != nil {
			return setError(err)
		}
		xdev.UpdateInputState(state)

	case "dualshock4":
		ds4, ok := info.dev.(*dualshock4.DualShock4)
		if !ok {
			return setError(fmt.Errorf("device type mismatch"))
		}
		var state dualshock4.InputState
		if err := state.UnmarshalBinary(buf); err != nil {
			return setError(err)
		}
		ds4.UpdateInputState(&state)

	case "dualsense", "dualsenseedge":
		ds, ok := info.dev.(*dualsense.DualSense)
		if !ok {
			return setError(fmt.Errorf("device type mismatch"))
		}
		var state dualsense.InputState
		if err := state.UnmarshalBinary(buf); err != nil {
			return setError(err)
		}
		ds.UpdateInputState(&state)

	case "steamdeck":
		sd, ok := info.dev.(*steamdeck.SteamDeck)
		if !ok {
			return setError(fmt.Errorf("device type mismatch"))
		}
		var state steamdeck.InputState
		if err := state.UnmarshalBinary(buf); err != nil {
			return setError(err)
		}
		sd.UpdateInputState(&state)

	case "steamcontroller":
		sc, ok := info.dev.(*steamcontroller.SteamController)
		if !ok {
			return setError(fmt.Errorf("device type mismatch"))
		}
		// Gordon uses its native 64-byte input format.
		var state steamcontroller.InputState
		if err := state.UnmarshalBinary(buf); err != nil {
			return setError(err)
		}
		sc.UpdateInputState(&state)

	case "ns2pro":
		// Switch 2 Pro Controller: 27-byte wire (Buttons:u32, LX/LY/RX/RY:u16
		// 12-bit native, Accel/Gyro:i16, batteryLevel:u8, charging:bool,
		// externalPower:bool). UpdateInputState takes the InputState by value
		// (not pointer) to match the port's API.
		ns2, ok := info.dev.(*ns2pro.NS2Pro)
		if !ok {
			return setError(fmt.Errorf("device type mismatch"))
		}
		var state ns2pro.InputState
		if err := state.UnmarshalBinary(buf); err != nil {
			return setError(err)
		}
		ns2.UpdateInputState(state)

	default:
		return setError(fmt.Errorf("input not supported for device type: %s", info.typeName))
	}

	return 0
}

// ---------------------------------------------------------------------------
// Feedback callbacks
// ---------------------------------------------------------------------------

//export viiper_device_set_feedback_callback
func viiper_device_set_feedback_callback(busID C.uint32_t, deviceID C.uint32_t, cb C.viiper_feedback_fn, userData unsafe.Pointer) C.int {
	mu.Lock()
	defer mu.Unlock()

	if server == nil {
		return setError(fmt.Errorf("not initialized"))
	}

	key := deviceKey{busID: uint32(busID), devID: uint32(deviceID)}
	info, ok := devices[key]
	if !ok {
		return setError(fmt.Errorf("device %d-%d not found", busID, deviceID))
	}

	reg := &feedbackReg{fn: cb, userData: userData}
	feedbackCallbacks[key] = reg

	bid := uint32(busID)
	did := uint32(deviceID)

	switch info.typeName {
	case "xbox360":
		xdev, ok := info.dev.(*xbox360.Xbox360)
		if !ok {
			return setError(fmt.Errorf("device type mismatch"))
		}
		xdev.SetRumbleCallback(func(rumble xbox360.XRumbleState) {
			data, err := rumble.MarshalBinary()
			if err != nil {
				return
			}
			invokeFeedbackCallback(bid, did, data)
		})

	case "dualshock4":
		ds4, ok := info.dev.(*dualshock4.DualShock4)
		if !ok {
			return setError(fmt.Errorf("device type mismatch"))
		}
		ds4.SetOutputCallback(func(output dualshock4.OutputState) {
			data, err := output.MarshalBinary()
			if err != nil {
				return
			}
			invokeFeedbackCallback(bid, did, data)
		})

	case "dualsense", "dualsenseedge":
		ds, ok := info.dev.(*dualsense.DualSense)
		if !ok {
			return setError(fmt.Errorf("device type mismatch"))
		}
		ds.SetOutputCallback(func(output dualsense.OutputState) {
			data, err := output.MarshalBinary()
			if err != nil {
				return
			}
			invokeFeedbackCallback(bid, did, data)
		})

	case "steamdeck":
		sd, ok := info.dev.(*steamdeck.SteamDeck)
		if !ok {
			return setError(fmt.Errorf("device type mismatch"))
		}
		sd.SetOutputCallback(func(output steamdeck.OutputState) {
			data, err := output.MarshalBinary()
			if err != nil {
				return
			}
			invokeFeedbackCallback(bid, did, data)
		})

	case "steamcontroller":
		sc, ok := info.dev.(*steamcontroller.SteamController)
		if !ok {
			return setError(fmt.Errorf("device type mismatch"))
		}
		sc.SetOutputCallback(func(output steamcontroller.OutputState) {
			data, err := output.MarshalBinary()
			if err != nil {
				return
			}
			invokeFeedbackCallback(bid, did, data)
		})

	case "ns2pro":
		ns2, ok := info.dev.(*ns2pro.NS2Pro)
		if !ok {
			return setError(fmt.Errorf("device type mismatch"))
		}
		ns2.SetOutputCallback(func(output ns2pro.OutputState) {
			data, err := output.MarshalBinary()
			if err != nil {
				return
			}
			invokeFeedbackCallback(bid, did, data)
		})

	default:
		return setError(fmt.Errorf("feedback not supported for device type: %s", info.typeName))
	}

	return 0
}

// invokeFeedbackCallback calls the registered C callback for a device.
// Must NOT hold mu when calling this (the callback may re-enter).
func invokeFeedbackCallback(busID, deviceID uint32, data []byte) {
	mu.Lock()
	key := deviceKey{busID: busID, devID: deviceID}
	reg, ok := feedbackCallbacks[key]
	mu.Unlock()

	if !ok || reg.fn == nil {
		return
	}

	cData := C.CBytes(data)
	defer C.free(cData)

	C.bridge_feedback_fn(reg.fn, C.uint32_t(busID), C.uint32_t(deviceID),
		(*C.uint8_t)(cData), C.int(len(data)), reg.userData)
}

func main() {}
