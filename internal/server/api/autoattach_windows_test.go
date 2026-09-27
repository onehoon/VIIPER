//go:build windows

package api

import (
	"context"
	"errors"
	"log/slog"
	"os/exec"
	"reflect"
	"testing"
	"unsafe"

	"github.com/Alia5/VIIPER/usbip"
	"golang.org/x/sys/windows"
)

func TestUSBIPWin20980NativeABIContract(t *testing.T) {
	var request attachIOCTL0980
	if got, want := unsafe.Offsetof(request.Size), uintptr(0); got != want {
		t.Fatalf("plugin_hardware size offset = %d, want %d", got, want)
	}
	if got, want := unsafe.Offsetof(request.PortOutput), uintptr(4); got != want {
		t.Fatalf("plugin_hardware port offset = %d, want %d", got, want)
	}
	if got, want := unsafe.Offsetof(request.BusID), uintptr(8); got != want {
		t.Fatalf("plugin_hardware busid offset = %d, want %d", got, want)
	}
	if got, want := unsafe.Offsetof(request.Service), uintptr(40); got != want {
		t.Fatalf("plugin_hardware service offset = %d, want %d", got, want)
	}
	if got, want := unsafe.Offsetof(request.Host), uintptr(72); got != want {
		t.Fatalf("plugin_hardware host offset = %d, want %d", got, want)
	}
	if got, want := attachPortOutputLength, uint32(8); got != want {
		t.Fatalf("plugin_hardware output length = %d, want %d", got, want)
	}
	if got, want := unsafe.Offsetof(request.Serial), uintptr(1100); got != want {
		t.Fatalf("plugin_hardware serial offset = %d, want %d", got, want)
	}
	if got, want := unsafe.Offsetof(request.WskEvents), uintptr(1116); got != want {
		t.Fatalf("plugin_hardware WSK events offset = %d, want %d", got, want)
	}
	if got, want := unsafe.Sizeof(request.WskEvents), uintptr(1); got != want {
		t.Fatalf("plugin_hardware WSK events size = %d, want %d", got, want)
	}
	if got, want := unsafe.Sizeof(request), uintptr(1120); got != want {
		t.Fatalf("plugin_hardware size = %d, want %d", got, want)
	}
	if got, want := len(request.BusID), 32; got != want {
		t.Fatalf("BUS_ID_SIZE = %d, want %d", got, want)
	}
	if got, want := len(request.Service), 32; got != want {
		t.Fatalf("NI_MAXSERV = %d, want %d", got, want)
	}
	if got, want := len(request.Host), 1025; got != want {
		t.Fatalf("NI_MAXHOST = %d, want %d", got, want)
	}
	if got, want := len(request.Serial), 16; got != want {
		t.Fatalf("SERIAL_BUFSZ = %d, want %d", got, want)
	}
	if got, want := unsafe.Sizeof(plugoutIOCTL{}), uintptr(8); got != want {
		t.Fatalf("plugout_hardware size = %d, want %d", got, want)
	}
}

func TestUSBIPWin20981NativeABIContract(t *testing.T) {
	var request attachIOCTL0981
	for _, field := range []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"size", unsafe.Offsetof(request.Size), 0},
		{"port", unsafe.Offsetof(request.PortOutput), 4},
		{"location hash", unsafe.Offsetof(request.LocationHash), 8},
		{"busid", unsafe.Offsetof(request.BusID), 12},
		{"service", unsafe.Offsetof(request.Service), 44},
		{"host", unsafe.Offsetof(request.Host), 76},
		{"serial", unsafe.Offsetof(request.Serial), 1104},
		{"wsk events", unsafe.Offsetof(request.WskEvents), 1120},
	} {
		if field.got != field.want {
			t.Errorf("plugin_hardware %s offset = %d, want %d", field.name, field.got, field.want)
		}
	}
	if got, want := unsafe.Sizeof(request.WskEvents), uintptr(1); got != want {
		t.Fatalf("plugin_hardware WSK events size = %d, want %d", got, want)
	}
	if got, want := unsafe.Sizeof(request), uintptr(1124); got != want {
		t.Fatalf("plugin_hardware size = %d, want %d", got, want)
	}
	if got, want := attachPortOutputLength, uint32(8); got != want {
		t.Fatalf("plugin_hardware output length = %d, want %d", got, want)
	}
	if len(request.BusID) != 32 || len(request.Service) != 32 || len(request.Host) != 1025 || len(request.Serial) != 16 {
		t.Fatalf("unexpected upstream field sizes: busid=%d service=%d host=%d serial=%d", len(request.BusID), len(request.Service), len(request.Host), len(request.Serial))
	}
	if got, want := unsafe.Sizeof(plugoutIOCTL{}), uintptr(8); got != want {
		t.Fatalf("plugout_hardware size = %d, want %d", got, want)
	}
}

func TestSelectUSBIPWin2NativeABI(t *testing.T) {
	for _, test := range []struct {
		version string
		wantABI usbipWin2NativeABI
	}{
		{version: "0.9.8.0", wantABI: usbipWin2NativeABI0980},
		{version: " 0.9.8.1\r\n", wantABI: usbipWin2NativeABI0981},
		{version: "", wantABI: usbipWin2NativeABIUnknown},
		{version: "0.9.7.8", wantABI: usbipWin2NativeABIUnknown},
		{version: "0.9.8.2", wantABI: usbipWin2NativeABIUnknown},
		{version: "not-a-version", wantABI: usbipWin2NativeABIUnknown},
	} {
		t.Run(test.version, func(t *testing.T) {
			if got := selectUSBIPWin2NativeABI(test.version); got != test.wantABI {
				t.Fatalf("selectUSBIPWin2NativeABI(%q) = %v, want %v", test.version, got, test.wantABI)
			}
		})
	}
}

func TestNativeAttachResponseRequiresExactOwnershipToken(t *testing.T) {
	if err := validateNativeAttachResponse(attachPortOutputLength, 37); err != nil {
		t.Fatalf("valid native response rejected: %v", err)
	}
	for _, response := range []struct {
		bytes uint32
		port  int32
	}{
		{bytes: attachPortOutputLength - 1, port: 37},
		{bytes: attachPortOutputLength + 1, port: 37},
		{bytes: attachPortOutputLength, port: 0},
		{bytes: attachPortOutputLength, port: -1},
	} {
		if err := validateNativeAttachResponse(response.bytes, response.port); err == nil {
			t.Fatalf("accepted invalid native response bytes=%d port=%d", response.bytes, response.port)
		}
	}
}

func TestVersionedNativeAttachResponseValidationForBothABIs(t *testing.T) {
	for _, version := range []string{"0.9.8.0", "0.9.8.1"} {
		t.Run(version, func(t *testing.T) {
			for _, response := range []struct {
				name          string
				bytesReturned uint32
				port          int32
				wantErr       bool
			}{
				{name: "exact prefix and positive port", bytesReturned: 8, port: 37},
				{name: "short prefix", bytesReturned: 7, port: 37, wantErr: true},
				{name: "long prefix", bytesReturned: 9, port: 37, wantErr: true},
				{name: "zero port", bytesReturned: 8, port: 0, wantErr: true},
				{name: "negative port", bytesReturned: 8, port: -1, wantErr: true},
			} {
				t.Run(response.name, func(t *testing.T) {
					ops := fakeNativeAttachOps(nil, nil, nil, response.port, response.bytesReturned)
					ops.readInstalledVersion = func() (string, error) { return version, nil }
					attachment, err := attachViaIOCTLWithOps(&usbip.ExportMeta{BusID: 9, DevID: 12}, 3241, slog.Default(), ops)
					if response.wantErr {
						if !errors.Is(err, ErrAttachmentOutcomeUnknown) {
							t.Fatalf("error = %v, want ErrAttachmentOutcomeUnknown", err)
						}
						return
					}
					if err != nil || attachment.Port != response.port {
						t.Fatalf("attachment=%+v err=%v, want port %d", attachment, err, response.port)
					}
				})
			}
		})
	}
}

func TestAttachViaIOCTLUsesIPv4LoopbackEndpoint(t *testing.T) {
	meta := &usbip.ExportMeta{BusID: 1, DevID: 2}
	var host [niMaxHost]byte
	ops := nativeAttachOps{
		readInstalledVersion: func() (string, error) { return "0.9.8.0", nil },
		discoverDevicePath:   func() (string, error) { return "fake-device-path", nil },
		openDevice:           func(string) (windows.Handle, error) { return windows.Handle(1), nil },
		closeDevice:          func(windows.Handle) error { return nil },
		pluginHardware: func(_ windows.Handle, request unsafe.Pointer, inputLength, outputLength uint32) (uint32, error) {
			if inputLength != 1120 || outputLength != attachPortOutputLength {
				t.Fatalf("IOCTL lengths = %d/%d, want 1120/8", inputLength, outputLength)
			}
			data := (*attachIOCTL0980)(request)
			copy(host[:], data.Host[:])
			data.PortOutput = 55
			return attachPortOutputLength, nil
		},
	}
	if _, err := attachViaIOCTLWithOps(meta, 3241, slog.Default(), ops); err != nil {
		t.Fatalf("native attach failed: %v", err)
	}
	if got := string(host[:len("127.0.0.1")]); got != "127.0.0.1" {
		t.Fatalf("native host = %q, want 127.0.0.1", got)
	}
}

func TestAttachViaIOCTLBuildsV0980ZeroCopyRequest(t *testing.T) {
	meta := &usbip.ExportMeta{BusID: 9, DevID: 12}
	var captured attachIOCTL0980
	ops := nativeAttachOps{
		readInstalledVersion: func() (string, error) { return "0.9.8.0", nil },
		discoverDevicePath:   func() (string, error) { return "fake-device-path", nil },
		openDevice:           func(string) (windows.Handle, error) { return windows.Handle(1), nil },
		closeDevice:          func(windows.Handle) error { return nil },
		pluginHardware: func(_ windows.Handle, request unsafe.Pointer, inputLength, outputLength uint32) (uint32, error) {
			if inputLength != 1120 || outputLength != attachPortOutputLength {
				t.Fatalf("IOCTL lengths = %d/%d, want 1120/8", inputLength, outputLength)
			}
			data := (*attachIOCTL0980)(request)
			captured = *data
			data.PortOutput = 55
			return attachPortOutputLength, nil
		},
	}
	if _, err := attachViaIOCTLWithOps(meta, 3241, slog.Default(), ops); err != nil {
		t.Fatalf("native attach failed: %v", err)
	}
	if captured.Size != 1120 || string(captured.BusID[:4]) != "9-12" || string(captured.Service[:4]) != "3241" || string(captured.Host[:9]) != "127.0.0.1" {
		t.Fatalf("unexpected request contents: %+v", captured)
	}
	if captured.ImportedDeviceLocationPadding != [3]byte{} || captured.Serial != [serialBufSize]byte{} || captured.WskEvents {
		t.Fatalf("serial/WSK policy = serial=%v wskEvents=%v", captured.Serial, captured.WskEvents)
	}
}

func TestAttachViaIOCTLSelectsAndSubmitsExactlyOneVersionedRequest(t *testing.T) {
	for _, test := range []struct {
		name         string
		version      string
		wantABI      string
		wantLength   uint32
		checkRequest func(*testing.T, unsafe.Pointer)
	}{
		{
			name:       "0.9.8.0",
			version:    "0.9.8.0",
			wantABI:    "0.9.8.0",
			wantLength: 1120,
			checkRequest: func(t *testing.T, request unsafe.Pointer) {
				data := (*attachIOCTL0980)(request)
				if data.Size != 1120 || data.PortOutput != 0 || string(data.BusID[:4]) != "9-12" || string(data.Service[:4]) != "3241" || string(data.Host[:9]) != "127.0.0.1" {
					t.Fatalf("unexpected v0.9.8.0 request: %+v", *data)
				}
				if data.ImportedDeviceLocationPadding != [3]byte{} || data.Serial != [serialBufSize]byte{} || data.WskEvents {
					t.Fatalf("unexpected v0.9.8.0 padding/serial/WSK: %+v", *data)
				}
				data.PortOutput = 55
			},
		},
		{
			name:       "0.9.8.1",
			version:    "0.9.8.1",
			wantABI:    "0.9.8.1",
			wantLength: 1124,
			checkRequest: func(t *testing.T, request unsafe.Pointer) {
				data := (*attachIOCTL0981)(request)
				if data.Size != 1124 || data.PortOutput != 0 || data.LocationHash != 0 || string(data.BusID[:4]) != "9-12" || string(data.Service[:4]) != "3241" || string(data.Host[:9]) != "127.0.0.1" {
					t.Fatalf("unexpected v0.9.8.1 request: %+v", *data)
				}
				if data.ImportedDeviceLocationPadding != [3]byte{} || data.Serial != [serialBufSize]byte{} || data.WskEvents {
					t.Fatalf("unexpected v0.9.8.1 padding/serial/WSK: %+v", *data)
				}
				data.PortOutput = 55
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			meta := &usbip.ExportMeta{BusID: 9, DevID: 12}
			var calls int
			var sequence []string
			handler := &timingRecordingHandler{}
			ops := nativeAttachOps{
				readInstalledVersion: func() (string, error) {
					sequence = append(sequence, "version")
					return test.version, nil
				},
				discoverDevicePath: func() (string, error) {
					sequence = append(sequence, "discover")
					return "fake-device-path", nil
				},
				openDevice: func(string) (windows.Handle, error) {
					sequence = append(sequence, "open")
					return windows.Handle(1), nil
				},
				closeDevice: func(windows.Handle) error { return nil },
				pluginHardware: func(_ windows.Handle, request unsafe.Pointer, inputLength, outputLength uint32) (uint32, error) {
					calls++
					sequence = append(sequence, "ioctl")
					if inputLength != test.wantLength || outputLength != attachPortOutputLength {
						t.Fatalf("IOCTL lengths = %d/%d, want %d/8", inputLength, outputLength, test.wantLength)
					}
					test.checkRequest(t, request)
					return attachPortOutputLength, nil
				},
			}
			attachment, err := attachViaIOCTLWithOps(meta, 3241, slog.New(handler), ops)
			if err != nil || attachment.Port != 55 || attachment.Backend != LocalhostAttachmentBackendNativeIOCTL {
				t.Fatalf("attachment=%+v err=%v", attachment, err)
			}
			if calls != 1 {
				t.Fatalf("native submit calls = %d, want 1", calls)
			}
			if !reflect.DeepEqual(sequence, []string{"version", "discover", "open", "ioctl"}) {
				t.Fatalf("operation order = %v, want version/discover/open/ioctl", sequence)
			}
			attrs := requireSingleTimingRecord(t, handler)
			if attrs["installedVersion"] != test.version || attrs["selectedABI"] != test.wantABI || attrs["inputLength"] != uint64(test.wantLength) {
				t.Fatalf("ABI timing evidence = %+v, want version=%s ABI=%s inputLength=%d", attrs, test.version, test.wantABI, test.wantLength)
			}
		})
	}
}

func TestAttachViaIOCTLLogsDeviceIoControlError(t *testing.T) {
	meta := &usbip.ExportMeta{BusID: 9, DevID: 12}
	ioctlErr := errors.New("ERROR_INVALID_PARAMETER")
	handler := &timingRecordingHandler{}
	ops := fakeNativeAttachOps(nil, nil, ioctlErr, 0, 0)

	_, err := attachViaIOCTLWithOps(meta, 3241, slog.New(handler), ops)
	if !errors.Is(err, ErrAttachmentOutcomeUnknown) {
		t.Fatalf("error = %v, want ErrAttachmentOutcomeUnknown", err)
	}

	for _, record := range handler.records {
		if record.Message != "native PLUGIN_HARDWARE DeviceIoControl failed" {
			continue
		}
		attrs := recordAttrs(record)
		loggedError, ok := attrs["error"].(error)
		if !ok || loggedError.Error() != ioctlErr.Error() {
			t.Fatalf("logged error = %v, want %v", attrs["error"], ioctlErr)
		}
		inputLength, inputOK := attrs["inputLength"].(uint64)
		outputLength, outputOK := attrs["outputLength"].(uint64)
		if !inputOK || !outputOK || inputLength != 1120 || outputLength != 8 {
			t.Fatalf("logged lengths = input %v (%T) output %v (%T), want values 1120/8", attrs["inputLength"], attrs["inputLength"], attrs["outputLength"], attrs["outputLength"])
		}
		return
	}
	t.Fatal("native DeviceIoControl error log record was not emitted")
}

func TestUnsupportedUSBIPWin2VersionIsKnownPreSubmitFallback(t *testing.T) {
	meta := &usbip.ExportMeta{BusID: 9, DevID: 12}
	ops := fakeNativeAttachOps(nil, nil, nil, 0, 0)
	ops.readInstalledVersion = func() (string, error) { return "0.9.8.2", nil }
	var ioctlCalls, commandCalls int
	ops.pluginHardware = func(windows.Handle, unsafe.Pointer, uint32, uint32) (uint32, error) {
		ioctlCalls++
		return 0, errors.New("must not submit unsupported ABI")
	}
	native := func(context.Context, *usbip.ExportMeta, uint16, *slog.Logger) (LocalhostAttachment, error) {
		return attachViaIOCTLWithOps(meta, 3241, slog.Default(), ops)
	}
	command := func(context.Context, *usbip.ExportMeta, uint16, *slog.Logger) (LocalhostAttachment, error) {
		commandCalls++
		return LocalhostAttachment{Backend: LocalhostAttachmentBackendCommand, Port: 72}, nil
	}
	attachment, err := attachLocalhostClientWithFallback(context.Background(), meta, 3241, true, slog.Default(), native, command)
	if err != nil || attachment.Backend != LocalhostAttachmentBackendCommand || attachment.Port != 72 {
		t.Fatalf("fallback attachment=%+v err=%v", attachment, err)
	}
	if ioctlCalls != 0 || commandCalls != 1 {
		t.Fatalf("native calls=%d command calls=%d, want 0/1", ioctlCalls, commandCalls)
	}
}

func TestSubmittedVersionedNativeFailureNeverRetriesOrFallsBack(t *testing.T) {
	for _, version := range []string{"0.9.8.0", "0.9.8.1"} {
		t.Run(version, func(t *testing.T) {
			meta := &usbip.ExportMeta{BusID: 9, DevID: 12}
			ops := fakeNativeAttachOps(nil, nil, errors.New("submitted IOCTL failure"), 0, 0)
			ops.readInstalledVersion = func() (string, error) { return version, nil }
			var ioctlCalls, commandCalls int
			previous := ops.pluginHardware
			ops.pluginHardware = func(handle windows.Handle, request unsafe.Pointer, inputLength, outputLength uint32) (uint32, error) {
				ioctlCalls++
				return previous(handle, request, inputLength, outputLength)
			}
			native := func(context.Context, *usbip.ExportMeta, uint16, *slog.Logger) (LocalhostAttachment, error) {
				return attachViaIOCTLWithOps(meta, 3241, slog.Default(), ops)
			}
			command := func(context.Context, *usbip.ExportMeta, uint16, *slog.Logger) (LocalhostAttachment, error) {
				commandCalls++
				return LocalhostAttachment{}, nil
			}
			_, err := attachLocalhostClientWithFallback(context.Background(), meta, 3241, true, slog.Default(), native, command)
			if !errors.Is(err, ErrAttachmentOutcomeUnknown) {
				t.Fatalf("error = %v, want ErrAttachmentOutcomeUnknown", err)
			}
			if ioctlCalls != 1 || commandCalls != 0 {
				t.Fatalf("native calls=%d command calls=%d, want 1/0", ioctlCalls, commandCalls)
			}
		})
	}
}

func TestUnsupportedUSBIPWin2VersionLogsNoGuessedABISize(t *testing.T) {
	for _, test := range []struct {
		name    string
		version string
		readErr error
		want    string
	}{
		{name: "malformed", version: "not-a-version", want: "not-a-version"},
		{name: "unsupported", version: "0.9.8.2", want: "0.9.8.2"},
		{name: "registry unavailable", readErr: errors.New("registry missing"), want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := &timingRecordingHandler{}
			ops := fakeNativeAttachOps(nil, nil, nil, 0, 0)
			ops.readInstalledVersion = func() (string, error) { return test.version, test.readErr }
			ops.discoverDevicePath = func() (string, error) {
				t.Fatal("discovery must not run without a supported ABI")
				return "", nil
			}
			_, err := attachViaIOCTLWithOps(&usbip.ExportMeta{BusID: 9, DevID: 12}, 3241, slog.New(handler), ops)
			if err == nil || errors.Is(err, ErrAttachmentOutcomeUnknown) {
				t.Fatalf("error = %v, want known pre-submit error", err)
			}
			attrs := requireSingleTimingRecord(t, handler)
			if attrs["installedVersion"] != test.want || attrs["selectedABI"] != "unsupported" || attrs["inputLength"] != uint64(0) || attrs["backendCalled"] != false {
				t.Fatalf("unsupported ABI timing evidence = %+v", attrs)
			}
		})
	}
}

func TestUSBIPLegacyAttachCommandSelectsZeroCopy(t *testing.T) {
	want := []string{"--tcp-port", "3241", "attach", "-r", "127.0.0.1", "-b", "9-12", "--receive-mode=zero-copy"}
	if got := usbipLegacyAttachCommandArgs(3241, "9-12"); !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy attach arguments = %#v, want %#v", got, want)
	}
}

// These tests exercise the native-ioctl/command breakdown timing entirely through the
// nativeAttachOps/nativeDetachOps/commandRunner fake seams -- they never touch the real
// usbip-win2 driver, SetupAPI, DeviceIoControl, or a real usbip.exe process, and never skip.

func fakeNativeAttachOps(discoverErr, openErr, ioctlErr error, portOutput int32, bytesReturned uint32) nativeAttachOps {
	return nativeAttachOps{
		readInstalledVersion: func() (string, error) { return "0.9.8.0", nil },
		discoverDevicePath: func() (string, error) {
			if discoverErr != nil {
				return "", discoverErr
			}
			return "fake-device-path", nil
		},
		openDevice: func(string) (windows.Handle, error) {
			if openErr != nil {
				return 0, openErr
			}
			return windows.Handle(1), nil
		},
		closeDevice: func(windows.Handle) error { return nil },
		pluginHardware: func(_ windows.Handle, request unsafe.Pointer, inputLength, _ uint32) (uint32, error) {
			if ioctlErr != nil {
				return 0, ioctlErr
			}
			switch inputLength {
			case 1120:
				(*attachIOCTL0980)(request).PortOutput = portOutput
			case 1124:
				(*attachIOCTL0981)(request).PortOutput = portOutput
			default:
				return 0, errors.New("unexpected native attach input length")
			}
			return bytesReturned, nil
		},
	}
}

func TestAttachViaIOCTLWithOpsTimingBreaksDownEachStage(t *testing.T) {
	meta := &usbip.ExportMeta{BusID: 1, DevID: 2}

	t.Run("discovery failure: only discoveryUs measured", func(t *testing.T) {
		handler := &timingRecordingHandler{}
		ops := fakeNativeAttachOps(errors.New("fake discovery failure"), nil, nil, 0, 0)
		_, err := attachViaIOCTLWithOps(meta, 3241, slog.New(handler), ops)
		if err == nil {
			t.Fatal("discovery failure must be reported")
		}
		attrs := requireSingleTimingRecord(t, handler)
		if attrs["operation"] != "attach" || attrs["layer"] != "native-ioctl" || attrs["backendCalled"] != false {
			t.Fatalf("unexpected timing attrs: %+v", attrs)
		}
		if attrs["openUs"] != int64(0) || attrs["ioctlUs"] != int64(0) || attrs["validationUs"] != int64(0) {
			t.Fatalf("stages after discovery must stay at zero when discovery fails: %+v", attrs)
		}
		requireNonNegativeInt64(t, attrs, "discoveryUs")
	})

	t.Run("open failure: discovery succeeded, open failed", func(t *testing.T) {
		handler := &timingRecordingHandler{}
		ops := fakeNativeAttachOps(nil, errors.New("fake open failure"), nil, 0, 0)
		_, err := attachViaIOCTLWithOps(meta, 3241, slog.New(handler), ops)
		if err == nil {
			t.Fatal("open failure must be reported")
		}
		attrs := requireSingleTimingRecord(t, handler)
		if attrs["backendCalled"] != false {
			t.Fatalf("backendCalled = %v, want false (DeviceIoControl must never run after an open failure)", attrs["backendCalled"])
		}
		if attrs["ioctlUs"] != int64(0) || attrs["validationUs"] != int64(0) {
			t.Fatalf("stages after open must stay at zero when open fails: %+v", attrs)
		}
		requireNonNegativeInt64(t, attrs, "discoveryUs")
		requireNonNegativeInt64(t, attrs, "openUs")
	})

	t.Run("IOCTL failure classifies as unsafe-outcome-unknown and reachedIOCTL=true", func(t *testing.T) {
		handler := &timingRecordingHandler{}
		ops := fakeNativeAttachOps(nil, nil, errors.New("fake DeviceIoControl failure"), 0, 0)
		_, err := attachViaIOCTLWithOps(meta, 3241, slog.New(handler), ops)
		if !errors.Is(err, ErrAttachmentOutcomeUnknown) {
			t.Fatalf("err = %v, want ErrAttachmentOutcomeUnknown", err)
		}
		attrs := requireSingleTimingRecord(t, handler)
		if attrs["backendCalled"] != true || attrs["result"] != "unsafe-outcome-unknown" {
			t.Fatalf("unexpected timing attrs: %+v", attrs)
		}
		if attrs["validationUs"] != int64(0) {
			t.Fatalf("validation must never run after a failed IOCTL: %+v", attrs)
		}
		requireNonNegativeInt64(t, attrs, "ioctlUs")
	})

	t.Run("success: every stage measured, result success", func(t *testing.T) {
		handler := &timingRecordingHandler{}
		ops := fakeNativeAttachOps(nil, nil, nil, 55, attachPortOutputLength)
		attachment, err := attachViaIOCTLWithOps(meta, 3241, slog.New(handler), ops)
		if err != nil || attachment.Port != 55 || attachment.Backend != LocalhostAttachmentBackendNativeIOCTL {
			t.Fatalf("attachment=%+v err=%v", attachment, err)
		}
		attrs := requireSingleTimingRecord(t, handler)
		if attrs["result"] != "success" || attrs["backendCalled"] != true {
			t.Fatalf("unexpected timing attrs: %+v", attrs)
		}
		for _, key := range []string{"totalUs", "discoveryUs", "openUs", "ioctlUs", "validationUs"} {
			requireNonNegativeInt64(t, attrs, key)
		}
	})
}

func TestAttachViaCommandWithRunnerTimingEmitsProcessAndClassificationFields(t *testing.T) {
	meta := &usbip.ExportMeta{BusID: 1, DevID: 2}

	t.Run("success", func(t *testing.T) {
		handler := &timingRecordingHandler{}
		run := func(context.Context, string, ...string) ([]byte, error) { return []byte("21\n"), nil }
		attachment, err := attachViaCommandWithRunner(context.Background(), meta, 3241, slog.New(handler), run)
		if err != nil || attachment.Port != 21 {
			t.Fatalf("attachment=%+v err=%v", attachment, err)
		}
		attrs := requireSingleTimingRecord(t, handler)
		if attrs["operation"] != "attach" || attrs["layer"] != "command" || attrs["result"] != "success" || attrs["backendCalled"] != true {
			t.Fatalf("unexpected timing attrs: %+v", attrs)
		}
		for _, key := range []string{"totalUs", "processUs", "classificationUs"} {
			requireNonNegativeInt64(t, attrs, key)
		}
	})

	t.Run("process start failure is a known failure, backendCalled still true", func(t *testing.T) {
		handler := &timingRecordingHandler{}
		startErr := &exec.Error{Name: "usbip", Err: errors.New("not found")}
		run := func(context.Context, string, ...string) ([]byte, error) { return nil, startErr }
		_, err := attachViaCommandWithRunner(context.Background(), meta, 3241, slog.New(handler), run)
		if err == nil || errors.Is(err, ErrAttachmentOutcomeUnknown) {
			t.Fatalf("err = %v, want a known (non-outcome-unknown) failure", err)
		}
		attrs := requireSingleTimingRecord(t, handler)
		if attrs["backendCalled"] != true || attrs["result"] != "retryable-failure" {
			t.Fatalf("unexpected timing attrs: %+v", attrs)
		}
	})
}

func TestDetachViaIOCTLWithOpsTimingRejectsInvalidPortBeforeDiscovery(t *testing.T) {
	handler := &timingRecordingHandler{}
	ops := fakeNativeAttachOps(nil, nil, nil, 0, 0) // Unused: validation fails before any op runs.

	err := detachViaIOCTLWithOps(0, slog.New(handler), fakeNativeDetachOps(ops))
	if err == nil {
		t.Fatal("port 0 must be rejected")
	}
	attrs := requireSingleTimingRecord(t, handler)
	if attrs["operation"] != "detach" || attrs["layer"] != "native-ioctl" || attrs["backendCalled"] != false {
		t.Fatalf("unexpected timing attrs: %+v", attrs)
	}
	if attrs["discoveryUs"] != int64(0) || attrs["openUs"] != int64(0) || attrs["ioctlUs"] != int64(0) {
		t.Fatalf("stages after validation must stay at zero when validation fails: %+v", attrs)
	}
	requireNonNegativeInt64(t, attrs, "validationUs")
}

func TestDetachViaIOCTLWithOpsTimingSuccess(t *testing.T) {
	handler := &timingRecordingHandler{}
	ops := nativeDetachOps{
		discoverDevicePath: func() (string, error) { return "fake-device-path", nil },
		openDevice:         func(string) (windows.Handle, error) { return windows.Handle(1), nil },
		closeDevice:        func(windows.Handle) error { return nil },
		plugoutHardware:    func(windows.Handle, *plugoutIOCTL) (uint32, error) { return 0, nil },
	}
	if err := detachViaIOCTLWithOps(37, slog.New(handler), ops); err != nil {
		t.Fatalf("detach failed: %v", err)
	}
	attrs := requireSingleTimingRecord(t, handler)
	if attrs["result"] != "success" || attrs["backendCalled"] != true {
		t.Fatalf("unexpected timing attrs: %+v", attrs)
	}
	for _, key := range []string{"totalUs", "validationUs", "discoveryUs", "openUs", "ioctlUs"} {
		requireNonNegativeInt64(t, attrs, key)
	}
}

func TestDetachViaCommandWithRunnerTimingEmitsProcessFields(t *testing.T) {
	handler := &timingRecordingHandler{}
	run := func(context.Context, string, ...string) ([]byte, error) { return []byte("ok\n"), nil }

	if err := detachViaCommandWithRunner(context.Background(), 37, slog.New(handler), run); err != nil {
		t.Fatalf("detach failed: %v", err)
	}
	attrs := requireSingleTimingRecord(t, handler)
	if attrs["operation"] != "detach" || attrs["layer"] != "command" || attrs["result"] != "success" || attrs["backendCalled"] != true {
		t.Fatalf("unexpected timing attrs: %+v", attrs)
	}
	requireNonNegativeInt64(t, attrs, "processUs")
}

// fakeNativeDetachOps adapts a nativeAttachOps' discover/open/close fakes for a detach test that
// only needs to prove validation runs before any of them; plugoutHardware is never reachable in
// that path so it can be a stub.
func fakeNativeDetachOps(attach nativeAttachOps) nativeDetachOps {
	return nativeDetachOps{
		discoverDevicePath: attach.discoverDevicePath,
		openDevice:         attach.openDevice,
		closeDevice:        attach.closeDevice,
		plugoutHardware:    func(windows.Handle, *plugoutIOCTL) (uint32, error) { return 0, nil },
	}
}

func requireSingleTimingRecord(t *testing.T, handler *timingRecordingHandler) map[string]any {
	t.Helper()
	records := handler.timingRecords()
	if len(records) != 1 {
		t.Fatalf("timing records = %d, want exactly 1", len(records))
	}
	attrs := recordAttrs(records[0])
	if attrs["operation"] == "attach" && attrs["layer"] == "native-ioctl" {
		if _, ok := attrs["installedVersion"].(string); !ok {
			t.Fatalf("native attach timing record is missing installedVersion: %+v", attrs)
		}
		if _, ok := attrs["selectedABI"].(string); !ok {
			t.Fatalf("native attach timing record is missing selectedABI: %+v", attrs)
		}
		if _, ok := attrs["inputLength"].(uint64); !ok {
			t.Fatalf("native attach timing record is missing inputLength: %+v", attrs)
		}
	}
	return attrs
}

func TestPlugoutIOCTLTargetsOnlyPositivePort(t *testing.T) {
	request, err := newPlugoutIOCTL(37)
	if err != nil {
		t.Fatal(err)
	}
	if request.Port != 37 || request.Size != uint32(unsafe.Sizeof(request)) {
		t.Fatalf("plugout request = %+v", request)
	}
	for _, port := range []int32{0, -1, -2} {
		if _, err := newPlugoutIOCTL(port); err == nil {
			t.Fatalf("plugout accepted unsafe port %d", port)
		}
	}
}
