package xbox360

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"reflect"
	"strconv"
	"sync"
	"testing"

	"github.com/Alia5/VIIPER/usbip"
)

type rumbleTraceTestHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (*rumbleTraceTestHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *rumbleTraceTestHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.records = append(h.records, r.Clone())
	h.mu.Unlock()
	return nil
}
func (h *rumbleTraceTestHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *rumbleTraceTestHandler) WithGroup(string) slog.Handler      { return h }

func (h *rumbleTraceTestHandler) snapshot() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]slog.Record(nil), h.records...)
}

func rumbleTraceRecordAttrs(r slog.Record) map[string]any {
	attrs := make(map[string]any, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	return attrs
}

func assertRumbleTraceProcessIdentity(t *testing.T, records []slog.Record) {
	t.Helper()
	if len(records) == 0 {
		t.Fatal("no rumble trace records to validate")
	}
	var processID int
	for i, record := range records {
		attrs := rumbleTraceRecordAttrs(record)
		got, err := strconv.Atoi(fmt.Sprint(attrs["ProcessID"]))
		if err != nil || got <= 0 {
			t.Fatalf("record %d ProcessID = %v, want a nonzero process ID", i, attrs["ProcessID"])
		}
		if processID == 0 {
			processID = got
		} else if got != processID {
			t.Fatalf("record %d ProcessID = %d, want consistent process ID %d", i, got, processID)
		}
	}
}

func TestRumbleTraceDisabledPreservesCallbackBehavior(t *testing.T) {
	pad, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	var callbacks []XRumbleState
	pad.SetRumbleCallback(func(state XRumbleState) { callbacks = append(callbacks, state) })
	pad.HandleTransfer(context.Background(), 1, usbip.DirOut, []byte{0, 8, 0, 3, 6, 0, 0, 0})
	if want := []XRumbleState{{LeftMotor: 3, RightMotor: 6}}; !reflect.DeepEqual(callbacks, want) {
		t.Fatalf("callbacks = %#v, want %#v", callbacks, want)
	}
	if pad.RumbleTraceSession() != nil {
		t.Fatal("trace unexpectedly enabled without an installed session")
	}
}

func TestRumbleTraceRecordsStopNonzeroAndUnknownPackets(t *testing.T) {
	pad, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := &rumbleTraceTestHandler{}
	trace := pad.InstallRumbleTrace(slog.New(handler), 71, 4)
	var callbacks []XRumbleState
	pad.SetRumbleCallback(func(state XRumbleState) { callbacks = append(callbacks, state) })

	pad.HandleTransfer(context.Background(), 1, usbip.DirOut, []byte{0, 8, 0, 0, 0, 0, 0, 0})
	pad.HandleTransfer(context.Background(), 1, usbip.DirOut, []byte{0, 8, 0, 3, 6, 0, 0, 0})
	pad.HandleTransfer(context.Background(), 1, usbip.DirOut, []byte{1, 3, 0xAA})

	if want := []XRumbleState{{}, {LeftMotor: 3, RightMotor: 6}}; !reflect.DeepEqual(callbacks, want) {
		t.Fatalf("callbacks = %#v, want %#v", callbacks, want)
	}
	records := handler.snapshot()
	assertRumbleTraceProcessIdentity(t, records)
	if len(records) != 9 { // Start + (raw, parsed, dispatch) x 2 + raw + parsed.
		t.Fatalf("trace record count = %d, want 9", len(records))
	}
	wantEvents := []string{
		"X360RumbleTraceStart",
		"X360RumbleRaw", "X360RumbleParsed", "X360RumbleCallbackDispatch",
		"X360RumbleRaw", "X360RumbleParsed", "X360RumbleCallbackDispatch",
		"X360RumbleRaw", "X360RumbleParsed",
	}
	for i, record := range records {
		attrs := rumbleTraceRecordAttrs(record)
		if attrs["Event"] != wantEvents[i] || fmt.Sprint(attrs["BusID"]) != "71" || fmt.Sprint(attrs["DeviceID"]) != "4" || fmt.Sprint(attrs["TraceSessionID"]) != fmt.Sprint(trace.sessionID) {
			t.Fatalf("record %d identity/event = %+v, want event %s and session identity", i, attrs, wantEvents[i])
		}
		if i > 0 && attrs["TraceSeq"] != uint64((i+2)/3) && i < 7 {
			t.Fatalf("record %d TraceSeq = %v, inconsistent with its packet", i, attrs["TraceSeq"])
		}
	}
	stop := rumbleTraceRecordAttrs(records[2])
	if stop["Recognized"] != true || fmt.Sprint(stop["Left"]) != "0" || fmt.Sprint(stop["Right"]) != "0" || stop["CallbackPresent"] != true {
		t.Fatalf("STOP parse = %+v", stop)
	}
	stopDispatch := rumbleTraceRecordAttrs(records[3])
	if stopDispatch["TraceSeq"] != uint64(1) || fmt.Sprint(stopDispatch["Left"]) != "0" || fmt.Sprint(stopDispatch["Right"]) != "0" {
		t.Fatalf("STOP dispatch = %+v", stopDispatch)
	}
	nonzero := rumbleTraceRecordAttrs(records[5])
	if nonzero["TraceSeq"] != uint64(2) || nonzero["Recognized"] != true || fmt.Sprint(nonzero["Left"]) != "3" || fmt.Sprint(nonzero["Right"]) != "6" {
		t.Fatalf("nonzero parse = %+v", nonzero)
	}
	nonzeroDispatch := rumbleTraceRecordAttrs(records[6])
	if nonzeroDispatch["TraceSeq"] != uint64(2) || fmt.Sprint(nonzeroDispatch["Left"]) != "3" || fmt.Sprint(nonzeroDispatch["Right"]) != "6" {
		t.Fatalf("nonzero dispatch = %+v", nonzeroDispatch)
	}
	unknown := rumbleTraceRecordAttrs(records[8])
	if unknown["TraceSeq"] != uint64(3) || unknown["Recognized"] != false || fmt.Sprint(unknown["Length"]) != "3" || fmt.Sprint(unknown["ReportId"]) != "1" || fmt.Sprint(unknown["DeclaredLength"]) != "3" {
		t.Fatalf("unknown parse = %+v", unknown)
	}
}

func TestRumbleTraceSurvivesCallbackClearAndSealsAtTerminalMarker(t *testing.T) {
	pad, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := &rumbleTraceTestHandler{}
	trace := pad.InstallRumbleTrace(slog.New(handler), 72, 9)
	callbacks := 0
	pad.SetRumbleCallback(func(XRumbleState) { callbacks++ })
	pad.SetRumbleCallback(nil)
	pad.HandleTransfer(context.Background(), 1, usbip.DirOut, []byte{0, 8, 0, 0, 0, 0, 0, 0})
	if callbacks != 0 {
		t.Fatalf("callback invoked %d times after clear", callbacks)
	}
	if pad.RumbleTraceSession() != trace {
		t.Fatal("clearing application callback removed the device-owned trace")
	}
	trace.End()
	pad.HandleTransfer(context.Background(), 1, usbip.DirOut, []byte{0, 8, 0, 4, 5, 0, 0, 0})
	records := handler.snapshot()
	assertRumbleTraceProcessIdentity(t, records)
	if len(records) != 4 {
		t.Fatalf("records after callback clear/seal = %d, want Start + raw + parsed + End", len(records))
	}
	parsed := rumbleTraceRecordAttrs(records[2])
	if parsed["CallbackPresent"] != false || parsed["TraceSeq"] != uint64(1) {
		t.Fatalf("parsed record after callback clear = %+v", parsed)
	}
	end := rumbleTraceRecordAttrs(records[3])
	if end["Event"] != "X360RumbleTraceEnd" || end["LastTraceSeq"] != uint64(1) {
		t.Fatalf("end record = %+v", end)
	}
}

func TestRumbleTraceSessionIDsSeparateReusedDeviceIdentity(t *testing.T) {
	first, _ := New(nil)
	second, _ := New(nil)
	firstHandler, secondHandler := &rumbleTraceTestHandler{}, &rumbleTraceTestHandler{}
	firstTrace := first.InstallRumbleTrace(slog.New(firstHandler), 73, 1)
	secondTrace := second.InstallRumbleTrace(slog.New(secondHandler), 73, 1)
	if firstTrace.sessionID == 0 || secondTrace.sessionID == 0 || firstTrace.sessionID == secondTrace.sessionID || firstTrace.sessionID >= secondTrace.sessionID {
		t.Fatalf("session IDs = %d, %d; want distinct nonzero increasing IDs", firstTrace.sessionID, secondTrace.sessionID)
	}
	first.HandleTransfer(context.Background(), 1, usbip.DirOut, []byte{0, 8, 0, 1, 2, 0, 0, 0})
	second.HandleTransfer(context.Background(), 1, usbip.DirOut, []byte{0, 8, 0, 3, 4, 0, 0, 0})
	var allRecords []slog.Record
	for i, handler := range []*rumbleTraceTestHandler{firstHandler, secondHandler} {
		records := handler.snapshot()
		allRecords = append(allRecords, records...)
		if len(records) != 3 {
			t.Fatalf("session %d records = %d, want Start + raw + parsed", i, len(records))
		}
		for _, record := range records {
			attrs := rumbleTraceRecordAttrs(record)
			wantSeq := uint64(0)
			if attrs["Event"] != "X360RumbleTraceStart" {
				wantSeq = 1
			}
			if fmt.Sprint(attrs["BusID"]) != "73" || fmt.Sprint(attrs["DeviceID"]) != "1" || attrs["TraceSeq"] != wantSeq {
				t.Fatalf("session %d record = %+v", i, attrs)
			}
		}
	}
	assertRumbleTraceProcessIdentity(t, allRecords)
}

func TestRumbleTraceBoundsRawPayloadTo32Bytes(t *testing.T) {
	pad, _ := New(nil)
	handler := &rumbleTraceTestHandler{}
	pad.InstallRumbleTrace(slog.New(handler), 74, 2)
	payload := make([]byte, 40)
	for i := range payload {
		payload[i] = byte(i)
	}
	pad.HandleTransfer(context.Background(), 1, usbip.DirOut, payload)
	records := handler.snapshot()
	assertRumbleTraceProcessIdentity(t, records)
	if len(records) != 3 {
		t.Fatalf("record count = %d, want Start + raw + parsed", len(records))
	}
	attrs := rumbleTraceRecordAttrs(records[1])
	if fmt.Sprint(attrs["Length"]) != "40" || attrs["Payload"] != hex.EncodeToString(payload[:32]) {
		t.Fatalf("bounded raw record = %+v", attrs)
	}
}

func TestRumbleTraceAbortCarriesProcessIdentity(t *testing.T) {
	pad, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := &rumbleTraceTestHandler{}
	trace := pad.InstallRumbleTrace(slog.New(handler), 75, 3)
	trace.Abort("test-rollback")

	records := handler.snapshot()
	assertRumbleTraceProcessIdentity(t, records)
	if len(records) != 2 || rumbleTraceRecordAttrs(records[1])["Event"] != "X360RumbleTraceAbort" {
		t.Fatalf("abort records = %+v, want Start + Abort", records)
	}
}
