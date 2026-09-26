package main

import "testing"

func TestUSBServerConfigTransportEnumsAndDefaults(t *testing.T) {
	tests := []struct {
		name        string
		receiveMode uint32
		inMode      uint32
		wantReceive uint32
		wantAsync   bool
		wantValid   bool
	}{
		{name: "zero initialized", wantReceive: 0, wantAsync: false, wantValid: true},
		{name: "zero-copy async", receiveMode: 0, inMode: 1, wantReceive: 0, wantAsync: true, wantValid: true},
		{name: "low-latency sequential", receiveMode: 1, inMode: 0, wantReceive: 1, wantAsync: false, wantValid: true},
		{name: "optimized opt-in", receiveMode: 1, inMode: 1, wantReceive: 1, wantAsync: true, wantValid: true},
		{name: "unknown receive mode", receiveMode: 2, wantValid: false},
		{name: "unknown IN mode", inMode: 2, wantValid: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseUSBServerTransportPolicy(tt.receiveMode, tt.inMode)
			if ok != tt.wantValid {
				t.Fatalf("valid = %t, want %t", ok, tt.wantValid)
			}
			if !ok {
				return
			}
			if uint32(got.receiveMode) != tt.wantReceive || got.asyncNonEp0IN != tt.wantAsync {
				t.Fatalf("policy = {receive:%d async:%t}, want {receive:%d async:%t}", got.receiveMode, got.asyncNonEp0IN, tt.wantReceive, tt.wantAsync)
			}
		})
	}
}

func TestUSBServerConfigX64ABI(t *testing.T) {
	layout := currentUSBServerConfigLayout()
	want := usbServerConfigLayout{
		size:                    40,
		addr:                    0,
		connectionTimeout:       8,
		deviceHandlerTimeout:    16,
		writeBatchFlushInterval: 24,
		usbipReceiveMode:        28,
		nonEp0InMode:            32,
		receiveZeroCopy:         0,
		receiveLowLatency:       1,
		nonEp0Sequential:        0,
		nonEp0Async:             1,
	}
	if layout != want {
		t.Fatalf("USBServerConfig ABI layout = %+v, want %+v", layout, want)
	}
}
