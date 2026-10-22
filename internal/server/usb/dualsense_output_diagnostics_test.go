package usb

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	deviceusb "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/Alia5/VIIPER/virtualbus"
)

func TestIsDualSenseEdgeDescriptor(t *testing.T) {
	tests := []struct {
		name string
		desc *deviceusb.Descriptor
		want bool
	}{
		{name: "edge", desc: &deviceusb.Descriptor{Strings: map[uint8]string{2: dualSenseEdgeProductName}}, want: true},
		{name: "edge with identity override", desc: &deviceusb.Descriptor{Device: deviceusb.DeviceDescriptor{IDProduct: 0xffff}, Strings: map[uint8]string{2: dualSenseEdgeProductName}}, want: true},
		{name: "base DualSense", desc: &deviceusb.Descriptor{Strings: map[uint8]string{2: "DualSense Wireless Controller"}}},
		{name: "other controller", desc: &deviceusb.Descriptor{Strings: map[uint8]string{2: "Xbox 360 Controller"}}},
		{name: "Steam Deck", desc: &deviceusb.Descriptor{Strings: map[uint8]string{2: "Steam Deck"}}},
		{name: "unrelated HID", desc: &deviceusb.Descriptor{Strings: map[uint8]string{2: "USB HID Device"}}},
		{name: "missing product", desc: &deviceusb.Descriptor{}},
		{name: "nil descriptor"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDualSenseEdgeDescriptor(tt.desc); got != tt.want {
				t.Fatalf("isDualSenseEdgeDescriptor() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDualSenseOutputSourceForTransfer(t *testing.T) {
	payload := dualSenseTestOutputReport(48)
	controlSetup := []byte{0x21, 0x09, 0x02, 0x02, 0x00, 0x00, 0x30, 0x00}
	wrongReportID := append([]byte(nil), payload...)
	wrongReportID[0] = 0x03

	tests := []struct {
		name    string
		dir     uint32
		ep      uint32
		setup   []byte
		payload []byte
		want    dualSenseOutputSource
		ok      bool
	}{
		{name: "interrupt OUT EP3", dir: usbip.DirOut, ep: 3, payload: payload, want: dualSenseOutputSourceInterruptOUT, ok: true},
		{name: "wrong interrupt report ID", dir: usbip.DirOut, ep: 3, payload: wrongReportID},
		{name: "control output SET_REPORT", dir: usbip.DirOut, setup: controlSetup, payload: payload, want: dualSenseOutputSourceControlSetReport, ok: true},
		{name: "empty control payload is retained as malformed", dir: usbip.DirOut, setup: controlSetup, want: dualSenseOutputSourceControlSetReport, ok: true},
		{name: "wrong control payload report ID", dir: usbip.DirOut, setup: controlSetup, payload: wrongReportID},
		{name: "feature SET_REPORT", dir: usbip.DirOut, setup: []byte{0x21, 0x09, 0x03, 0x02, 0, 0, 0x30, 0}, payload: payload},
		{name: "wrong request", dir: usbip.DirOut, setup: []byte{0x21, 0x08, 0x02, 0x02, 0, 0, 0x30, 0}, payload: payload},
		{name: "short setup", dir: usbip.DirOut, setup: controlSetup[:7], payload: payload},
		{name: "other endpoint", dir: usbip.DirOut, ep: 2, setup: controlSetup, payload: payload},
		{name: "IN endpoint", dir: usbip.DirIn, ep: 3, payload: payload},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := dualSenseOutputSourceForTransfer(tt.dir, tt.ep, tt.setup, tt.payload)
			if ok != tt.ok || (ok && got != tt.want) {
				t.Fatalf("dualSenseOutputSourceForTransfer() = (%v, %v), want (%v, %v)", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestClassifyDualSenseOutput(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]byte)
		want   dualSenseOutputClass
	}{
		{name: "neutral candidate", want: dualSenseOutputClassExactNeutralCandidate},
		{name: "V1 flag", mutate: func(report []byte) { report[1] = 0x01 }, want: dualSenseOutputClassParserEligibleV1},
		{name: "V2 flag", mutate: func(report []byte) { report[39] = 0x04 }, want: dualSenseOutputClassParserEligibleV2},
		{name: "selector-only flags", mutate: func(report []byte) { report[1], report[39] = 0x02, 0x08 }, want: dualSenseOutputClassOtherOutput},
		{name: "right trigger update", mutate: func(report []byte) { report[1] = 0x04 }, want: dualSenseOutputClassTriggerUpdateFlagPresent},
		{name: "left trigger update", mutate: func(report []byte) { report[1] = 0x08 }, want: dualSenseOutputClassTriggerUpdateFlagPresent},
		{name: "both trigger updates", mutate: func(report []byte) { report[1] = 0x0c }, want: dualSenseOutputClassTriggerUpdateFlagPresent},
		{name: "trigger plus rumble", mutate: func(report []byte) { report[1] = 0x05 }, want: dualSenseOutputClassParserEligibleV1 | dualSenseOutputClassTriggerUpdateFlagPresent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := dualSenseTestOutputReport(64)
			if tt.mutate != nil {
				tt.mutate(report)
			}
			if got := classifyDualSenseOutput(report); got != tt.want {
				t.Fatalf("classifyDualSenseOutput() = %06b, want %06b", got, tt.want)
			}
		})
	}

	if got := classifyDualSenseOutput(nil); got != dualSenseOutputClassInvalidOrShort {
		t.Fatalf("empty payload class = %v, want InvalidOrShort", got)
	}
	if got := classifyDualSenseOutput(dualSenseTestOutputReport(47)); got != dualSenseOutputClassInvalidOrShort {
		t.Fatalf("47-byte payload class = %v, want InvalidOrShort", got)
	}
	wrongID := dualSenseTestOutputReport(64)
	wrongID[0] = 0x03
	if got := classifyDualSenseOutput(wrongID); got != dualSenseOutputClassInvalidOrShort {
		t.Fatalf("wrong report ID class = %v, want InvalidOrShort", got)
	}
}

func TestDualSenseOutputTracePreservesRawPayloadAndTriggerBlocks(t *testing.T) {
	for _, length := range []int{48, 64} {
		t.Run(fmt.Sprintf("length_%d", length), func(t *testing.T) {
			var output bytes.Buffer
			diagnostics := newDualSenseOutputDiagnostics(slog.New(slog.NewTextHandler(&output, nil)))
			report := dualSenseTestOutputReport(length)
			report[1], report[2], report[3], report[4], report[39] = 0x0d, 0xa5, 0x12, 0x34, 0x0c
			for i := 0; i < 11; i++ {
				report[11+i] = byte(0x10 + i)
				report[22+i] = byte(0x30 + i)
			}
			diagnostics.observeAt(dualSenseOutputSourceInterruptOUT, report, time.Unix(100, 0))

			log := output.String()
			if !strings.Contains(log, "source=InterruptOUT") || !strings.Contains(log, "class=ParserEligibleV1,ParserEligibleV2,TriggerUpdateFlagPresent") {
				t.Fatalf("log missing ingress/classification: %s", log)
			}
			if !strings.Contains(log, "rawHex="+hex.EncodeToString(report)) {
				t.Fatalf("log does not preserve all %d original bytes: %s", length, log)
			}
			if !strings.Contains(log, "triggerRightHex="+hex.EncodeToString(report[11:22])) || !strings.Contains(log, "triggerLeftHex="+hex.EncodeToString(report[22:33])) {
				t.Fatalf("log does not preserve right/left trigger blocks: %s", log)
			}
			if !strings.Contains(log, "triggerRightUpdateFlag=true") || !strings.Contains(log, "triggerLeftUpdateFlag=true") {
				t.Fatalf("log missing trigger update flags: %s", log)
			}
		})
	}
}

func TestDualSenseOutputTraceBoundsPayloadsAndNeverIndexesShortData(t *testing.T) {
	var output bytes.Buffer
	diagnostics := newDualSenseOutputDiagnostics(slog.New(slog.NewTextHandler(&output, nil)))
	now := time.Unix(200, 0)

	for _, payload := range [][]byte{
		nil,
		{dualSenseOutputReportID},
		dualSenseTestOutputReport(47),
	} {
		diagnostics.observeAt(dualSenseOutputSourceControlSetReport, payload, now)
	}
	short := dualSenseTestOutputReport(47)
	diagnostics.observeAt(dualSenseOutputSourceControlSetReport, short, now)
	oversized := dualSenseTestOutputReport(65)
	oversized[64] = 0xee
	diagnostics.observeAt(dualSenseOutputSourceInterruptOUT, oversized, now)

	log := output.String()
	if strings.Count(log, "class=InvalidOrShort") < 1 {
		t.Fatalf("short payloads were not classified: %s", log)
	}
	if !strings.Contains(log, "payloadLen=65") || !strings.Contains(log, "rawTruncated=true") {
		t.Fatalf("oversized payload was not marked truncated: %s", log)
	}
	if !strings.Contains(log, "rawHex="+hex.EncodeToString(oversized[:dualSenseOutputRawLimit])) {
		t.Fatalf("oversized payload did not preserve exactly its bounded prefix: %s", log)
	}
	if strings.Contains(log, hex.EncodeToString(oversized)) {
		t.Fatalf("oversized payload was logged in full: %s", log)
	}
	var malformedOutput bytes.Buffer
	malformedDiagnostics := newDualSenseOutputDiagnostics(slog.New(slog.NewTextHandler(&malformedOutput, nil)))
	malformedDiagnostics.observeAt(dualSenseOutputSourceControlSetReport, short, now)
	malformedLog := malformedOutput.String()
	if !strings.Contains(malformedLog, "rawHex="+hex.EncodeToString(short[:dualSenseOutputMalformedRawLimit])) {
		t.Fatalf("malformed payload did not preserve its bounded sample: %s", malformedLog)
	}
	if strings.Contains(malformedLog, "rawHex="+hex.EncodeToString(short)) {
		t.Fatalf("malformed payload was logged in full: %s", malformedLog)
	}
}

func TestDualSenseOutputSignatureTracksHapticsAndAudioControlsNotLEDAnimation(t *testing.T) {
	base := dualSenseTestOutputReport(64)
	baseline := dualSenseOutputSignatureFor(base)

	triggerChange := append([]byte(nil), base...)
	triggerChange[22] = 0x01
	if dualSenseOutputSignatureFor(triggerChange) == baseline {
		t.Fatal("left trigger effect change was not tracked")
	}

	audioControlChange := append([]byte(nil), base...)
	audioControlChange[5] = 0x01
	if dualSenseOutputSignatureFor(audioControlChange) == baseline {
		t.Fatal("audio-control byte change was not tracked")
	}

	ledAnimation := append([]byte(nil), base...)
	ledAnimation[42] = 0x01
	if dualSenseOutputSignatureFor(ledAnimation) != baseline {
		t.Fatal("LED animation should not create repeated detailed snapshots")
	}
}

func TestDualSenseOutputTraceDeduplicatesCapsAndSummarizesWindows(t *testing.T) {
	var output bytes.Buffer
	server := &Server{logger: slog.New(slog.NewTextHandler(&output, nil))}
	diagnostics := newDualSenseOutputDiagnostics(server.logger)
	start := time.Unix(300, 0)
	neutral := dualSenseTestOutputReport(64)
	diagnostics.observeAt(dualSenseOutputSourceInterruptOUT, neutral, start)
	diagnostics.observeAt(dualSenseOutputSourceInterruptOUT, neutral, start.Add(time.Second))

	changed := dualSenseTestOutputReport(64)
	changed[1] = 0x04
	changed[11] = 0x06
	diagnostics.observeAt(dualSenseOutputSourceInterruptOUT, changed, start.Add(2*time.Second))
	control := dualSenseTestOutputReport(48)
	diagnostics.observeAt(dualSenseOutputSourceControlSetReport, control, start.Add(2*time.Second))
	if diagnostics.counts.controlSetReport != 1 {
		t.Fatalf("control path count = %d, want 1", diagnostics.counts.controlSetReport)
	}

	for i := 0; i < 110; i++ {
		report := dualSenseTestOutputReport(64)
		report[1] = 0x01
		report[3] = byte(i)
		diagnostics.observeAt(dualSenseOutputSourceInterruptOUT, report, start.Add(3*time.Second))
	}
	if diagnostics.counts.detailedSnapshots != dualSenseOutputDetailedLogBudget {
		t.Fatalf("detailed snapshots = %d, want cap %d", diagnostics.counts.detailedSnapshots, dualSenseOutputDetailedLogBudget)
	}
	if diagnostics.counts.suppressedSnapshots == 0 {
		t.Fatal("expected over-budget changed reports to be counted as suppressed")
	}
	if diagnostics.counts.repeated == 0 {
		t.Fatal("unchanged report was not counted as repeated")
	}

	// The first packet after ten seconds closes the old activity window and starts a fresh one.
	diagnostics.observeAt(dualSenseOutputSourceInterruptOUT, neutral, start.Add(11*time.Second))
	if diagnostics.counts.total != 1 || diagnostics.counts.detailedSnapshots != 1 {
		t.Fatalf("window was not reset: counts=%+v", diagnostics.counts)
	}
	diagnostics.finishAt(start.Add(12 * time.Second))
	log := output.String()
	if !strings.Contains(log, "source=InterruptOUT") || !strings.Contains(log, "source=ControlSetReportOutput") {
		t.Fatalf("trace did not preserve both ingress source labels: %s", log)
	}
	if !strings.Contains(log, "suppressedDetailedSnapshots=13") {
		t.Fatalf("summary missing exact suppressed count: %s", log)
	}
	if got := strings.Count(log, "msg=DualSenseOutputTraceSummary"); got != 2 {
		t.Fatalf("summary count = %d, want prior-window and final summaries once each: %s", got, log)
	}
}

func TestDualSenseOutputTraceUsesProvidedServerLogger(t *testing.T) {
	var output bytes.Buffer
	server := &Server{logger: slog.New(slog.NewTextHandler(&output, nil))}
	diagnostics := newDualSenseOutputDiagnostics(server.logger)
	diagnostics.observeAt(dualSenseOutputSourceInterruptOUT, dualSenseTestOutputReport(64), time.Unix(400, 0))
	if !strings.Contains(output.String(), "DualSenseOutputTrace") {
		t.Fatalf("trace was not written to the supplied server logger: %s", output.String())
	}
	if got := newDualSenseOutputDiagnostics(nil); got != nil {
		t.Fatal("nil logger should not create a collector")
	}
}

func TestHandleUrbStreamTracesEdgeOutputThroughServerLoggerWithoutChangingTransfer(t *testing.T) {
	device := newTransportOutTestDevice()
	device.descriptor.Strings = map[uint8]string{2: dualSenseEdgeProductName}
	busID := transportOutTestBusID.Add(1) + 42000
	bus, err := virtualbus.NewWithBusID(busID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bus.Add(device); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	server := New(ServerConfig{DisableAutoBusCleanup: true}, slog.New(slog.NewTextHandler(&logs, nil)), nil)
	if err := server.AddBus(bus); err != nil {
		t.Fatal(err)
	}
	serverConn, clientConn := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- server.handleUrbStream(serverConn, device) }()
	harness := &transportOutHarness{client: clientConn}
	doneReceived := false
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
		if !doneReceived {
			<-done
		}
		_ = server.RemoveBus(busID)
	})

	interruptReport := dualSenseTestOutputReport(64)
	interruptReport[1], interruptReport[3], interruptReport[4] = 0x01, 0x12, 0x34
	if err := harness.sendTransfer(1, usbip.DirOut, 3, uint32(len(interruptReport)), [8]byte{}, interruptReport); err != nil {
		t.Fatalf("send interrupt OUT: %v", err)
	}
	if seq, actualLength := harness.readResponse(t); seq != 1 || actualLength != uint32(len(interruptReport)) {
		t.Fatalf("interrupt RET_SUBMIT seq/length = %d/%d", seq, actualLength)
	}

	controlReport := dualSenseTestOutputReport(48)
	controlReport[1] = 0x08
	controlSetup := [8]byte{0x21, 0x09, 0x02, 0x02, 0x00, 0x00, 0x30, 0x00}
	if err := harness.sendTransfer(2, usbip.DirOut, 0, uint32(len(controlReport)), controlSetup, controlReport); err != nil {
		t.Fatalf("send control SET_REPORT: %v", err)
	}
	if seq, actualLength := harness.readResponse(t); seq != 2 || actualLength != uint32(len(controlReport)) {
		t.Fatalf("control RET_SUBMIT seq/length = %d/%d", seq, actualLength)
	}

	if got := device.receivedPayloads(); len(got) != 1 || !bytes.Equal(got[0], interruptReport) {
		t.Fatalf("device received OUT payloads = %x, want the unchanged EP3 payload %x", got, interruptReport)
	}
	_ = clientConn.Close()
	_ = serverConn.Close()
	streamErr := <-done
	doneReceived = true
	if streamErr == nil {
		t.Fatal("closing the USB/IP stream unexpectedly returned nil")
	}
	logsText := logs.String()
	if !strings.Contains(logsText, "source=InterruptOUT") || !strings.Contains(logsText, "source=ControlSetReportOutput") {
		t.Fatalf("server logger did not capture both output sources: %s", logsText)
	}
	if !strings.Contains(logsText, "rawHex="+hex.EncodeToString(interruptReport)) || !strings.Contains(logsText, "rawHex="+hex.EncodeToString(controlReport)) {
		t.Fatalf("server logger did not preserve both original payloads: %s", logsText)
	}
}

func dualSenseTestOutputReport(length int) []byte {
	report := make([]byte, length)
	if length > 0 {
		report[0] = dualSenseOutputReportID
	}
	return report
}
