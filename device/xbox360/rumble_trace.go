package xbox360

import (
	"encoding/hex"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
)

var rumbleTraceSessionCounter atomic.Uint64

// RumbleTrace is an internal Go diagnostic session. It does not participate in
// device behavior, callback ownership, or the exported C ABI.
type RumbleTrace struct {
	logger    *slog.Logger
	busID     uint32
	deviceID  uint32
	sessionID uint64
	mu        sync.Mutex
	seq       uint64
	sealed    bool
}

func newRumbleTrace(logger *slog.Logger, busID, deviceID uint32) *RumbleTrace {
	var sessionID uint64
	for {
		current := rumbleTraceSessionCounter.Load()
		if current == math.MaxUint64 {
			return nil
		}
		sessionID = current + 1
		if rumbleTraceSessionCounter.CompareAndSwap(current, sessionID) {
			break
		}
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &RumbleTrace{logger: logger, busID: busID, deviceID: deviceID, sessionID: sessionID}
}

// InstallRumbleTrace attaches a new diagnostic session to the logical device
// and emits its start marker. The caller must invoke it after USB identity is
// known and before the device can be exposed to a host.
func (x *Xbox360) InstallRumbleTrace(logger *slog.Logger, busID, deviceID uint32) *RumbleTrace {
	trace := newRumbleTrace(logger, busID, deviceID)
	if trace == nil {
		return nil
	}
	x.rumbleTrace.Store(trace)
	trace.log("X360RumbleTraceStart", "TraceSeq", uint64(0))
	return trace
}

// RumbleTraceSession returns the currently installed internal diagnostic
// session so the owning lifecycle can retain it through transport drain.
func (x *Xbox360) RumbleTraceSession() *RumbleTrace { return x.rumbleTrace.Load() }

// ClearRumbleTrace releases the expected terminal session after its marker has
// been queued. Callback clearing deliberately does not call this method.
func (x *Xbox360) ClearRumbleTrace(expected *RumbleTrace) {
	if expected != nil {
		x.rumbleTrace.CompareAndSwap(expected, nil)
	}
}

// End seals a successfully retired session and emits its final sequence fence.
func (t *RumbleTrace) End() {
	t.finish("X360RumbleTraceEnd", "")
}

// Abort seals a creation session that was safely rolled back before commit.
func (t *RumbleTrace) Abort(reason string) {
	t.finish("X360RumbleTraceAbort", reason)
}

func (t *RumbleTrace) finish(event, reason string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sealed {
		return
	}
	t.sealed = true
	attrs := []any{"LastTraceSeq", t.seq}
	if reason != "" {
		attrs = append(attrs, "Reason", reason)
	}
	t.log(event, attrs...)
}

func (t *RumbleTrace) recordPacket(payload []byte, recognized, callbackPresent bool, rumble XRumbleState) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sealed {
		return
	}
	t.seq++
	seq := t.seq
	bounded := payload
	if len(bounded) > 32 {
		bounded = bounded[:32]
	}
	t.log("X360RumbleRaw", "TraceSeq", seq, "Length", len(payload), "Payload", hex.EncodeToString(bounded))
	if recognized {
		t.log("X360RumbleParsed", "TraceSeq", seq, "Recognized", true, "Left", rumble.LeftMotor, "Right", rumble.RightMotor, "CallbackPresent", callbackPresent)
		if callbackPresent {
			t.log("X360RumbleCallbackDispatch", "TraceSeq", seq, "Left", rumble.LeftMotor, "Right", rumble.RightMotor)
		}
		return
	}
	attrs := []any{"TraceSeq", seq, "Recognized", false, "Length", len(payload)}
	if len(payload) > 0 {
		attrs = append(attrs, "ReportId", payload[0])
	}
	if len(payload) > 1 {
		attrs = append(attrs, "DeclaredLength", payload[1])
	}
	t.log("X360RumbleParsed", attrs...)
}

func (t *RumbleTrace) log(event string, attrs ...any) {
	base := []any{"Event", event, "BusID", t.busID, "DeviceID", t.deviceID, "TraceSessionID", t.sessionID}
	t.logger.Info("xbox360 rumble trace", append(base, attrs...)...)
}
