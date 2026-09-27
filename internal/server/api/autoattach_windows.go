//go:build windows

package api

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/Alia5/VIIPER/usbip"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	setupapi                             = windows.NewLazySystemDLL("setupapi.dll")
	procSetupDiGetClassDevsW             = setupapi.NewProc("SetupDiGetClassDevsW")
	procSetupDiEnumDeviceInterfaces      = setupapi.NewProc("SetupDiEnumDeviceInterfaces")
	procSetupDiGetDeviceInterfaceDetailW = setupapi.NewProc("SetupDiGetDeviceInterfaceDetailW")
	procSetupDiDestroyDeviceInfoList     = setupapi.NewProc("SetupDiDestroyDeviceInfoList")
)

const (
	DigcfPresent         = 0x00000002
	DigcfDeviceInterface = 0x00000010
)

type SpDeviceInterfaceData struct {
	CbSize             uint32
	InterfaceClassGUID windows.GUID
	Flags              uint32
	Reserved           uintptr
}

type SpDeviceInterfaceDetailData struct {
	CbSize     uint32
	DevicePath [1]uint16
}

// Device GUID from usbip-win2 driver
var deviceGUID = windows.GUID{
	Data1: 0xB4030C06,
	Data2: 0xDC5F,
	Data3: 0x4FCC,
	Data4: [8]byte{0x87, 0xEB, 0xE5, 0x51, 0x5A, 0x09, 0x35, 0xC0},
}

const (
	niMaxHost     = 1025
	niMaxServ     = 32
	serialBufSize = 16
)

// attachIOCTL0980 mirrors usbip-win2 v0.9.8.0's MSVC plugin_hardware ABI.
// The explicit tail padding preserves the imported_device_location base size.
type attachIOCTL0980 struct {
	Size                          uint32
	PortOutput                    int32
	BusID                         [32]byte
	Service                       [niMaxServ]byte
	Host                          [niMaxHost]byte
	ImportedDeviceLocationPadding [3]byte // MSVC imported_device_location base-subobject tail padding
	Serial                        [serialBufSize]byte
	WskEvents                     bool
}

// attachIOCTL0981 mirrors v0.9.8.1, which inserts LocationHash into the
// imported_device_location base. The driver computes LocationHash on output;
// VIIPER keeps it zero on input and never uses it for ownership.
type attachIOCTL0981 struct {
	Size                          uint32
	PortOutput                    int32
	LocationHash                  uint32
	BusID                         [32]byte
	Service                       [niMaxServ]byte
	Host                          [niMaxHost]byte
	ImportedDeviceLocationPadding [3]byte
	Serial                        [serialBufSize]byte
	WskEvents                     bool
}

type usbipWin2NativeABI uint8

const (
	usbipWin2NativeABIUnknown usbipWin2NativeABI = iota
	usbipWin2NativeABI0980
	usbipWin2NativeABI0981
)

func selectUSBIPWin2NativeABI(version string) usbipWin2NativeABI {
	switch strings.TrimSpace(version) {
	case "0.9.8.0":
		return usbipWin2NativeABI0980
	case "0.9.8.1":
		return usbipWin2NativeABI0981
	default:
		return usbipWin2NativeABIUnknown
	}
}

func (abi usbipWin2NativeABI) String() string {
	switch abi {
	case usbipWin2NativeABI0980:
		return "0.9.8.0"
	case usbipWin2NativeABI0981:
		return "0.9.8.1"
	default:
		return "unsupported"
	}
}

func (abi usbipWin2NativeABI) inputLength() uint32 {
	switch abi {
	case usbipWin2NativeABI0980:
		return uint32(unsafe.Sizeof(attachIOCTL0980{}))
	case usbipWin2NativeABI0981:
		return uint32(unsafe.Sizeof(attachIOCTL0981{}))
	default:
		return 0
	}
}

const (
	usbipWin2UninstallSubKey = `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\{199505b0-b93d-4521-a8c7-897818e0205a}_is1`
	usbipWin2DisplayVersion  = "DisplayVersion"
	attachPortOutputLength   = uint32(8)
)

// plugoutIOCTL is the common 8-byte plugout_hardware ABI in both supported releases.
type plugoutIOCTL struct {
	Size uint32
	Port int32
}

const (
	fileDeviceUnknown    = 0x00000022
	methodBuffered       = 0
	fileReadData         = 0x0001
	fileWriteData        = 0x0002
	ioctlPluginHardware  = (fileDeviceUnknown << 16) | ((fileReadData | fileWriteData) << 14) | (0x800 << 2) | methodBuffered
	ioctlPlugoutHardware = (fileDeviceUnknown << 16) | ((fileReadData | fileWriteData) << 14) | (0x801 << 2) | methodBuffered
)

func attachLocalhostClientImpl(ctx context.Context, deviceExportMeta *usbip.ExportMeta, usbipServerPort uint16, useNativeIOCTL bool, logger *slog.Logger) error {
	// Keep the legacy auto-attach contract isolated from tracked ownership.
	// PR3B will migrate callers that can retain attachment-outcome-unknown.
	if useNativeIOCTL {
		if _, err := attachViaIOCTL(ctx, deviceExportMeta, usbipServerPort, logger); err == nil {
			return nil
		} else {
			slog.Error("Native IOCTL auto-attach failed, falling back to command execution", "error", err)
			slog.Info("Trying fallback via usbip executable")
		}
	}
	return attachViaCommandLegacy(ctx, deviceExportMeta, usbipServerPort, logger)
}

func attachLocalhostClientTrackedImpl(ctx context.Context, deviceExportMeta *usbip.ExportMeta, usbipServerPort uint16, useNativeIOCTL bool, logger *slog.Logger) (LocalhostAttachment, error) {
	return attachLocalhostClientWithFallback(ctx, deviceExportMeta, usbipServerPort, useNativeIOCTL, logger, attachViaIOCTL, attachViaCommand)
}

// nativeAttachOps is the fake seam for attachViaIOCTLWithOps. Production code always uses
// realNativeAttachOps(); tests inject fakes so they never touch the real usbip-win2 driver,
// SetupAPI, registry, or DeviceIoControl. pluginHardware receives the selected ABI request as
// the shared input/output buffer, matching the real METHOD_BUFFERED call.
type nativeAttachOps struct {
	readInstalledVersion func() (string, error)
	discoverDevicePath   func() (string, error)
	openDevice           func(devicePath string) (windows.Handle, error)
	closeDevice          func(handle windows.Handle) error
	pluginHardware       func(handle windows.Handle, request unsafe.Pointer, inputLength, outputLength uint32) (bytesReturned uint32, err error)
}

// nativeDetachOps is the fake seam for detachViaIOCTLWithOps, mirroring nativeAttachOps.
type nativeDetachOps struct {
	discoverDevicePath func() (string, error)
	openDevice         func(devicePath string) (windows.Handle, error)
	closeDevice        func(handle windows.Handle) error
	plugoutHardware    func(handle windows.Handle, data *plugoutIOCTL) (bytesReturned uint32, err error)
}

func realNativeAttachOps() nativeAttachOps {
	return nativeAttachOps{
		readInstalledVersion: readInstalledUSBIPWin2Version,
		discoverDevicePath:   func() (string, error) { return getDeviceInterfacePath(&deviceGUID) },
		openDevice:           openUSBIPWin2Device,
		closeDevice:          func(handle windows.Handle) error { return windows.CloseHandle(handle) },
		pluginHardware: func(handle windows.Handle, request unsafe.Pointer, inputLength, outputLength uint32) (uint32, error) {
			var bytesReturned uint32
			err := windows.DeviceIoControl(
				handle,
				ioctlPluginHardware,
				(*byte)(request),
				inputLength,
				(*byte)(request),
				outputLength,
				&bytesReturned,
				nil,
			)
			return bytesReturned, err
		},
	}
}

func readInstalledUSBIPWin2Version() (string, error) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, usbipWin2UninstallSubKey, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return "", fmt.Errorf("open usbip-win2 uninstall registry key: %w", err)
	}
	defer key.Close()

	version, _, err := key.GetStringValue(usbipWin2DisplayVersion)
	if err != nil {
		return "", fmt.Errorf("read usbip-win2 DisplayVersion: %w", err)
	}
	return version, nil
}

func realNativeDetachOps() nativeDetachOps {
	return nativeDetachOps{
		discoverDevicePath: func() (string, error) { return getDeviceInterfacePath(&deviceGUID) },
		openDevice:         openUSBIPWin2Device,
		closeDevice:        func(handle windows.Handle) error { return windows.CloseHandle(handle) },
		plugoutHardware: func(handle windows.Handle, data *plugoutIOCTL) (uint32, error) {
			var bytesReturned uint32
			err := windows.DeviceIoControl(handle, ioctlPlugoutHardware, (*byte)(unsafe.Pointer(data)), uint32(unsafe.Sizeof(*data)), nil, 0, &bytesReturned, nil)
			return bytesReturned, err
		},
	}
}

func openUSBIPWin2Device(devicePath string) (windows.Handle, error) {
	devicePathUTF16, err := windows.UTF16PtrFromString(devicePath)
	if err != nil {
		return 0, fmt.Errorf("open: failed to convert device path: %w", err)
	}
	handle, err := windows.CreateFile(
		devicePathUTF16,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return 0, fmt.Errorf("open: failed to open usbip-win2 device: %w", err)
	}
	return handle, nil
}

// commandRunner is the fake seam for attachViaCommandWithRunner/detachViaCommandWithRunner.
// Production code always uses realCommandRunner(); tests inject a fake so they never execute a
// real usbip.exe process.
type commandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

func realCommandRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

type nativeAttachABIEvidence struct {
	installedVersion string
	selectedABI      string
	inputLength      uint32
}

// logNativeIOCTLTiming emits one behavior-neutral "attachment-timing" summary (layer=native-ioctl)
// per actual native attach/detach attempt. discoveryUs/openUs/ioctlUs/validationUs are 0 for any
// stage never reached; reachedIOCTL distinguishes a request that never got past discovery/open
// (backendCalled=false) from one where DeviceIoControl actually ran. This is diagnostic-only and
// runs after err is already finalized by the caller; it never changes err or classification.
func logNativeIOCTLTiming(logger *slog.Logger, operation string, err error, reachedIOCTL bool, total time.Duration, discoveryUs, openUs, ioctlUs, validationUs int64, attachABI *nativeAttachABIEvidence) {
	attrs := []any{
		"operation", operation,
		"layer", "native-ioctl",
		"result", attachmentTimingResultLabel(err),
		"backend", "native-ioctl",
		"backendCalled", reachedIOCTL,
		"totalUs", total.Microseconds(),
		"discoveryUs", discoveryUs,
		"openUs", openUs,
		"ioctlUs", ioctlUs,
		"validationUs", validationUs,
	}
	if attachABI != nil {
		attrs = append(attrs,
			"installedVersion", attachABI.installedVersion,
			"selectedABI", attachABI.selectedABI,
			"inputLength", uint64(attachABI.inputLength),
		)
	}
	logger.Info("attachment-timing", attrs...)
}

// logCommandBackendTiming emits one behavior-neutral "attachment-timing" summary
// (layer=command) per actual command-backend attempt.
func logCommandBackendTiming(logger *slog.Logger, operation string, err error, backendCalled bool, total time.Duration, processUs, classificationUs int64) {
	logger.Info("attachment-timing",
		"operation", operation,
		"layer", "command",
		"result", attachmentTimingResultLabel(err),
		"backend", "command",
		"backendCalled", backendCalled,
		"totalUs", total.Microseconds(),
		"processUs", processUs,
		"classificationUs", classificationUs,
	)
}

func attachViaIOCTL(_ context.Context, deviceExportMeta *usbip.ExportMeta, usbipServerPort uint16, logger *slog.Logger) (LocalhostAttachment, error) {
	return attachViaIOCTLWithOps(deviceExportMeta, usbipServerPort, logger, realNativeAttachOps())
}

func attachViaIOCTLWithOps(deviceExportMeta *usbip.ExportMeta, usbipServerPort uint16, logger *slog.Logger, ops nativeAttachOps) (result LocalhostAttachment, err error) {
	nativeStart := time.Now()
	var discoveryUs, openUs, ioctlUs, validationUs int64
	var reachedIOCTL bool
	attachABI := nativeAttachABIEvidence{selectedABI: "unsupported"}
	defer func() {
		logNativeIOCTLTiming(logger, "attach", err, reachedIOCTL, time.Since(nativeStart), discoveryUs, openUs, ioctlUs, validationUs, &attachABI)
	}()

	if deviceExportMeta == nil {
		err = fmt.Errorf("argumentValidation: missing device export metadata")
		return
	}

	if usbipServerPort == 0 {
		err = fmt.Errorf("argumentValidation: invalid TCP port number (0)")
		return
	}

	busID := fmt.Sprintf("%d-%d", deviceExportMeta.BusID, deviceExportMeta.DevID)
	if len(busID) >= len(attachIOCTL0980{}.BusID) {
		err = fmt.Errorf("argumentValidation: bus ID too long: %s", busID)
		return
	}

	service := fmt.Sprintf("%d", usbipServerPort)
	if len(service) >= len(attachIOCTL0980{}.Service) {
		err = fmt.Errorf("argumentValidation: service string too long: %s", service)
		return
	}

	logger.Info("Auto-attaching localhost client via native IOCTL",
		"busID", deviceExportMeta.BusID,
		"deviceID", deviceExportMeta.DevID)
	if ops.readInstalledVersion == nil {
		err = fmt.Errorf("version: usbip-win2 package version reader is unavailable")
		return
	}
	installedVersion, versionErr := ops.readInstalledVersion()
	if versionErr != nil {
		err = fmt.Errorf("version: failed to read usbip-win2 DisplayVersion: %w", versionErr)
		return
	}
	installedVersion = strings.TrimSpace(installedVersion)
	attachABI.installedVersion = installedVersion
	selectedABI := selectUSBIPWin2NativeABI(installedVersion)
	attachABI.selectedABI = selectedABI.String()
	if selectedABI == usbipWin2NativeABIUnknown {
		err = fmt.Errorf("version: unsupported usbip-win2 DisplayVersion %q", installedVersion)
		return
	}
	attachABI.inputLength = selectedABI.inputLength()
	var request0980 attachIOCTL0980
	var request0981 attachIOCTL0981
	var request unsafe.Pointer
	var portOutput *int32
	switch selectedABI {
	case usbipWin2NativeABI0980:
		request0980.Size = uint32(unsafe.Sizeof(request0980))
		copy(request0980.BusID[:], busID)
		copy(request0980.Service[:], service)
		copy(request0980.Host[:], "127.0.0.1")
		request = unsafe.Pointer(&request0980)
		portOutput = &request0980.PortOutput
	case usbipWin2NativeABI0981:
		request0981.Size = uint32(unsafe.Sizeof(request0981))
		copy(request0981.BusID[:], busID)
		copy(request0981.Service[:], service)
		copy(request0981.Host[:], "127.0.0.1")
		request = unsafe.Pointer(&request0981)
		portOutput = &request0981.PortOutput
	default:
		// The selected ABI was validated above; this is an internal invariant.
		err = fmt.Errorf("version: no supported usbip-win2 ABI selected")
		return
	}

	discoveryStart := time.Now()
	devicePath, discoveryErr := ops.discoverDevicePath()
	discoveryUs = time.Since(discoveryStart).Microseconds()
	if discoveryErr != nil {
		err = fmt.Errorf("discovery: %w", discoveryErr)
		return
	}
	logger.Debug("Found usbip-win2 device", "path", devicePath)

	openStart := time.Now()
	handle, openErr := ops.openDevice(devicePath)
	openUs = time.Since(openStart).Microseconds()
	if openErr != nil {
		err = openErr
		return
	}
	defer ops.closeDevice(handle) // nolint

	logger.Debug("Opened device handle")

	ioctlStart := time.Now()
	bytesReturned, ioctlErr := ops.pluginHardware(handle, request, attachABI.inputLength, attachPortOutputLength)
	ioctlUs = time.Since(ioctlStart).Microseconds()
	reachedIOCTL = true
	if ioctlErr != nil {
		logger.Error("native PLUGIN_HARDWARE DeviceIoControl failed",
			"error", ioctlErr,
			"inputLength", uint64(attachABI.inputLength),
			"outputLength", attachPortOutputLength)
		err = fmt.Errorf("%w: native PLUGIN_HARDWARE DeviceIoControl failed: %v", ErrAttachmentOutcomeUnknown, ioctlErr)
		return
	}

	logger.Debug("IOCTL completed", "bytesReturned", bytesReturned, "portOutput", *portOutput)

	validationStart := time.Now()
	validationErr := validateNativeAttachResponse(bytesReturned, *portOutput)
	validationUs = time.Since(validationStart).Microseconds()
	if validationErr != nil {
		err = validationErr
		return
	}

	logger.Info("Successfully attached device via IOCTL",
		"busID", deviceExportMeta.BusID,
		"deviceID", deviceExportMeta.DevID,
		"usbPort", *portOutput)

	result = LocalhostAttachment{Backend: LocalhostAttachmentBackendNativeIOCTL, Port: *portOutput}
	return
}

func attachViaCommand(ctx context.Context, deviceExportMeta *usbip.ExportMeta, usbipServerPort uint16, logger *slog.Logger) (LocalhostAttachment, error) {
	return attachViaCommandWithRunner(ctx, deviceExportMeta, usbipServerPort, logger, realCommandRunner)
}

func attachViaCommandWithRunner(ctx context.Context, deviceExportMeta *usbip.ExportMeta, usbipServerPort uint16, logger *slog.Logger, run commandRunner) (result LocalhostAttachment, err error) {
	commandStart := time.Now()
	var processUs, classificationUs int64
	defer func() {
		logCommandBackendTiming(logger, "attach", err, true, time.Since(commandStart), processUs, classificationUs)
	}()

	logger.Info("Auto-attaching localhost client", "busID", deviceExportMeta.BusID, "deviceID", deviceExportMeta.DevID)

	args := usbipAttachCommandArgs(usbipServerPort, fmt.Sprintf("%d-%d", deviceExportMeta.BusID, deviceExportMeta.DevID))
	processStart := time.Now()
	output, cmdErr := run(ctx, "usbip", args...)
	processUs = time.Since(processStart).Microseconds()
	if cmdErr != nil {
		logger.Error("Failed to attach device",
			"error", cmdErr,
			"port", usbipServerPort,
			"output", string(output))
		classifyStart := time.Now()
		port, resultErr := classifyUSBIPAttachCommandResult(output, cmdErr)
		classificationUs = time.Since(classifyStart).Microseconds()
		if resultErr != nil {
			err = resultErr
			return
		}
		result = LocalhostAttachment{Backend: LocalhostAttachmentBackendCommand, Port: port}
		return
	}
	logger.Debug("usbip attach output", "output", string(output))
	classifyStart := time.Now()
	port, classifyErr := classifyUSBIPAttachCommandResult(output, nil)
	classificationUs = time.Since(classifyStart).Microseconds()
	if classifyErr != nil {
		err = classifyErr
		return
	}
	result = LocalhostAttachment{Backend: LocalhostAttachmentBackendCommand, Port: port}
	return
}

// attachViaCommandLegacy preserves the old error-only auto-attach behavior.
// It intentionally does not participate in tracked attachment ownership; that
// migration must happen atomically with PR3B's device lifecycle state.
func attachViaCommandLegacy(ctx context.Context, deviceExportMeta *usbip.ExportMeta, usbipServerPort uint16, logger *slog.Logger) error {
	logger.Info("Auto-attaching localhost client", "busID", deviceExportMeta.BusID, "deviceID", deviceExportMeta.DevID)
	cmd := exec.CommandContext(ctx, "usbip", usbipLegacyAttachCommandArgs(usbipServerPort, fmt.Sprintf("%d-%d", deviceExportMeta.BusID, deviceExportMeta.DevID))...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		logger.Error("Failed to attach device", "error", err, "port", usbipServerPort, "output", string(output))
		return err
	}
	logger.Debug("usbip attach output", "output", string(output))
	return nil
}

func usbipLegacyAttachCommandArgs(usbipServerPort uint16, busID string) []string {
	return []string{
		"--tcp-port", strconv.FormatUint(uint64(usbipServerPort), 10),
		"attach", "-r", "127.0.0.1", "-b", busID,
		"--receive-mode=zero-copy",
	}
}

func validateNativeAttachResponse(bytesReturned uint32, port int32) error {
	if bytesReturned != attachPortOutputLength {
		return fmt.Errorf("%w: native PLUGIN_HARDWARE returned %d bytes, expected %d", ErrAttachmentOutcomeUnknown, bytesReturned, attachPortOutputLength)
	}
	if port <= 0 {
		return fmt.Errorf("%w: native PLUGIN_HARDWARE returned invalid USB port %d", ErrAttachmentOutcomeUnknown, port)
	}
	return nil
}

func detachLocalhostClientImpl(ctx context.Context, attachment LocalhostAttachment, logger *slog.Logger) error {
	if attachment.Port <= 0 {
		return fmt.Errorf("invalid USB/IP import port %d", attachment.Port)
	}
	switch attachment.Backend {
	case LocalhostAttachmentBackendNativeIOCTL:
		return detachViaIOCTL(ctx, attachment.Port, logger)
	case LocalhostAttachmentBackendCommand:
		return detachViaCommand(ctx, attachment.Port, logger)
	default:
		return fmt.Errorf("unknown localhost attachment backend %d", attachment.Backend)
	}
}

func detachViaIOCTL(_ context.Context, port int32, logger *slog.Logger) error {
	return detachViaIOCTLWithOps(port, logger, realNativeDetachOps())
}

func detachViaIOCTLWithOps(port int32, logger *slog.Logger, ops nativeDetachOps) (err error) {
	nativeStart := time.Now()
	var validationUs, discoveryUs, openUs, ioctlUs int64
	var reachedIOCTL bool
	defer func() {
		logNativeIOCTLTiming(logger, "detach", err, reachedIOCTL, time.Since(nativeStart), discoveryUs, openUs, ioctlUs, validationUs, nil)
	}()

	validationStart := time.Now()
	request, validationErr := newPlugoutIOCTL(port)
	validationUs = time.Since(validationStart).Microseconds()
	if validationErr != nil {
		err = validationErr
		return
	}
	discoveryStart := time.Now()
	devicePath, discoveryErr := ops.discoverDevicePath()
	discoveryUs = time.Since(discoveryStart).Microseconds()
	if discoveryErr != nil {
		err = fmt.Errorf("discovery: %w", discoveryErr)
		return
	}
	openStart := time.Now()
	handle, openErr := ops.openDevice(devicePath)
	openUs = time.Since(openStart).Microseconds()
	if openErr != nil {
		err = openErr
		return
	}
	defer ops.closeDevice(handle) // nolint

	ioctlStart := time.Now()
	_, ioctlErr := ops.plugoutHardware(handle, &request)
	ioctlUs = time.Since(ioctlStart).Microseconds()
	reachedIOCTL = true
	if ioctlErr != nil {
		err = classifyNativeDetachResult(ioctlErr)
		return
	}
	logger.Info("Successfully detached device via IOCTL", "usbPort", port)
	return
}

func newPlugoutIOCTL(port int32) (plugoutIOCTL, error) {
	if port <= 0 {
		return plugoutIOCTL{}, fmt.Errorf("invalid USB/IP import port %d", port)
	}
	return plugoutIOCTL{Size: uint32(unsafe.Sizeof(plugoutIOCTL{})), Port: port}, nil
}

func detachViaCommand(ctx context.Context, port int32, logger *slog.Logger) error {
	return detachViaCommandWithRunner(ctx, port, logger, realCommandRunner)
}

func detachViaCommandWithRunner(ctx context.Context, port int32, logger *slog.Logger, run commandRunner) (err error) {
	commandStart := time.Now()
	var processUs, classificationUs int64
	var backendCalled bool
	defer func() {
		logCommandBackendTiming(logger, "detach", err, backendCalled, time.Since(commandStart), processUs, classificationUs)
	}()

	args, argsErr := usbipDetachCommandArgs(port)
	if argsErr != nil {
		err = argsErr
		return
	}
	processStart := time.Now()
	output, cmdErr := run(ctx, "usbip", args...)
	processUs = time.Since(processStart).Microseconds()
	backendCalled = true
	if cmdErr != nil {
		classifyStart := time.Now()
		classifyErr := classifyUSBIPDetachCommandResult(cmdErr)
		classificationUs = time.Since(classifyStart).Microseconds()
		err = fmt.Errorf("%w: %s", classifyErr, output)
		return
	}
	logger.Info("Successfully detached device via usbip command", "usbPort", port)
	return
}

func getDeviceInterfacePath(guid *windows.GUID) (string, error) {
	r0, _, e1 := syscall.SyscallN(procSetupDiGetClassDevsW.Addr(),
		uintptr(unsafe.Pointer(guid)),
		0,
		0,
		uintptr(DigcfPresent|DigcfDeviceInterface))

	devInfo := windows.Handle(r0)
	if devInfo == windows.InvalidHandle {
		if e1 != 0 {
			return "", fmt.Errorf("discovery: SetupDiGetClassDevsW failed: %w", e1)
		}
		return "", fmt.Errorf("discovery: SetupDiGetClassDevsW failed with invalid handle")
	}
	defer func() {
		_, _, err := syscall.SyscallN(procSetupDiDestroyDeviceInfoList.Addr(), uintptr(devInfo))
		if err != 0 {
			slog.Error("SetupDiDestroyDeviceInfoList failed", "error", err)
		}
	}()

	var interfaceData SpDeviceInterfaceData
	interfaceData.CbSize = uint32(unsafe.Sizeof(interfaceData))

	r1, _, e2 := syscall.SyscallN(procSetupDiEnumDeviceInterfaces.Addr(),
		uintptr(devInfo),
		0,
		uintptr(unsafe.Pointer(guid)),
		0,
		uintptr(unsafe.Pointer(&interfaceData)))

	if r1 == 0 {
		if e2 != 0 {
			return "", fmt.Errorf("discovery: usbip-win2 driver not found: %w", e2)
		}
		return "", fmt.Errorf("discovery: usbip-win2 driver not found")
	}

	var requiredSize uint32
	r2, _, err := syscall.SyscallN(procSetupDiGetDeviceInterfaceDetailW.Addr(),
		uintptr(devInfo),
		uintptr(unsafe.Pointer(&interfaceData)),
		0,
		0,
		uintptr(unsafe.Pointer(&requiredSize)),
		0)
	if r2 == 0 && err != windows.ERROR_INSUFFICIENT_BUFFER {
		return "", fmt.Errorf("discovery: SetupDiGetDeviceInterfaceDetailW (size query) failed: %w", err)
	}
	if requiredSize == 0 {
		return "", fmt.Errorf("discovery: SetupDiGetDeviceInterfaceDetailW (size query) returned invalid required size")
	}

	detailData := make([]byte, requiredSize)
	detailHeader := (*SpDeviceInterfaceDetailData)(unsafe.Pointer(&detailData[0]))
	detailHeader.CbSize = uint32(unsafe.Sizeof(SpDeviceInterfaceDetailData{}))

	r3, _, e3 := syscall.SyscallN(procSetupDiGetDeviceInterfaceDetailW.Addr(),
		uintptr(devInfo),
		uintptr(unsafe.Pointer(&interfaceData)),
		uintptr(unsafe.Pointer(detailHeader)),
		uintptr(requiredSize),
		0,
		0)

	if r3 == 0 {
		if e3 != 0 {
			return "", fmt.Errorf("discovery: SetupDiGetDeviceInterfaceDetailW failed: %w", e3)
		}
		return "", fmt.Errorf("discovery: SetupDiGetDeviceInterfaceDetailW failed")
	}

	path := windows.UTF16PtrToString(&detailHeader.DevicePath[0])
	return path, nil
}

func CheckAutoAttachPrerequisites(useNativeIOCTL bool, logger *slog.Logger) bool {
	if useNativeIOCTL {
		_, err := getDeviceInterfacePath(&deviceGUID)
		if err != nil {
			logger.Warn("Native IOCTL auto-attach prerequisites not met", "error", err)
			logger.Warn("Native IOCTL auto-attach is unavailable until discovery succeeds")
			logger.Info("If usbip-win2 is not installed, download and install:")
			logger.Info("  https://github.com/vadimgrn/usbip-win2")
			logger.Info("  https://github.com/OSSign/vadimgrn--usbip-win2")
			return false
		}
		logger.Debug("usbip-win2 driver found")
		return true
	}

	if _, err := exec.LookPath("usbip.exe"); err != nil {
		logger.Warn("USB/IP tool 'usbip.exe' not found in PATH")
		logger.Warn("Auto-attach requires usbip-win2")
		logger.Info("Download and install usbip-win2:")
		logger.Info("  https://github.com/vadimgrn/usbip-win2")
		return false
	}

	logger.Debug("usbip.exe tool found in PATH")
	return true
}
