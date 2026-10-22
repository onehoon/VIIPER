package usb

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
)

// This immediate-capture build is intentionally enabled; set it to false after capture and rebuild.
const enableDualSenseOutputDiagnostics = true

const (
	dualSenseEdgeProductName         = "DualSense Edge Wireless Controller"
	dualSenseOutputReportID          = 0x02
	dualSenseOutputMinReportLength   = 48
	dualSenseOutputRawLimit          = 64
	dualSenseOutputMalformedRawLimit = 16
	dualSenseOutputDiagnosticWindow  = 10 * time.Second
	dualSenseOutputDetailedLogBudget = 100
)

type dualSenseOutputSource uint8

const (
	dualSenseOutputSourceInterruptOUT dualSenseOutputSource = iota + 1
	dualSenseOutputSourceControlSetReport
)

func (s dualSenseOutputSource) String() string {
	switch s {
	case dualSenseOutputSourceInterruptOUT:
		return "InterruptOUT"
	case dualSenseOutputSourceControlSetReport:
		return "ControlSetReportOutput"
	default:
		return "Unknown"
	}
}

func dualSenseOutputSourceForTransfer(dir, ep uint32, setup, payload []byte) (dualSenseOutputSource, bool) {
	if dir != usbip.DirOut {
		return 0, false
	}
	if ep == 3 {
		if len(payload) > 0 && payload[0] != dualSenseOutputReportID {
			return 0, false
		}
		return dualSenseOutputSourceInterruptOUT, true
	}
	if ep != 0 || len(setup) != 8 || setup[0] != 0x21 || setup[1] != 0x09 ||
		binary.LittleEndian.Uint16(setup[2:4]) != 0x0202 {
		return 0, false
	}
	// Empty data is retained as a malformed sample; a different report ID is not this output report.
	if len(payload) > 0 && payload[0] != dualSenseOutputReportID {
		return 0, false
	}
	return dualSenseOutputSourceControlSetReport, true
}

func isDualSenseEdgeDescriptor(desc *usb.Descriptor) bool {
	return desc != nil && desc.Strings[2] == dualSenseEdgeProductName
}

type dualSenseOutputClass uint8

const (
	dualSenseOutputClassInvalidOrShort dualSenseOutputClass = 1 << iota
	dualSenseOutputClassParserEligibleV1
	dualSenseOutputClassParserEligibleV2
	dualSenseOutputClassExactNeutralCandidate
	dualSenseOutputClassTriggerUpdateFlagPresent
	dualSenseOutputClassOtherOutput
)

type dualSenseOutputDiagnosticCounts struct {
	total                  uint64
	interruptOUT           uint64
	controlSetReport       uint64
	parserEligibleV1       uint64
	parserEligibleV2       uint64
	exactNeutralCandidates uint64
	rightTriggerUpdates    uint64
	leftTriggerUpdates     uint64
	otherOutput            uint64
	malformedOrShort       uint64
	repeated               uint64
	detailedSnapshots      uint64
	suppressedSnapshots    uint64
}

type dualSenseOutputSignature struct {
	valid         bool
	payloadLength uint32
	tracked       [32]byte
}

// The collector is local to one serialized handleUrbStream OUT path. It never retains a payload.
type dualSenseOutputDiagnostics struct {
	logger        *slog.Logger
	windowStarted time.Time
	counts        dualSenseOutputDiagnosticCounts
	seenClasses   [2]dualSenseOutputClass
	lastSignature [2]dualSenseOutputSignature
	hasSignature  [2]bool
}

func newDualSenseOutputDiagnostics(logger *slog.Logger) *dualSenseOutputDiagnostics {
	if logger == nil {
		return nil
	}
	return &dualSenseOutputDiagnostics{logger: logger}
}

func (d *dualSenseOutputDiagnostics) observe(source dualSenseOutputSource, payload []byte) {
	d.observeAt(source, payload, time.Now())
}

func (d *dualSenseOutputDiagnostics) observeAt(source dualSenseOutputSource, payload []byte, now time.Time) {
	if d == nil || d.logger == nil {
		return
	}
	if d.windowStarted.IsZero() {
		d.windowStarted = now
	} else if elapsed := now.Sub(d.windowStarted); elapsed >= dualSenseOutputDiagnosticWindow {
		d.logSummary(elapsed)
		d.resetWindow(now)
	}

	class := classifyDualSenseOutput(payload)
	d.counts.total++
	sourceIndex := dualSenseOutputSourceIndex(source)
	if source == dualSenseOutputSourceInterruptOUT {
		d.counts.interruptOUT++
	} else {
		d.counts.controlSetReport++
	}
	if class&dualSenseOutputClassParserEligibleV1 != 0 {
		d.counts.parserEligibleV1++
	}
	if class&dualSenseOutputClassParserEligibleV2 != 0 {
		d.counts.parserEligibleV2++
	}
	if class&dualSenseOutputClassExactNeutralCandidate != 0 {
		d.counts.exactNeutralCandidates++
	}
	if class&dualSenseOutputClassTriggerUpdateFlagPresent != 0 && len(payload) >= dualSenseOutputMinReportLength && payload[0] == dualSenseOutputReportID {
		if payload[1]&0x04 != 0 {
			d.counts.rightTriggerUpdates++
		}
		if payload[1]&0x08 != 0 {
			d.counts.leftTriggerUpdates++
		}
	}
	if class&dualSenseOutputClassOtherOutput != 0 {
		d.counts.otherOutput++
	}
	if class&dualSenseOutputClassInvalidOrShort != 0 {
		d.counts.malformedOrShort++
	}

	signature := dualSenseOutputSignatureFor(payload)
	changed := !d.hasSignature[sourceIndex] || d.lastSignature[sourceIndex] != signature
	firstClassForSource := class&^d.seenClasses[sourceIndex] != 0
	d.seenClasses[sourceIndex] |= class
	d.lastSignature[sourceIndex] = signature
	d.hasSignature[sourceIndex] = true
	if !changed && !firstClassForSource {
		d.counts.repeated++
		return
	}
	if d.counts.detailedSnapshots >= dualSenseOutputDetailedLogBudget {
		d.counts.suppressedSnapshots++
		return
	}
	d.counts.detailedSnapshots++
	d.logSnapshot(source, payload, class)
}

func dualSenseOutputSourceIndex(source dualSenseOutputSource) int {
	if source == dualSenseOutputSourceControlSetReport {
		return 1
	}
	return 0
}

func classifyDualSenseOutput(payload []byte) dualSenseOutputClass {
	if len(payload) < dualSenseOutputMinReportLength || payload[0] != dualSenseOutputReportID {
		return dualSenseOutputClassInvalidOrShort
	}

	var class dualSenseOutputClass
	if payload[1]&0x01 != 0 {
		class |= dualSenseOutputClassParserEligibleV1
	}
	if payload[39]&0x04 != 0 {
		class |= dualSenseOutputClassParserEligibleV2
	}
	if payload[1]&0x04 != 0 || payload[1]&0x08 != 0 {
		class |= dualSenseOutputClassTriggerUpdateFlagPresent
	}
	if payload[1] == 0 && payload[2] == 0 && payload[39] == 0 && payload[3] == 0 && payload[4] == 0 {
		class |= dualSenseOutputClassExactNeutralCandidate
	}
	if class == 0 {
		class = dualSenseOutputClassOtherOutput
	}
	return class
}

func dualSenseOutputSignatureFor(payload []byte) dualSenseOutputSignature {
	signature := dualSenseOutputSignature{payloadLength: uint32(len(payload))}
	if len(payload) >= dualSenseOutputMinReportLength && payload[0] == dualSenseOutputReportID {
		signature.valid = true
		copy(signature.tracked[0:4], payload[1:5])
		copy(signature.tracked[4:9], payload[5:10])
		copy(signature.tracked[9:31], payload[11:33])
		signature.tracked[31] = payload[39]
		return signature
	}
	limit := min(len(payload), dualSenseOutputMalformedRawLimit)
	copy(signature.tracked[:], payload[:limit])
	return signature
}

func (d *dualSenseOutputDiagnostics) logSnapshot(source dualSenseOutputSource, payload []byte, class dualSenseOutputClass) {
	valid := len(payload) >= dualSenseOutputMinReportLength && payload[0] == dualSenseOutputReportID
	rawLimit := dualSenseOutputMalformedRawLimit
	if valid {
		rawLimit = dualSenseOutputRawLimit
	}
	captured := payload[:min(len(payload), rawLimit)]
	attrs := []any{
		"event", "DualSenseOutputTrace",
		"model", "Edge",
		"source", source.String(),
		"payloadLen", len(payload),
		"class", dualSenseOutputClassString(class),
		"rawHex", hex.EncodeToString(captured),
		"rawTruncated", len(payload) > len(captured),
	}
	if len(payload) > 0 {
		attrs = append(attrs, "reportID", fmt.Sprintf("%02x", payload[0]))
	}
	if valid {
		attrs = append(attrs,
			"flag0", fmt.Sprintf("%02x", payload[1]),
			"flag1", fmt.Sprintf("%02x", payload[2]),
			"motorRight8", fmt.Sprintf("%02x", payload[3]),
			"motorLeft8", fmt.Sprintf("%02x", payload[4]),
			"flag2", fmt.Sprintf("%02x", payload[39]),
			"triggerRightUpdateFlag", payload[1]&0x04 != 0,
			"triggerLeftUpdateFlag", payload[1]&0x08 != 0,
			"triggerRightModeRaw", fmt.Sprintf("%02x", payload[11]),
			"triggerLeftModeRaw", fmt.Sprintf("%02x", payload[22]),
			"triggerRightHex", hex.EncodeToString(payload[11:22]),
			"triggerLeftHex", hex.EncodeToString(payload[22:33]),
		)
	}
	d.logger.Info("DualSenseOutputTrace", attrs...)
}

func dualSenseOutputClassString(class dualSenseOutputClass) string {
	var names []string
	for _, entry := range []struct {
		flag dualSenseOutputClass
		name string
	}{
		{dualSenseOutputClassParserEligibleV1, "ParserEligibleV1"},
		{dualSenseOutputClassParserEligibleV2, "ParserEligibleV2"},
		{dualSenseOutputClassExactNeutralCandidate, "ExactNeutralCandidate"},
		{dualSenseOutputClassTriggerUpdateFlagPresent, "TriggerUpdateFlagPresent"},
		{dualSenseOutputClassOtherOutput, "OtherOutput"},
		{dualSenseOutputClassInvalidOrShort, "InvalidOrShort"},
	} {
		if class&entry.flag != 0 {
			names = append(names, entry.name)
		}
	}
	return strings.Join(names, ",")
}

func (d *dualSenseOutputDiagnostics) logSummary(duration time.Duration) {
	d.logger.Info("DualSenseOutputTraceSummary",
		"event", "DualSenseOutputTraceSummary",
		"model", "Edge",
		"windowDurationMs", duration.Milliseconds(),
		"totalReports", d.counts.total,
		"interruptOUTReports", d.counts.interruptOUT,
		"controlSetReportReports", d.counts.controlSetReport,
		"parserEligibleV1", d.counts.parserEligibleV1,
		"parserEligibleV2", d.counts.parserEligibleV2,
		"exactNeutralCandidates", d.counts.exactNeutralCandidates,
		"rightTriggerFlagUpdates", d.counts.rightTriggerUpdates,
		"leftTriggerFlagUpdates", d.counts.leftTriggerUpdates,
		"otherReports", d.counts.otherOutput,
		"malformedOrShortReports", d.counts.malformedOrShort,
		"repeatedReports", d.counts.repeated,
		"detailedSnapshots", d.counts.detailedSnapshots,
		"suppressedDetailedSnapshots", d.counts.suppressedSnapshots,
	)
}

func (d *dualSenseOutputDiagnostics) resetWindow(now time.Time) {
	d.windowStarted = now
	d.counts = dualSenseOutputDiagnosticCounts{}
	d.seenClasses = [2]dualSenseOutputClass{}
	d.lastSignature = [2]dualSenseOutputSignature{}
	d.hasSignature = [2]bool{}
}

func (d *dualSenseOutputDiagnostics) finish() {
	d.finishAt(time.Now())
}

func (d *dualSenseOutputDiagnostics) finishAt(now time.Time) {
	if d == nil || d.logger == nil || d.counts.total == 0 {
		return
	}
	duration := now.Sub(d.windowStarted)
	if duration < 0 {
		duration = 0
	}
	d.logSummary(duration)
	d.counts = dualSenseOutputDiagnosticCounts{}
}
