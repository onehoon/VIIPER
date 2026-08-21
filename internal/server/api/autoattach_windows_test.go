//go:build windows

package api

import (
	"context"
	"errors"
	"log/slog"
	"os/exec"
	"testing"
	"unsafe"

	"github.com/Alia5/VIIPER/usbip"
	"golang.org/x/sys/windows"
)

func TestUSBIPWin2977NativeABIContract(t *testing.T) {
	var request attachIOCTL
	if got, want := unsafe.Offsetof(request.PortOutput), uintptr(4); got != want {
		t.Fatalf("plugin_hardware port offset = %d, want %d", got, want)
	}
	if got, want := attachPortOutputLength, uint32(8); got != want {
		t.Fatalf("plugin_hardware output length = %d, want %d", got, want)
	}
	if got, want := attachInputLength, uint32(1100); got != want {
		t.Fatalf("plugin_hardware input length = %d, want %d", got, want)
	}
	inputLength, outputLength := nativeAttachIOCTLLengths()
	if inputLength != attachInputLength || outputLength != attachPortOutputLength {
		t.Fatalf("native attach lengths = input %d output %d, want input %d output %d", inputLength, outputLength, attachInputLength, attachPortOutputLength)
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
	if got, want := unsafe.Sizeof(plugoutIOCTL{}), uintptr(8); got != want {
		t.Fatalf("plugout_hardware size = %d, want %d", got, want)
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

func TestAttachViaIOCTLUsesIPv4LoopbackEndpoint(t *testing.T) {
	meta := &usbip.ExportMeta{BusID: 1, DevID: 2}
	var host [niMaxHost]byte
	ops := nativeAttachOps{
		discoverDevicePath: func() (string, error) { return "fake-device-path", nil },
		openDevice:         func(string) (windows.Handle, error) { return windows.Handle(1), nil },
		closeDevice:        func(windows.Handle) error { return nil },
		pluginHardware: func(_ windows.Handle, data *attachIOCTL) (uint32, error) {
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

// These tests exercise the native-ioctl/command breakdown timing entirely through the
// nativeAttachOps/nativeDetachOps/commandRunner fake seams -- they never touch the real
// usbip-win2 driver, SetupAPI, DeviceIoControl, or a real usbip.exe process, and never skip.

func fakeNativeAttachOps(discoverErr, openErr, ioctlErr error, portOutput int32, bytesReturned uint32) nativeAttachOps {
	return nativeAttachOps{
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
		pluginHardware: func(_ windows.Handle, data *attachIOCTL) (uint32, error) {
			if ioctlErr != nil {
				return 0, ioctlErr
			}
			data.PortOutput = portOutput
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
	return recordAttrs(records[0])
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
