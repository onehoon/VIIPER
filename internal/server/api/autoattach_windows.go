//go:build windows

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"

	"github.com/Alia5/VIIPER/usbip"
	"golang.org/x/sys/windows"
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
	niMaxHost = 1025
	niMaxServ = 32
)

// usbip-win2 v0.9.7.7 ABI reference:
// https://github.com/vadimgrn/usbip-win2/blob/7c219953101cc5d0ec9a0bcb3eb87259cf72bedd/include/usbip/vhci.h
//
// plugin_hardware is intentionally pinned to that released ABI. Later
// usbip-win2 versions add fields and are not supported by this native binding.
type attachIOCTL struct {
	Size       uint32
	PortOutput int32
	BusID      [32]byte
	Service    [niMaxServ]byte
	Host       [niMaxHost]byte
}

// plugoutIOCTL is usbip-win2 v0.9.7.7 ioctl::plugout_hardware.
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

var (
	attachPortOutputLength = uint32(unsafe.Offsetof(attachIOCTL{}.PortOutput) + unsafe.Sizeof(attachIOCTL{}.PortOutput))
	attachInputLength      = uint32(unsafe.Sizeof(attachIOCTL{}))
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

func attachViaIOCTL(_ context.Context, deviceExportMeta *usbip.ExportMeta, usbipServerPort uint16, logger *slog.Logger) (LocalhostAttachment, error) {
	logger.Info("Auto-attaching localhost client via native IOCTL",
		"busID", deviceExportMeta.BusID,
		"deviceID", deviceExportMeta.DevID)

	if usbipServerPort == 0 {
		return LocalhostAttachment{}, fmt.Errorf("argumentValidation: invalid TCP port number (0)")
	}

	devicePath, err := getDeviceInterfacePath(&deviceGUID)
	if err != nil {
		return LocalhostAttachment{}, fmt.Errorf("discovery: %w", err)
	}

	logger.Debug("Found usbip-win2 device", "path", devicePath)

	var ioctlData attachIOCTL
	ioctlData.Size = uint32(unsafe.Sizeof(ioctlData))

	busID := fmt.Sprintf("%d-%d", deviceExportMeta.BusID, deviceExportMeta.DevID)
	if len(busID) >= len(ioctlData.BusID) {
		return LocalhostAttachment{}, fmt.Errorf("argumentValidation: bus ID too long: %s", busID)
	}
	copy(ioctlData.BusID[:], busID)

	service := fmt.Sprintf("%d", usbipServerPort)
	if len(service) >= len(ioctlData.Service) {
		return LocalhostAttachment{}, fmt.Errorf("argumentValidation: service string too long: %s", service)
	}
	copy(ioctlData.Service[:], service)
	copy(ioctlData.Host[:], "localhost")

	devicePathUTF16, err := windows.UTF16PtrFromString(devicePath)
	if err != nil {
		return LocalhostAttachment{}, fmt.Errorf("open: failed to convert device path: %w", err)
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
		return LocalhostAttachment{}, fmt.Errorf("open: failed to open usbip-win2 device: %w", err)
	}
	defer windows.CloseHandle(handle) // nolint

	logger.Debug("Opened device handle")

	var bytesReturned uint32
	inputLength, outputLength := nativeAttachIOCTLLengths()
	err = windows.DeviceIoControl(
		handle,
		ioctlPluginHardware,
		(*byte)(unsafe.Pointer(&ioctlData)),
		inputLength,
		(*byte)(unsafe.Pointer(&ioctlData)),
		outputLength,
		&bytesReturned,
		nil,
	)
	if err != nil {
		return LocalhostAttachment{}, fmt.Errorf("%w: native PLUGIN_HARDWARE DeviceIoControl failed: %v", ErrAttachmentOutcomeUnknown, err)
	}

	logger.Debug("IOCTL completed", "bytesReturned", bytesReturned, "portOutput", ioctlData.PortOutput)

	if err := validateNativeAttachResponse(bytesReturned, ioctlData.PortOutput); err != nil {
		return LocalhostAttachment{}, err
	}

	logger.Info("Successfully attached device via IOCTL",
		"busID", deviceExportMeta.BusID,
		"deviceID", deviceExportMeta.DevID,
		"usbPort", ioctlData.PortOutput)

	return LocalhostAttachment{Backend: LocalhostAttachmentBackendNativeIOCTL, Port: ioctlData.PortOutput}, nil
}

func nativeAttachIOCTLLengths() (input uint32, output uint32) {
	return attachInputLength, attachPortOutputLength
}

func attachViaCommand(ctx context.Context, deviceExportMeta *usbip.ExportMeta, usbipServerPort uint16, logger *slog.Logger) (LocalhostAttachment, error) {
	logger.Info("Auto-attaching localhost client", "busID", deviceExportMeta.BusID, "deviceID", deviceExportMeta.DevID)

	cmd := exec.CommandContext(
		ctx,
		"usbip",
		usbipAttachCommandArgs(usbipServerPort, fmt.Sprintf("%d-%d", deviceExportMeta.BusID, deviceExportMeta.DevID))...,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		logger.Error("Failed to attach device",
			"error", err,
			"port", usbipServerPort,
			"output", string(output))
		port, resultErr := classifyUSBIPAttachCommandResult(output, err)
		if resultErr != nil {
			return LocalhostAttachment{}, resultErr
		}
		return LocalhostAttachment{Backend: LocalhostAttachmentBackendCommand, Port: port}, nil
	}
	logger.Debug("usbip attach output", "output", string(output))
	port, err := classifyUSBIPAttachCommandResult(output, nil)
	if err != nil {
		return LocalhostAttachment{}, err
	}
	return LocalhostAttachment{Backend: LocalhostAttachmentBackendCommand, Port: port}, nil
}

// attachViaCommandLegacy preserves the old error-only auto-attach behavior.
// It intentionally does not participate in tracked attachment ownership; that
// migration must happen atomically with PR3B's device lifecycle state.
func attachViaCommandLegacy(ctx context.Context, deviceExportMeta *usbip.ExportMeta, usbipServerPort uint16, logger *slog.Logger) error {
	logger.Info("Auto-attaching localhost client", "busID", deviceExportMeta.BusID, "deviceID", deviceExportMeta.DevID)
	cmd := exec.CommandContext(
		ctx,
		"usbip",
		"--tcp-port", strconv.FormatUint(uint64(usbipServerPort), 10),
		"attach", "-r", "localhost", "-b", fmt.Sprintf("%d-%d", deviceExportMeta.BusID, deviceExportMeta.DevID),
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		logger.Error("Failed to attach device", "error", err, "port", usbipServerPort, "output", string(output))
		return err
	}
	logger.Debug("usbip attach output", "output", string(output))
	return nil
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
	request, err := newPlugoutIOCTL(port)
	if err != nil {
		return err
	}
	devicePath, err := getDeviceInterfacePath(&deviceGUID)
	if err != nil {
		return fmt.Errorf("discovery: %w", err)
	}
	devicePathUTF16, err := windows.UTF16PtrFromString(devicePath)
	if err != nil {
		return fmt.Errorf("open: failed to convert device path: %w", err)
	}
	handle, err := windows.CreateFile(devicePathUTF16, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return fmt.Errorf("open: failed to open usbip-win2 device: %w", err)
	}
	defer windows.CloseHandle(handle) // nolint

	var bytesReturned uint32
	if err := windows.DeviceIoControl(handle, ioctlPlugoutHardware, (*byte)(unsafe.Pointer(&request)), uint32(unsafe.Sizeof(request)), nil, 0, &bytesReturned, nil); err != nil {
		return fmt.Errorf("%w: PLUGOUT_HARDWARE DeviceIoControl failed: %v", ErrDetachmentOutcomeUnknown, err)
	}
	logger.Info("Successfully detached device via IOCTL", "usbPort", port)
	return nil
}

func newPlugoutIOCTL(port int32) (plugoutIOCTL, error) {
	if port <= 0 {
		return plugoutIOCTL{}, fmt.Errorf("invalid USB/IP import port %d", port)
	}
	return plugoutIOCTL{Size: uint32(unsafe.Sizeof(plugoutIOCTL{})), Port: port}, nil
}

func detachViaCommand(ctx context.Context, port int32, logger *slog.Logger) error {
	args, err := usbipDetachCommandArgs(port)
	if err != nil {
		return err
	}
	output, err := exec.CommandContext(ctx, "usbip", args...).CombinedOutput()
	if err != nil {
		var startErr *exec.Error
		if errors.As(err, &startErr) {
			return err
		}
		return fmt.Errorf("%w: usbip detach process started but did not report success: %v: %s", ErrDetachmentOutcomeUnknown, err, output)
	}
	logger.Info("Successfully detached device via usbip command", "usbPort", port)
	return nil
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
