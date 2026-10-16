package dualsense

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

func TestWriteUSBInputReportReuseAndSequence(t *testing.T) {
	d := &DualSense{timestampBase: time.Now().Add(-time.Second)}
	meta := &MetaState{BatteryStatus: 0x5a}
	active := &InputState{
		LX: -128, LY: 127, RX: 0, RY: -1,
		Buttons: ButtonCross | ButtonPS | ButtonEdgeL4 | ButtonEdgeR4,
		DPad:    DPadUp | DPadLeft,
		L2:      0x12, R2: 0x34,
		Touch1X: 0x0123, Touch1Y: 0x0345, Touch1Active: true,
		Touch2X: 0xffff, Touch2Y: 0x0432,
		GyroX: -1234, GyroY: 0x2345, GyroZ: -1,
		AccelX: 0x1234, AccelY: -2345, AccelZ: -8192,
	}
	wantActive := []byte{
		0x01, 0x00, 0xff, 0x80, 0x7f, 0x12, 0x34, 0x01,
		0x27, 0x00, 0xc1, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x2e, 0xfb, 0x45, 0x23, 0xff, 0xff, 0x34, 0x12,
		0xd7, 0xf6, 0x00, 0xe0, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x23, 0x51, 0x34, 0x80, 0x80, 0x27,
		0x43, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x10, 0x00, 0x00, 0x00, 0x5a, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}

	dst := bytes.Repeat([]byte{0xcc}, InputReportSize+5)
	suffix := append([]byte(nil), dst[InputReportSize:]...)
	writeAndCheckTimestamp(t, d, dst, active, meta)
	wantActive[7] = 0x01
	copy(wantActive[28:32], dst[28:32])
	if !bytes.Equal(dst[:InputReportSize], wantActive) {
		t.Fatalf("active report = % x, want % x", dst[:InputReportSize], wantActive)
	}
	if !bytes.Equal(dst[InputReportSize:], suffix) {
		t.Fatalf("writer changed destination suffix: got % x, want % x", dst[InputReportSize:], suffix)
	}

	neutral := &InputState{}
	neutralMeta := &MetaState{}
	neutralWant := make([]byte, InputReportSize)
	neutralWant[0] = ReportIDInput
	neutralWant[1], neutralWant[2], neutralWant[3], neutralWant[4] = 128, 128, 128, 128
	neutralWant[7] = 0x02
	neutralWant[8] = DPadUSBNeutral
	neutralWant[33] = TouchInactiveMask
	neutralWant[37] = TouchInactiveMask
	neutralWant[49] = 0x10
	writeAndCheckTimestamp(t, d, dst, neutral, neutralMeta)
	copy(neutralWant[28:32], dst[28:32])
	if !bytes.Equal(dst[:InputReportSize], neutralWant) {
		t.Fatalf("neutral report retained stale data: got % x, want % x", dst[:InputReportSize], neutralWant)
	}

	writeAndCheckTimestamp(t, d, dst, active, meta)
	wantActive[7] = 0x03
	copy(wantActive[28:32], dst[28:32])
	if !bytes.Equal(dst[:InputReportSize], wantActive) {
		t.Fatalf("active report after reuse = % x, want % x", dst[:InputReportSize], wantActive)
	}
	if !bytes.Equal(dst[InputReportSize:], suffix) {
		t.Fatalf("writer changed destination suffix after reuse: got % x, want % x", dst[InputReportSize:], suffix)
	}

	first := d.buildUSBInputReport(active, meta)
	firstSnapshot := append([]byte(nil), first...)
	firstSequence := first[7]
	second := d.buildUSBInputReport(neutral, neutralMeta)
	if len(first) != InputReportSize || len(second) != InputReportSize {
		t.Fatalf("wrapper report lengths = %d and %d, want %d", len(first), len(second), InputReportSize)
	}
	if &first[0] == &second[0] {
		t.Fatal("buildUSBInputReport returned the same backing storage twice")
	}
	if firstSequence != 0x04 || second[7] != 0x05 {
		t.Fatalf("wrapper sequences = %d then %d, want 4 then 5", firstSequence, second[7])
	}
	if !bytes.Equal(first, firstSnapshot) {
		t.Fatalf("first wrapper report changed after second call: got % x, want % x", first, firstSnapshot)
	}
}

func writeAndCheckTimestamp(t *testing.T, d *DualSense, dst []byte, state *InputState, meta *MetaState) {
	t.Helper()
	before := uint64(time.Since(d.timestampBase).Microseconds()) * 3
	d.writeUSBInputReport(dst, state, meta)
	after := uint64(time.Since(d.timestampBase).Microseconds()) * 3
	got := uint64(binary.LittleEndian.Uint32(dst[28:32]))
	if got < before || got > after {
		t.Fatalf("timestamp = %d, outside encoder call interval [%d, %d]", got, before, after)
	}
}

func TestInPlaceWriterIsSharedByDualSenseAndEdge(t *testing.T) {
	normal, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	edge, err := NewEdge(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := normal.GetDescriptor().Device.IDProduct; got != DefaultPIDDS {
		t.Fatalf("normal DualSense PID = %#04x, want %#04x", got, DefaultPIDDS)
	}
	if got := edge.GetDescriptor().Device.IDProduct; got != DefaultPIDDSEdge {
		t.Fatalf("DualSense Edge PID = %#04x, want %#04x", got, DefaultPIDDSEdge)
	}

	state := NewInputState()
	meta := &MetaState{BatteryStatus: BatteryFullyCharged}
	normalReport := normal.buildUSBInputReport(state, meta)
	edgeReport := edge.buildUSBInputReport(state, meta)
	if len(normalReport) != InputReportSize || len(edgeReport) != InputReportSize {
		t.Fatalf("normal/Edge report lengths = %d/%d, want %d", len(normalReport), len(edgeReport), InputReportSize)
	}
	for _, report := range [][]byte{normalReport, edgeReport} {
		clear(report[28:32]) // Timestamp is sampled from each device's live clock.
	}
	if !bytes.Equal(normalReport, edgeReport) {
		t.Fatalf("shared normal/Edge reports differ outside timestamp: normal % x, Edge % x", normalReport, edgeReport)
	}
}
