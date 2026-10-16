package steamdeck

import (
	"bytes"
	"testing"
)

func TestWriteReportReuseClearsExactlyOneReport(t *testing.T) {
	active := &InputState{
		A: true, X: true, B: true, Y: true, L1: true, R1: true, L2Digital: true, R2Digital: true,
		L5: true, Menu: true, Steam: true, Options: true, DPadDown: true, DPadLeft: true, DPadRight: true, DPadUp: true,
		L3: true, RPadTouch: true, LPadTouch: true, RPadPress: true, LPadPress: true, R5: true, R3: true,
		RStickTouch: true, LStickTouch: true, R4: true, L4: true, QuickAccess: true,
		Reserved15: 0xa5,
		LPadX:      -32768, LPadY: -1, RPadX: 0x1234, RPadY: 32767,
		AccelX: -1, AccelY: 0x2345, AccelZ: -32768,
		Pitch: 1, Yaw: -2, Roll: 0x3456,
		GyroQuatW: 0x4567, GyroQuatX: -1, GyroQuatY: 0x6789, GyroQuatZ: -32768,
		LTrigger: 0, RTrigger: 0xffff,
		LStickX: -1, LStickY: -32768, RStickX: 0x2345, RStickY: 32767,
		LPadForce: 0x1112, RPadForce: 0x3334, LStickForce: 0x5556, RStickForce: 0x7778,
		Frame: 0xdeadbeef,
	}
	frame := uint32(0x78563412)
	wantActive := []byte{
		0x01, 0x00, 0x09, 0x40, 0x12, 0x34, 0x56, 0x78,
		0xff, 0xff, 0x5f, 0x04, 0x00, 0xc6, 0x04, 0xa5,
		0x00, 0x80, 0xff, 0xff, 0x34, 0x12, 0xff, 0x7f,
		0xff, 0xff, 0x45, 0x23, 0x00, 0x80, 0x01, 0x00,
		0xfe, 0xff, 0x56, 0x34, 0x67, 0x45, 0xff, 0xff,
		0x89, 0x67, 0x00, 0x80, 0x00, 0x00, 0xff, 0xff,
		0xff, 0xff, 0x00, 0x80, 0x45, 0x23, 0xff, 0x7f,
		0x12, 0x11, 0x34, 0x33, 0x56, 0x55, 0x78, 0x77,
	}

	dst := bytes.Repeat([]byte{0xa5}, InputReportLen+5)
	suffix := append([]byte(nil), dst[InputReportLen:]...)
	active.writeReport(dst, frame)
	if !bytes.Equal(dst[:InputReportLen], wantActive) {
		t.Fatalf("active report = % x, want % x", dst[:InputReportLen], wantActive)
	}
	if !bytes.Equal(dst[InputReportLen:], suffix) {
		t.Fatalf("writer changed destination suffix: got % x, want % x", dst[InputReportLen:], suffix)
	}

	neutral := &InputState{}
	wantNeutral := make([]byte, InputReportLen)
	wantNeutral[0] = 0x01
	wantNeutral[2] = InputReportID
	wantNeutral[3] = InputReportLen
	neutral.writeReport(dst, 0)
	if !bytes.Equal(dst[:InputReportLen], wantNeutral) {
		t.Fatalf("neutral report retained stale data: got % x, want % x", dst[:InputReportLen], wantNeutral)
	}

	active.writeReport(dst, frame)
	if !bytes.Equal(dst[:InputReportLen], wantActive) {
		t.Fatalf("active report after reuse = % x, want % x", dst[:InputReportLen], wantActive)
	}
	if !bytes.Equal(dst[InputReportLen:], suffix) {
		t.Fatalf("writer changed destination suffix after reuse: got % x, want % x", dst[InputReportLen:], suffix)
	}
}

func TestBuildReportReturnsIndependentAllocation(t *testing.T) {
	state := &InputState{A: true, LPadX: -1234, LStickForce: 0x1234}
	first := state.buildReport(0x01020304)
	wantFirst := append([]byte(nil), first...)
	*state = InputState{R3: true, RStickForce: 0xabcd}
	second := state.buildReport(0x05060708)

	if len(first) != InputReportLen || len(second) != InputReportLen {
		t.Fatalf("report lengths = %d and %d, want %d", len(first), len(second), InputReportLen)
	}
	if &first[0] == &second[0] {
		t.Fatal("buildReport returned the same backing storage twice")
	}
	if !bytes.Equal(first, wantFirst) {
		t.Fatalf("first report changed after second call: got % x, want % x", first, wantFirst)
	}
}
