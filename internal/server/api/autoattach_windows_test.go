//go:build windows

package api

import (
	"context"
	"log/slog"
	"testing"
	"unsafe"

	"github.com/Alia5/VIIPER/usbip"
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

// These tests exercise the native-ioctl/command breakdown timing on this test machine's real
// (absent) usbip-win2 driver and usbip.exe binary -- both are expected to fail deterministically
// in CI/dev environments without hardware, which is exactly the path that lets us assert on
// timing-field presence and non-negativity without any sleep or magnitude dependency. They do
// not require or claim hardware validation.

func TestAttachViaIOCTLTimingBreaksDownDiscoveryFailure(t *testing.T) {
	handler := &timingRecordingHandler{}
	logger := slog.New(handler)
	meta := &usbip.ExportMeta{BusID: 1, DevID: 2}

	_, err := attachViaIOCTL(context.Background(), meta, 3241, logger)
	if err == nil {
		t.Skip("usbip-win2 driver is present on this machine; discovery-failure path not exercised")
	}

	records := handler.timingRecords()
	if len(records) != 1 {
		t.Fatalf("timing records = %d, want exactly 1", len(records))
	}
	attrs := recordAttrs(records[0])
	if attrs["operation"] != "attach" || attrs["layer"] != "native-ioctl" {
		t.Fatalf("unexpected timing attrs: %+v", attrs)
	}
	if attrs["backendCalled"] != false {
		t.Fatalf("backendCalled = %v, want false (DeviceIoControl must never run after a discovery failure)", attrs["backendCalled"])
	}
	for _, key := range []string{"totalUs", "discoveryUs", "openUs", "ioctlUs", "validationUs"} {
		requireNonNegativeInt64(t, attrs, key)
	}
	if attrs["openUs"] != int64(0) || attrs["ioctlUs"] != int64(0) || attrs["validationUs"] != int64(0) {
		t.Fatalf("stages after discovery must stay at zero when discovery fails: %+v", attrs)
	}
}

func TestAttachViaCommandTimingEmitsProcessAndClassificationFields(t *testing.T) {
	handler := &timingRecordingHandler{}
	logger := slog.New(handler)
	meta := &usbip.ExportMeta{BusID: 1, DevID: 2}

	// usbip.exe is not expected to be present/reachable in this environment; either way exactly
	// one timing summary must be emitted with backendCalled=true, since CombinedOutput always
	// runs regardless of outcome.
	_, _ = attachViaCommand(context.Background(), meta, 3241, logger)

	records := handler.timingRecords()
	if len(records) != 1 {
		t.Fatalf("timing records = %d, want exactly 1", len(records))
	}
	attrs := recordAttrs(records[0])
	if attrs["operation"] != "attach" || attrs["layer"] != "command" {
		t.Fatalf("unexpected timing attrs: %+v", attrs)
	}
	if attrs["backendCalled"] != true {
		t.Fatalf("backendCalled = %v, want true", attrs["backendCalled"])
	}
	for _, key := range []string{"totalUs", "processUs", "classificationUs"} {
		requireNonNegativeInt64(t, attrs, key)
	}
}

func TestDetachViaIOCTLTimingRejectsInvalidPortBeforeDiscovery(t *testing.T) {
	handler := &timingRecordingHandler{}
	logger := slog.New(handler)

	err := detachViaIOCTL(context.Background(), 0, logger)
	if err == nil {
		t.Fatal("port 0 must be rejected")
	}

	records := handler.timingRecords()
	if len(records) != 1 {
		t.Fatalf("timing records = %d, want exactly 1", len(records))
	}
	attrs := recordAttrs(records[0])
	if attrs["operation"] != "detach" || attrs["layer"] != "native-ioctl" {
		t.Fatalf("unexpected timing attrs: %+v", attrs)
	}
	if attrs["backendCalled"] != false {
		t.Fatalf("backendCalled = %v, want false (discovery/IOCTL must never run for an invalid port)", attrs["backendCalled"])
	}
	if attrs["discoveryUs"] != int64(0) || attrs["openUs"] != int64(0) || attrs["ioctlUs"] != int64(0) {
		t.Fatalf("stages after validation must stay at zero when validation fails: %+v", attrs)
	}
	requireNonNegativeInt64(t, attrs, "validationUs")
}

func TestDetachViaCommandTimingEmitsProcessFields(t *testing.T) {
	handler := &timingRecordingHandler{}
	logger := slog.New(handler)

	// A valid port passes argument validation, so the command always actually runs (backendCalled
	// must be true) regardless of whether usbip.exe is present in this environment.
	_ = detachViaCommand(context.Background(), 37, logger)

	records := handler.timingRecords()
	if len(records) != 1 {
		t.Fatalf("timing records = %d, want exactly 1", len(records))
	}
	attrs := recordAttrs(records[0])
	if attrs["operation"] != "detach" || attrs["layer"] != "command" {
		t.Fatalf("unexpected timing attrs: %+v", attrs)
	}
	if attrs["backendCalled"] != true {
		t.Fatalf("backendCalled = %v, want true", attrs["backendCalled"])
	}
	requireNonNegativeInt64(t, attrs, "processUs")
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
