//go:build windows

package api

import (
	"testing"
	"unsafe"
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
