package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime/cgo"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	testusbip "github.com/Alia5/VIIPER/_testing"
	"github.com/Alia5/VIIPER/device/xbox360"
	"github.com/Alia5/VIIPER/internal/server/api"
	serverusb "github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/usbip"
)

type xbox360TraceCapture struct {
	mu           sync.Mutex
	records      []slog.Record
	blockEvent   string
	blockEntered chan struct{}
	blockRelease <-chan struct{}
	blockUsed    atomic.Bool
}

func (*xbox360TraceCapture) Enabled(context.Context, slog.Level) bool { return true }
func (h *xbox360TraceCapture) Handle(_ context.Context, r slog.Record) error {
	if h.blockEvent != "" && recordAttrs(r)["Event"] == h.blockEvent {
		if h.blockUsed.CompareAndSwap(false, true) {
			close(h.blockEntered)
			<-h.blockRelease
		}
	}
	h.mu.Lock()
	h.records = append(h.records, r.Clone())
	h.mu.Unlock()
	return nil
}

func (h *xbox360TraceCapture) blockNext(event string) (<-chan struct{}, func()) {
	entered, release := make(chan struct{}), make(chan struct{})
	h.blockEvent, h.blockEntered, h.blockRelease = event, entered, release
	h.blockUsed.Store(false)
	var once sync.Once
	return entered, func() { once.Do(func() { close(release) }) }
}
func (h *xbox360TraceCapture) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *xbox360TraceCapture) WithGroup(string) slog.Handler      { return h }

func (h *xbox360TraceCapture) snapshot() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]slog.Record(nil), h.records...)
}

func xbox360TraceAttrs(r slog.Record) map[string]any { return recordAttrs(r) }

func xbox360TraceEvents(h *xbox360TraceCapture) []map[string]any {
	var events []map[string]any
	for _, record := range h.snapshot() {
		attrs := xbox360TraceAttrs(record)
		if _, ok := attrs["Event"]; ok {
			events = append(events, attrs)
		}
	}
	return events
}

func assertXbox360TraceProcessIdentity(t *testing.T, events []map[string]any) {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("no Xbox360 trace events to validate")
	}
	var processID int
	for i, event := range events {
		got, err := strconv.Atoi(fmt.Sprint(event["ProcessID"]))
		if err != nil || got <= 0 {
			t.Fatalf("event %d ProcessID = %v, want a nonzero process ID", i, event["ProcessID"])
		}
		if processID == 0 {
			processID = got
		} else if got != processID {
			t.Fatalf("event %d ProcessID = %d, want consistent process ID %d", i, got, processID)
		}
	}
}

func setRumbleTraceSinkForTest(t *testing.T, h slog.Handler) {
	t.Helper()
	old := rumbleTraceLoggerFactory
	rumbleTraceLoggerFactory = func() *slog.Logger { return buildEmbeddedLogger(h, nil) }
	t.Cleanup(func() { rumbleTraceLoggerFactory = old })
}

func createXbox360ForTraceTest(t *testing.T, hw *usbServerHandleWrapper, busID uint32, autoAttach bool) (deviceHandle, *xbox360.Xbox360) {
	t.Helper()
	serverHandle := diagnosticServerHandle(t, hw)
	var handle deviceHandle
	if !createXbox360Device(serverHandle, &handle, busID, autoAttach, 0, 0, 0) {
		t.Fatal("canonical Xbox360 creation failed")
	}
	dhw := hw.deviceHandleRecords[handle]
	pad, ok := dhw.device.(*xbox360.Xbox360)
	if !ok {
		t.Fatalf("created device type = %T, want *xbox360.Xbox360", dhw.device)
	}
	return handle, pad
}

func TestCanonicalXbox360TraceIsForcedOnWithoutEnvironmentSwitch(t *testing.T) {
	const legacyEnvironment = "VIIPER_X360_RUMBLE_TRACE"
	values := []struct {
		name  string
		value string
		unset bool
	}{
		{name: "unset", unset: true},
		{name: "zero", value: "0"},
	}
	for i, tc := range values {
		t.Run(tc.name, func(t *testing.T) {
			if tc.unset {
				previous, existed := os.LookupEnv(legacyEnvironment)
				if err := os.Unsetenv(legacyEnvironment); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if existed {
						_ = os.Setenv(legacyEnvironment, previous)
					} else {
						_ = os.Unsetenv(legacyEnvironment)
					}
				})
			} else {
				t.Setenv(legacyEnvironment, tc.value)
			}
			handler := &xbox360TraceCapture{}
			setRumbleTraceSinkForTest(t, handler)
			hw, _ := newLifecycleTestServer(t, uint32(10200+i))
			handle, pad := createXbox360ForTraceTest(t, hw, uint32(10200+i), false)
			if pad.RumbleTraceSession() == nil {
				t.Fatal("canonical Xbox360 creation did not install the forced trace")
			}
			starts := 0
			for _, event := range xbox360TraceEvents(handler) {
				if event["Event"] == "X360RumbleTraceStart" {
					starts++
				}
			}
			if starts != 1 {
				t.Fatalf("TraceStart count = %d, want 1", starts)
			}
			if got := removeXbox360DeviceResult(uintptr(handle)); got != typedDeviceRemoveSuccess {
				t.Fatalf("remove result = %d", got)
			}
		})
	}
}

func TestCanonicalXbox360TraceFileOpenFailureDoesNotBlockDeviceLifecycle(t *testing.T) {
	failedHandler, failedWriter := openEmbeddedLogFileHandler(
		func() (string, bool) { return `X:\unavailable\libVIIPER.log`, true },
		noopStatModTime,
		func(string) (dailyLogWriter, error) { return nil, errors.New("simulated file-open failure") },
		time.Now,
	)
	if failedHandler != nil || failedWriter != nil {
		t.Fatal("simulated file-open failure unexpectedly produced a sink")
	}
	setRumbleTraceSinkForTest(t, failedHandler)
	hw, _ := newLifecycleTestServer(t, 10209)
	hw.logger = buildEmbeddedLogger(failedHandler, nil)
	handle, pad := createXbox360ForTraceTest(t, hw, 10209, false)
	if pad.RumbleTraceSession() == nil {
		t.Fatal("file-sink failure prevented forced trace installation")
	}
	if got := removeXbox360DeviceResult(uintptr(handle)); got != typedDeviceRemoveSuccess {
		t.Fatalf("device removal after file-sink failure = %d", got)
	}
}

func TestCanonicalXbox360TraceStartsBeforeAutoAttachAndUsesFileOnlySink(t *testing.T) {
	traceSink, callbackSink := &xbox360TraceCapture{}, &xbox360TraceCapture{}
	setRumbleTraceSinkForTest(t, traceSink)
	hw, _ := newLifecycleTestServer(t, 10211)
	hw.logger = slog.New(callbackSink)
	attachCalled := false
	hw.ops.attachLocalhostTracked = func(_ context.Context, meta *usbip.ExportMeta, _ uint16, _ bool, _ *slog.Logger) (api.LocalhostAttachment, error) {
		attachCalled = true
		events := xbox360TraceEvents(traceSink)
		if len(events) != 1 || events[0]["Event"] != "X360RumbleTraceStart" || fmt.Sprint(events[0]["BusID"]) != "10211" || fmt.Sprint(events[0]["DeviceID"]) != fmt.Sprint(meta.DevID) {
			return api.LocalhostAttachment{}, errors.New("trace did not start after identity and before auto-attach")
		}
		devs := hw.s.GetBus(10211).Devices()
		pad, ok := devs[0].(*xbox360.Xbox360)
		if !ok {
			return api.LocalhostAttachment{}, fmt.Errorf("created device type is %T", devs[0])
		}
		pad.SetRumbleCallback(func(xbox360.XRumbleState) {})
		pad.HandleTransfer(context.Background(), 1, usbip.DirOut, []byte{0, 8, 0, 0, 0, 0, 0, 0})
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 5011}, nil
	}
	hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error { return nil }
	handle, pad := createXbox360ForTraceTest(t, hw, 10211, true)
	if !attachCalled || pad.RumbleTraceSession() == nil {
		t.Fatal("auto-attach did not run with the device trace already active")
	}
	if got := []string{xbox360TraceEvents(traceSink)[0]["Event"].(string), xbox360TraceEvents(traceSink)[1]["Event"].(string), xbox360TraceEvents(traceSink)[2]["Event"].(string)}; fmt.Sprint(got) != "[X360RumbleTraceStart X360RumbleRaw X360RumbleParsed]" {
		t.Fatalf("first auto-attached OUT ordering = %v", got)
	}
	hw.logger.Info("ordinary-observer-check")
	for _, record := range callbackSink.snapshot() {
		if attrs := xbox360TraceAttrs(record); attrs["Event"] != nil {
			t.Fatalf("rumble trace reached VIIPERLogCallback observer: %+v", attrs)
		}
	}
	foundOrdinary := false
	for _, record := range callbackSink.snapshot() {
		if record.Message == "ordinary-observer-check" {
			foundOrdinary = true
		}
	}
	if !foundOrdinary {
		t.Fatal("ordinary server diagnostics stopped reaching the callback observer")
	}
	if got := removeXbox360DeviceResult(uintptr(handle)); got != typedDeviceRemoveSuccess {
		t.Fatalf("remove result = %d", got)
	}
	for _, record := range callbackSink.snapshot() {
		if attrs := xbox360TraceAttrs(record); attrs["Event"] != nil {
			t.Fatalf("rumble trace terminal record reached VIIPERLogCallback observer: %+v", attrs)
		}
	}
	events := xbox360TraceEvents(traceSink)
	assertXbox360TraceProcessIdentity(t, events)
	for _, event := range events {
		if event["Event"] == "X360RumbleTraceEnd" {
			foundDispatch := false
			for _, traceEvent := range events {
				if traceEvent["Event"] == "X360RumbleCallbackDispatch" {
					foundDispatch = true
					break
				}
			}
			if !foundDispatch {
				t.Fatal("auto-attached trace did not include callback dispatch")
			}
			return
		}
	}
	t.Fatal("successful auto-attached device removal did not emit TraceEnd")
}

func TestCanonicalXbox360TraceKnownRollbackAbortsAndReusedIDGetsNewSession(t *testing.T) {
	traceSink := &xbox360TraceCapture{}
	setRumbleTraceSinkForTest(t, traceSink)
	hw, bus := newLifecycleTestServer(t, 10212)
	attachCalls := 0
	startWasPresentBeforeAttach := false
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		attachCalls++
		events := xbox360TraceEvents(traceSink)
		startWasPresentBeforeAttach = len(events) > 0 && events[0]["Event"] == "X360RumbleTraceStart"
		if attachCalls == 1 {
			return api.LocalhostAttachment{}, errors.New("known attach failure")
		}
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 5012}, nil
	}
	hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error { return nil }
	serverHandle := diagnosticServerHandle(t, hw)
	var failedHandle deviceHandle
	if createXbox360Device(serverHandle, &failedHandle, 10212, true, 0, 0, 0) {
		t.Fatal("known failed auto-attach unexpectedly created a device")
	}
	if !startWasPresentBeforeAttach {
		t.Fatal("auto-attach attempt began before TraceStart")
	}
	if len(bus.Devices()) != 0 || len(hw.deviceHandles[10212]) != 0 {
		t.Fatal("known rollback retained the created device")
	}
	firstEvents := xbox360TraceEvents(traceSink)
	if len(firstEvents) != 2 || firstEvents[0]["Event"] != "X360RumbleTraceStart" || firstEvents[1]["Event"] != "X360RumbleTraceAbort" || firstEvents[1]["Reason"] != "auto-attach-failure" || firstEvents[1]["LastTraceSeq"] != uint64(0) {
		t.Fatalf("known rollback trace = %+v", firstEvents)
	}
	firstSession := firstEvents[0]["TraceSessionID"]
	var secondHandle deviceHandle
	if !createXbox360Device(serverHandle, &secondHandle, 10212, false, 0, 0, 0) {
		t.Fatal("creation after rollback failed")
	}
	secondDeviceID := hw.deviceHandleRecords[secondHandle].exportMeta.DevID
	allEvents := xbox360TraceEvents(traceSink)
	secondSession := allEvents[len(allEvents)-1]["TraceSessionID"]
	if fmt.Sprint(firstEvents[0]["DeviceID"]) != fmt.Sprint(secondDeviceID) || firstSession == secondSession {
		t.Fatalf("reused device identity/session = %v / %v, first session=%v", secondDeviceID, secondSession, firstSession)
	}
	if got := removeXbox360DeviceResult(uintptr(secondHandle)); got != typedDeviceRemoveSuccess {
		t.Fatalf("second device remove result = %d", got)
	}
	for _, event := range xbox360TraceEvents(traceSink) {
		if event["TraceSessionID"] == firstSession && event["Event"] == "X360RumbleTraceEnd" {
			t.Fatal("aborted creation session was incorrectly ended as a committed device")
		}
	}
}

func TestCanonicalXbox360TraceCallbackClearLeavesTraceActive(t *testing.T) {
	traceSink := &xbox360TraceCapture{}
	setRumbleTraceSinkForTest(t, traceSink)
	hw, _ := newLifecycleTestServer(t, 10216)
	handle, pad := createXbox360ForTraceTest(t, hw, 10216, false)
	var callbacks []xbox360.XRumbleState
	if !setXbox360RumbleCallback(uintptr(handle), func(state xbox360.XRumbleState) { callbacks = append(callbacks, state) }) {
		t.Fatal("callback registration failed")
	}
	pad.HandleTransfer(context.Background(), 1, usbip.DirOut, []byte{0, 8, 0, 3, 6, 0, 0, 0})
	if !setXbox360RumbleCallback(uintptr(handle), nil) {
		t.Fatal("callback clear failed")
	}
	pad.HandleTransfer(context.Background(), 1, usbip.DirOut, []byte{0, 8, 0, 0, 0, 0, 0, 0})
	if len(callbacks) != 1 || callbacks[0] != (xbox360.XRumbleState{LeftMotor: 3, RightMotor: 6}) {
		t.Fatalf("application callbacks = %+v, want only the pre-clear 3/6 callback", callbacks)
	}
	events := xbox360TraceEvents(traceSink)
	if pad.RumbleTraceSession() == nil {
		t.Fatal("SetXbox360RumbleCallback(NULL) cleared the trace context")
	}
	var sequenceTwoRaw, sequenceTwoParsed, sequenceTwoDispatch int
	for _, event := range events {
		if event["TraceSeq"] != uint64(2) {
			continue
		}
		switch event["Event"] {
		case "X360RumbleRaw":
			sequenceTwoRaw++
		case "X360RumbleParsed":
			sequenceTwoParsed++
			if event["CallbackPresent"] != false || event["Recognized"] != true {
				t.Fatalf("post-clear parsed event = %+v", event)
			}
		case "X360RumbleCallbackDispatch":
			sequenceTwoDispatch++
		}
	}
	if sequenceTwoRaw != 1 || sequenceTwoParsed != 1 || sequenceTwoDispatch != 0 {
		t.Fatalf("post-clear raw/parsed/dispatch counts = %d/%d/%d", sequenceTwoRaw, sequenceTwoParsed, sequenceTwoDispatch)
	}
	if got := removeXbox360DeviceResult(uintptr(handle)); got != typedDeviceRemoveSuccess {
		t.Fatalf("remove result = %d", got)
	}
}

func TestCanonicalXbox360TraceRollbackAbortWaitsForExposedTransportDrain(t *testing.T) {
	const busID = uint32(10217)
	traceSink := &xbox360TraceCapture{}
	setRumbleTraceSinkForTest(t, traceSink)
	traceEntered, releaseTrace := traceSink.blockNext("X360RumbleRaw")
	defer releaseTrace()
	hw, _ := newLifecycleTestServer(t, busID)
	startXbox360TraceTransport(t, hw)
	serverHandle := diagnosticServerHandle(t, hw)
	importedCh := make(chan *testusbip.ImportResult, 1)
	attachErrCh := make(chan error, 1)
	submitDone := make(chan error, 1)
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		client := testusbip.NewUsbIpClient(t, hw.s.Addr())
		imported, err := client.AttachDevice(fmt.Sprintf("%d-1", busID))
		attachErrCh <- err
		if err != nil {
			return api.LocalhostAttachment{}, errors.New("host import failed")
		}
		importedCh <- imported
		go func() {
			submitDone <- client.SubmitWithTimeout(imported.Conn, usbip.DirOut, 1, []byte{0, 8, 0, 0, 0, 0, 0, 0}, nil, 10*time.Second)
		}()
		select {
		case <-traceEntered:
			return api.LocalhostAttachment{}, errors.New("known local attach failure after host OUT became in-flight")
		case <-time.After(5 * time.Second):
			return api.LocalhostAttachment{}, errors.New("host OUT did not reach trace sink")
		}
	}
	finalized := make(chan struct{})
	var finalizedOnce sync.Once
	oldDelete := hw.ops.deleteHandle
	hw.ops.deleteHandle = func(h cgo.Handle) {
		oldDelete(h)
		finalizedOnce.Do(func() { close(finalized) })
	}
	createDone := make(chan bool, 1)
	go func() {
		var handle deviceHandle
		createDone <- createXbox360Device(serverHandle, &handle, busID, true, 0, 0, 0)
	}()
	select {
	case err := <-attachErrCh:
		if err != nil {
			t.Fatalf("test host import failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("auto-attach test seam did not attempt the host import")
	}
	var imported *testusbip.ImportResult
	select {
	case imported = <-importedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("test host import was not retained")
	}
	t.Cleanup(func() { _ = imported.Conn.Close() })
	awaitXbox360TraceSignal(t, traceEntered, "creation rollback in-flight raw trace")
	awaitXbox360TraceSignal(t, finalized, "rolled-back logical handle finalization")
	for _, event := range xbox360TraceEvents(traceSink) {
		if event["Event"] == "X360RumbleTraceAbort" {
			t.Fatal("TraceAbort was emitted before the exposed transport drain completed")
		}
	}
	releaseTrace()
	select {
	case created := <-createDone:
		if created {
			t.Fatal("known failed create unexpectedly succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("creation rollback did not finish after transport drain")
	}
	select {
	case <-submitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("rollback drain did not release the in-flight OUT")
	}
	events := xbox360TraceEvents(traceSink)
	if len(events) != 4 || events[0]["Event"] != "X360RumbleTraceStart" || events[1]["Event"] != "X360RumbleRaw" || events[2]["Event"] != "X360RumbleParsed" || events[3]["Event"] != "X360RumbleTraceAbort" {
		t.Fatalf("rollback trace event order = %+v", events)
	}
	if events[3]["LastTraceSeq"] != uint64(1) || events[3]["Reason"] != "auto-attach-failure" {
		t.Fatalf("rollback terminal marker = %+v", events[3])
	}
	assertXbox360TraceProcessIdentity(t, events)
}

func TestCanonicalXbox360TraceDoesNotAbortUnknownRetainedCreation(t *testing.T) {
	traceSink := &xbox360TraceCapture{}
	setRumbleTraceSinkForTest(t, traceSink)
	hw, _ := newLifecycleTestServer(t, 10213)
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		return api.LocalhostAttachment{}, api.ErrAttachmentOutcomeUnknown
	}
	serverHandle := diagnosticServerHandle(t, hw)
	var handle deviceHandle
	if createXbox360Device(serverHandle, &handle, 10213, true, 0, 0, 0) {
		t.Fatal("unsafe attachment outcome unexpectedly returned create success")
	}
	if hw.state != serverCloseFailed || len(hw.deviceHandles[10213]) != 1 {
		t.Fatalf("unknown outcome state/retained handles = %s / %d", hw.state, len(hw.deviceHandles[10213]))
	}
	events := xbox360TraceEvents(traceSink)
	if len(events) != 1 || events[0]["Event"] != "X360RumbleTraceStart" {
		t.Fatalf("unknown retained creation emitted terminal trace: %+v", events)
	}
}

func TestCanonicalXbox360TraceDistinguishesMultipleDevices(t *testing.T) {
	traceSink := &xbox360TraceCapture{}
	setRumbleTraceSinkForTest(t, traceSink)
	hw, _ := newLifecycleTestServer(t, 10214)
	firstHandle, first := createXbox360ForTraceTest(t, hw, 10214, false)
	secondHandle, second := createXbox360ForTraceTest(t, hw, 10214, false)
	first.HandleTransfer(context.Background(), 1, usbip.DirOut, []byte{0, 8, 0, 1, 2, 0, 0, 0})
	second.HandleTransfer(context.Background(), 1, usbip.DirOut, []byte{0, 8, 0, 3, 4, 0, 0, 0})
	seen := map[string]map[string]bool{}
	sessions := make(map[string]string)
	events := xbox360TraceEvents(traceSink)
	assertXbox360TraceProcessIdentity(t, events)
	for _, event := range events {
		deviceID := fmt.Sprint(event["DeviceID"])
		if event["Event"] == "X360RumbleTraceStart" {
			sessions[deviceID] = fmt.Sprint(event["TraceSessionID"])
		}
		if event["Event"] == "X360RumbleRaw" || event["Event"] == "X360RumbleParsed" {
			if seen[deviceID] == nil {
				seen[deviceID] = make(map[string]bool)
			}
			id := fmt.Sprint(event["TraceSessionID"])
			if event["TraceSeq"] != uint64(1) || id != sessions[deviceID] {
				t.Fatalf("device %s record escaped its trace session: %+v", deviceID, event)
			}
			seen[deviceID][id] = true
		}
	}
	if len(seen["1"]) != 1 || len(seen["2"]) != 1 || sessions["1"] == sessions["2"] {
		t.Fatalf("per-device trace identities/sequences = %+v", seen)
	}
	if got := removeXbox360DeviceResult(uintptr(firstHandle)); got != typedDeviceRemoveSuccess {
		t.Fatalf("first remove result = %d", got)
	}
	if got := removeXbox360DeviceResult(uintptr(secondHandle)); got != typedDeviceRemoveSuccess {
		t.Fatalf("second remove result = %d", got)
	}
}

func TestCanonicalXbox360TraceFailedRemovalDoesNotEndRetainedDevice(t *testing.T) {
	traceSink := &xbox360TraceCapture{}
	setRumbleTraceSinkForTest(t, traceSink)
	hw, _ := newLifecycleTestServer(t, 10215)
	handle, pad := createXbox360ForTraceTest(t, hw, 10215, false)
	hw.ops.removeDevice = func(*serverusb.Server, uint32, string) error { return errors.New("known remove failure") }
	if got := removeXbox360DeviceResult(uintptr(handle)); got != typedDeviceRemoveRetryableFailure {
		t.Fatalf("failed remove result = %d", got)
	}
	if pad.RumbleTraceSession() == nil || !lookupIdentityExists(uintptr(handle)) {
		t.Fatal("failed logical removal did not retain the active device trace")
	}
	for _, event := range xbox360TraceEvents(traceSink) {
		if event["Event"] == "X360RumbleTraceEnd" {
			t.Fatal("failed removal emitted a premature TraceEnd")
		}
	}
	hw.ops.removeDevice = defaultServerOperations().removeDevice
	if got := removeXbox360DeviceResult(uintptr(handle)); got != typedDeviceRemoveSuccess {
		t.Fatalf("retry remove result = %d", got)
	}
}

func startXbox360TraceTransport(t *testing.T, hw *usbServerHandleWrapper) {
	t.Helper()
	serveDone := make(chan error, 1)
	go func() { serveDone <- hw.s.ListenAndServe() }()
	select {
	case <-hw.s.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("USB/IP server did not become ready")
	}
	t.Cleanup(func() {
		_ = hw.s.Close()
		select {
		case err := <-serveDone:
			if err != nil {
				t.Errorf("USB/IP serve loop returned %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("USB/IP serve loop did not stop")
		}
	})
}

func awaitXbox360TraceSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestCanonicalXbox360TraceEndWaitsForTransportDrainOnAllRemovalPaths(t *testing.T) {
	paths := []string{"typed-device", "bus", "server-close"}
	for i, path := range paths {
		t.Run(path, func(t *testing.T) {
			busID := uint32(10220 + i)
			traceSink := &xbox360TraceCapture{}
			setRumbleTraceSinkForTest(t, traceSink)
			hw, _ := newLifecycleTestServer(t, busID)
			handle, pad := createXbox360ForTraceTest(t, hw, busID, false)
			traceEntered, releaseTrace := traceSink.blockNext("X360RumbleRaw")
			defer releaseTrace()
			pad.SetRumbleCallback(func(xbox360.XRumbleState) {})
			finalized := make(chan struct{})
			oldDelete := hw.ops.deleteHandle
			hw.ops.deleteHandle = func(h cgo.Handle) {
				oldDelete(h)
				close(finalized)
			}
			startXbox360TraceTransport(t, hw)
			serverHandle := diagnosticServerHandle(t, hw)
			client := testusbip.NewUsbIpClient(t, hw.s.Addr())
			imported, err := client.AttachDevice(fmt.Sprintf("%d-1", busID))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = imported.Conn.Close() })
			submitDone := make(chan error, 1)
			go func() {
				submitDone <- client.SubmitWithTimeout(imported.Conn, usbip.DirOut, 1, []byte{0, 8, 0, 0, 0, 0, 0, 0}, nil, 10*time.Second)
			}()
			awaitXbox360TraceSignal(t, traceEntered, "in-flight raw trace record")

			retirementDone := make(chan bool, 1)
			go func() {
				switch path {
				case "typed-device":
					retirementDone <- removeXbox360DeviceResult(uintptr(handle)) == typedDeviceRemoveSuccess
				case "bus":
					retirementDone <- callRemoveUSBBusForTest(serverHandle, busID)
				case "server-close":
					retirementDone <- callCloseUSBServerForTest(serverHandle)
				}
			}()
			awaitXbox360TraceSignal(t, finalized, "logical handle finalization")
			if pad.RumbleTraceSession() == nil {
				t.Fatal("trace context was not retained after logical-handle finalization")
			}
			for _, event := range xbox360TraceEvents(traceSink) {
				if event["Event"] == "X360RumbleTraceEnd" {
					t.Fatal("TraceEnd was emitted before the in-flight transfer left the drain window")
				}
			}
			releaseTrace()
			select {
			case <-submitDone: // A closed transport may make the client observe its expected disconnect.
			case <-time.After(5 * time.Second):
				t.Fatal("in-flight USB/IP OUT did not leave the transport")
			}
			select {
			case ok := <-retirementDone:
				if !ok {
					t.Fatalf("%s retirement failed", path)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("%s retirement did not finish after drain", path)
			}
			events := xbox360TraceEvents(traceSink)
			endIndex := -1
			for i, event := range events {
				if event["Event"] == "X360RumbleTraceEnd" {
					endIndex = i
					if event["LastTraceSeq"] != uint64(1) {
						t.Fatalf("TraceEnd LastTraceSeq = %v, want 1", event["LastTraceSeq"])
					}
				}
			}
			if endIndex < 0 {
				t.Fatal("successfully retired device has no TraceEnd")
			}
			if pad.RumbleTraceSession() != nil {
				t.Fatal("trace context was not released after its drain-fenced end marker")
			}
			for _, event := range events[endIndex+1:] {
				if event["Event"] == "X360RumbleRaw" || event["Event"] == "X360RumbleParsed" || event["Event"] == "X360RumbleCallbackDispatch" {
					t.Fatalf("packet trace appeared after TraceEnd: %+v", event)
				}
			}
		})
	}
}

func TestCanonicalXbox360TraceSeparatesReusedDeviceIDDuringPriorDrain(t *testing.T) {
	const busID = uint32(10230)
	traceSink := &xbox360TraceCapture{}
	setRumbleTraceSinkForTest(t, traceSink)
	hw, _ := newLifecycleTestServer(t, busID)
	handleA, padA := createXbox360ForTraceTest(t, hw, busID, false)
	traceEntered, releaseTrace := traceSink.blockNext("X360RumbleRaw")
	defer releaseTrace()
	padA.SetRumbleCallback(func(xbox360.XRumbleState) {})
	finalized := make(chan struct{})
	oldDelete := hw.ops.deleteHandle
	var finalizedOnce sync.Once
	hw.ops.deleteHandle = func(h cgo.Handle) {
		oldDelete(h)
		finalizedOnce.Do(func() { close(finalized) })
	}
	startXbox360TraceTransport(t, hw)
	clientA := testusbip.NewUsbIpClient(t, hw.s.Addr())
	importA, err := clientA.AttachDevice(fmt.Sprintf("%d-1", busID))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = importA.Conn.Close() })
	submitA := make(chan error, 1)
	go func() {
		submitA <- clientA.SubmitWithTimeout(importA.Conn, usbip.DirOut, 1, []byte{0, 8, 0, 0, 0, 0, 0, 0}, nil, 10*time.Second)
	}()
	awaitXbox360TraceSignal(t, traceEntered, "session A raw trace record")
	removeA := make(chan typedDeviceRemoveResult, 1)
	go func() { removeA <- removeXbox360DeviceResult(uintptr(handleA)) }()
	awaitXbox360TraceSignal(t, finalized, "session A logical finalization")
	if padA.RumbleTraceSession() == nil {
		t.Fatal("session A trace context was released before its drain completed")
	}
	for _, event := range xbox360TraceEvents(traceSink) {
		if event["Event"] == "X360RumbleTraceEnd" {
			t.Fatal("session A ended while its managed transfer was still in flight")
		}
	}

	handleB, _ := createXbox360ForTraceTest(t, hw, busID, false)
	if got := hw.deviceHandleRecords[handleB].exportMeta.DevID; got != 1 {
		t.Fatalf("reused DeviceID = %d, want 1", got)
	}
	clientB := testusbip.NewUsbIpClient(t, hw.s.Addr())
	importB, err := clientB.AttachDevice(fmt.Sprintf("%d-1", busID))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = importB.Conn.Close() })
	if err := clientB.Submit(importB.Conn, usbip.DirOut, 1, []byte{0, 8, 0, 7, 9, 0, 0, 0}, nil); err != nil {
		t.Fatalf("session B OUT failed while session A drained: %v", err)
	}

	var startA, startB map[string]any
	for _, event := range xbox360TraceEvents(traceSink) {
		if event["Event"] != "X360RumbleTraceStart" {
			continue
		}
		if startA == nil {
			startA = event
		} else {
			startB = event
		}
	}
	if startA == nil || startB == nil || fmt.Sprint(startA["BusID"]) != fmt.Sprint(startB["BusID"]) || fmt.Sprint(startA["DeviceID"]) != fmt.Sprint(startB["DeviceID"]) || startA["TraceSessionID"] == startB["TraceSessionID"] {
		t.Fatalf("reused logical identity did not receive distinct sessions: A=%+v B=%+v", startA, startB)
	}
	events := xbox360TraceEvents(traceSink)
	for _, event := range events {
		if fmt.Sprint(event["TraceSessionID"]) == fmt.Sprint(startB["TraceSessionID"]) && (event["Event"] == "X360RumbleRaw" || event["Event"] == "X360RumbleParsed") && event["TraceSeq"] != uint64(1) {
			t.Fatalf("session B sequence did not start independently: %+v", event)
		}
	}
	releaseTrace()
	select {
	case <-submitA: // Expected disconnect after the removal began its drain.
	case <-time.After(5 * time.Second):
		t.Fatal("session A transfer did not leave the drain")
	}
	select {
	case result := <-removeA:
		if result != typedDeviceRemoveSuccess {
			t.Fatalf("session A removal result = %d", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session A drain did not complete")
	}
	events = xbox360TraceEvents(traceSink)
	endA := -1
	lastPacketA := -1
	for i, event := range events {
		if fmt.Sprint(event["TraceSessionID"]) != fmt.Sprint(startA["TraceSessionID"]) {
			continue
		}
		switch event["Event"] {
		case "X360RumbleRaw", "X360RumbleParsed", "X360RumbleCallbackDispatch":
			lastPacketA = i
		case "X360RumbleTraceEnd":
			endA = i
			if event["LastTraceSeq"] != uint64(1) {
				t.Fatalf("session A final sequence = %v, want 1", event["LastTraceSeq"])
			}
		}
	}
	if endA < 0 || lastPacketA < 0 || endA <= lastPacketA {
		t.Fatalf("session A final packet/end order = %d/%d", lastPacketA, endA)
	}
	if padA.RumbleTraceSession() != nil {
		t.Fatal("session A trace context remains attached after TraceEnd")
	}
	if got := removeXbox360DeviceResult(uintptr(handleB)); got != typedDeviceRemoveSuccess {
		t.Fatalf("session B remove result = %d", got)
	}
	events = xbox360TraceEvents(traceSink)
	for _, check := range []struct {
		start      map[string]any
		wantEvents []string
	}{
		{startA, []string{"X360RumbleTraceStart", "X360RumbleRaw", "X360RumbleParsed", "X360RumbleCallbackDispatch", "X360RumbleTraceEnd"}},
		{startB, []string{"X360RumbleTraceStart", "X360RumbleRaw", "X360RumbleParsed", "X360RumbleTraceEnd"}},
	} {
		var sessionEvents []map[string]any
		for _, event := range events {
			if fmt.Sprint(event["TraceSessionID"]) == fmt.Sprint(check.start["TraceSessionID"]) {
				sessionEvents = append(sessionEvents, event)
			}
		}
		if len(sessionEvents) != len(check.wantEvents) {
			t.Fatalf("session %+v event set = %+v", check.start, sessionEvents)
		}
		for i, event := range sessionEvents {
			if event["Event"] != check.wantEvents[i] || fmt.Sprint(event["BusID"]) != fmt.Sprint(check.start["BusID"]) || fmt.Sprint(event["DeviceID"]) != fmt.Sprint(check.start["DeviceID"]) {
				t.Fatalf("session %+v event %d = %+v", check.start, i, event)
			}
			if i > 0 && i < len(sessionEvents)-1 && event["TraceSeq"] != uint64(1) {
				t.Fatalf("session %+v packet sequence = %+v", check.start, event)
			}
		}
		if sessionEvents[len(sessionEvents)-1]["LastTraceSeq"] != uint64(1) {
			t.Fatalf("session %+v end fence = %+v", check.start, sessionEvents[len(sessionEvents)-1])
		}
	}
}
