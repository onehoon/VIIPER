package xbox360

import (
	"bytes"
	"testing"
)

func TestWriteReportReuseClearsExactlyOneReport(t *testing.T) {
	active := &InputState{
		Buttons:  0xdeadbeef,
		LT:       0x00,
		RT:       0xff,
		LX:       -32768,
		LY:       -1,
		RX:       1,
		RY:       32767,
		Reserved: [6]byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66},
	}
	wantActive := []byte{
		0x00, 0x14, 0xef, 0xbe, 0x00, 0xff, 0x00, 0x80, 0xff, 0xff,
		0x01, 0x00, 0xff, 0x7f, 0x11, 0x22, 0x33, 0x44, 0x55, 0x00,
	}

	dst := bytes.Repeat([]byte{0xa5}, xbox360InputReportLen+4)
	suffix := append([]byte(nil), dst[xbox360InputReportLen:]...)
	active.writeReport(dst)
	if !bytes.Equal(dst[:xbox360InputReportLen], wantActive) {
		t.Fatalf("active report = % x, want % x", dst[:xbox360InputReportLen], wantActive)
	}
	if !bytes.Equal(dst[xbox360InputReportLen:], suffix) {
		t.Fatalf("writer changed destination suffix: got % x, want % x", dst[xbox360InputReportLen:], suffix)
	}

	neutral := &InputState{}
	wantNeutral := make([]byte, xbox360InputReportLen)
	wantNeutral[1] = 0x14
	neutral.writeReport(dst)
	if !bytes.Equal(dst[:xbox360InputReportLen], wantNeutral) {
		t.Fatalf("neutral report retained stale data: got % x, want % x", dst[:xbox360InputReportLen], wantNeutral)
	}

	active.writeReport(dst)
	if !bytes.Equal(dst[:xbox360InputReportLen], wantActive) {
		t.Fatalf("active report after reuse = % x, want % x", dst[:xbox360InputReportLen], wantActive)
	}
	if dst[19] != 0 {
		t.Fatalf("final report byte = %#02x, want zero; the sixth Reserved byte is not serialized", dst[19])
	}
	if !bytes.Equal(dst[xbox360InputReportLen:], suffix) {
		t.Fatalf("writer changed destination suffix after reuse: got % x, want % x", dst[xbox360InputReportLen:], suffix)
	}
}

func TestBuildReportReturnsIndependentAllocation(t *testing.T) {
	state := &InputState{Buttons: 0xdeadbeef, LX: -32768, Reserved: [6]byte{1, 2, 3, 4, 5, 6}}
	first := state.BuildReport()
	wantFirst := append([]byte(nil), first...)
	state.Buttons = 0
	state.LX = 0
	state.Reserved = [6]byte{}
	second := state.BuildReport()

	if len(first) != xbox360InputReportLen || len(second) != xbox360InputReportLen {
		t.Fatalf("report lengths = %d and %d, want %d", len(first), len(second), xbox360InputReportLen)
	}
	if &first[0] == &second[0] {
		t.Fatal("BuildReport returned the same backing storage twice")
	}
	if !bytes.Equal(first, wantFirst) {
		t.Fatalf("first report changed after second call: got % x, want % x", first, wantFirst)
	}
}
