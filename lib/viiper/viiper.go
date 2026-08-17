package main

import "C"
import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/cgo"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Alia5/VIIPER/device"
	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/internal/server/usb"
	viiperusb "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/Alia5/VIIPER/virtualbus"
)

func main() {}

func goStringOrEmpty(p *C.char) string {
	if p == nil {
		return ""
	}
	return C.GoString(p)
}

type deviceHandle cgo.Handle

type serverLifecycleState uint8

const (
	serverActive serverLifecycleState = iota
	serverClosing
	serverCloseFailed
	serverClosed
)

type serverOperations struct {
	attachLocalhostTracked func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error)
	detachLocalhost        func(context.Context, api.LocalhostAttachment, *slog.Logger) error
	rollbackDevice         func(*virtualbus.VirtualBus, viiperusb.Device) error
	removeDevice           func(*usb.Server, uint32, string) error
	removeBus              func(*usb.Server, uint32) error
	close                  func(*usb.Server) error
	deleteHandle           func(cgo.Handle)
}

func defaultServerOperations() serverOperations {
	return serverOperations{
		attachLocalhostTracked: api.AttachLocalhostClientTracked,
		detachLocalhost:        api.DetachLocalhostClient,
		rollbackDevice:         func(bus *virtualbus.VirtualBus, dev viiperusb.Device) error { return bus.Remove(dev) },
		removeDevice: func(s *usb.Server, busID uint32, deviceID string) error {
			return s.RemoveDeviceByIDWithoutBusCleanup(busID, deviceID)
		},
		removeBus:    func(s *usb.Server, busID uint32) error { return s.RemoveBus(busID) },
		close:        func(s *usb.Server) error { return s.Close() },
		deleteHandle: func(h cgo.Handle) { h.Delete() },
	}
}

type usbServerHandleWrapper struct {
	s                          *usb.Server
	lifecycleMu                sync.Mutex
	mtx                        sync.Mutex // Legacy wrapper synchronization; lifecycleMu gates mutations.
	state                      serverLifecycleState
	deviceHandles              map[uint32][]deviceHandle
	deviceHandleRecords        map[deviceHandle]*deviceHandleWrapper
	ops                        serverOperations
	logger                     *slog.Logger
	rejectionWarnings          map[string]bool
	onCallbackCleared          func(*deviceHandleWrapper)
	onLifecycleLockAttempt     func(operation string)
	closePhase                 canonicalClosePhase
	logicalCloseInProgress     bool
	backendLogLogger           *slog.Logger
	onAttachmentTimingSnapshot func(totalUs int64)
}

// notifyLifecycleLockAttempt is a nil-by-default, behavior-neutral seam used only by
// deterministic lifecycle tests. It runs immediately before the owning server lock is acquired.
func notifyLifecycleLockAttempt(hw *usbServerHandleWrapper, operation string) {
	if hw.onLifecycleLockAttempt != nil {
		hw.onLifecycleLockAttempt(operation)
	}
}

type canonicalClosePhase uint8

const (
	logicalTeardownPending canonicalClosePhase = iota
	transportClosePending
	closeComplete
)

type transportTeardownResult struct {
	ok                  bool
	drains              []*usb.TransportDrain
	diagnostic          teardownDiagnostic
	detachBackendCalled bool
	backendLogs         *deferredLogBatch
}

type teardownDiagnostic struct {
	operation                  string
	phase                      string
	result                     string
	busID                      uint32
	deviceID                   string
	attachmentStateBefore      string
	attachmentStateAfter       string
	serverStateBefore          serverLifecycleState
	serverStateAfter           serverLifecycleState
	serverStatePresent         bool
	busCountBefore             int
	busCountPresent            bool
	attachmentBackend          api.LocalhostAttachmentBackend
	importPort                 int32
	detachBackendCalled        bool
	detachBackendCalledPresent bool
	deviceCountBefore          int
	remainingBusCount          int
	unknownAttachmentCount     int
	backendReportedError       bool
	busPresentAfter            bool
	error                      string
}

func teardownResultLabel(result typedDeviceRemoveResult) string {
	switch result {
	case typedDeviceRemoveSuccess:
		return "success"
	case typedDeviceRemoveRetryableFailure:
		return "retryable-failure"
	case typedDeviceRemoveUnsafeOutcomeUnknown:
		return "unsafe-outcome-unknown"
	default:
		return "invalid"
	}
}

func teardownResultLogLevel(result string) slog.Level {
	switch result {
	case "success":
		return slog.LevelInfo
	case "retryable-failure":
		return slog.LevelWarn
	case "unsafe-outcome-unknown":
		return slog.LevelError
	default:
		return slog.LevelDebug
	}
}

func logTeardownDiagnostic(logger *slog.Logger, d teardownDiagnostic, totalUs int64) {
	args := []any{"operation", d.operation, "layer", "canonical", "result", d.result, "phase", d.phase, "totalUs", totalUs}
	if d.serverStatePresent {
		args = append(args, "serverStateBefore", d.serverStateBefore.String(), "serverStateAfter", d.serverStateAfter.String())
	}
	if d.busCountPresent {
		args = append(args, "busCountBefore", d.busCountBefore)
	}
	args = append(args, "remainingBusCount", d.remainingBusCount)
	if d.busID != 0 {
		args = append(args, "busID", d.busID)
	}
	if d.deviceID != "" {
		args = append(args, "deviceID", d.deviceID)
	}
	if d.attachmentStateBefore != "" {
		args = append(args, "attachmentStateBefore", d.attachmentStateBefore, "attachmentStateAfter", d.attachmentStateAfter)
	}
	args = append(args, "attachmentBackend", d.attachmentBackend, "importPort", d.importPort)
	if d.detachBackendCalledPresent {
		args = append(args, "detachBackendCalled", d.detachBackendCalled)
	}
	if d.deviceCountBefore != 0 {
		args = append(args, "deviceCountBefore", d.deviceCountBefore)
	}
	if d.unknownAttachmentCount != 0 {
		args = append(args, "unknownAttachmentCount", d.unknownAttachmentCount)
	}
	if d.backendReportedError {
		args = append(args, "backendReportedError", true, "busPresentAfter", d.busPresentAfter)
	}
	if d.error != "" {
		args = append(args, "error", d.error)
	}
	logger.Log(context.Background(), teardownResultLogLevel(d.result), d.operation+" teardown", args...)
}

type typedDeviceRemoveResult uint8

const (
	typedDeviceRemoveSuccess typedDeviceRemoveResult = iota
	typedDeviceRemoveRetryableFailure
	typedDeviceRemoveUnsafeOutcomeUnknown
	typedDeviceRemoveInvalid
)

type deviceAttachResult uint8

const (
	deviceAttachSuccess deviceAttachResult = iota
	deviceAttachRetryableFailure
	deviceAttachUnsafeOutcomeUnknown
	deviceAttachInvalid
)

type deviceDetachResult uint8

const (
	deviceDetachSuccess deviceDetachResult = iota
	deviceDetachRetryableFailure
	deviceDetachUnsafeOutcomeUnknown
	deviceDetachInvalid
)

func waitTransportDrains(drains []*usb.TransportDrain) {
	for _, drain := range drains {
		if drain != nil {
			drain.Wait()
		}
	}
}

type deviceHandleWrapper struct {
	device     any
	exportMeta *usbip.ExportMeta
	usbServer  *usbServerHandleWrapper
	attachment deviceAttachmentRecord
}

type deviceAttachmentState uint8

const (
	attachmentDetached deviceAttachmentState = iota
	attachmentAttached
	attachmentOutcomeUnknown
)

type deviceAttachmentRecord struct {
	state      deviceAttachmentState
	attachment api.LocalhostAttachment
}

var serverHandleRecords sync.Map // map[uintptr]*usbServerHandleWrapper
var deviceHandleRecords sync.Map // map[uintptr]*deviceHandleWrapper

func lookupServerHandle(raw uintptr) (*usbServerHandleWrapper, bool) {
	v, ok := serverHandleRecords.Load(raw)
	if !ok {
		return nil, false
	}
	hw, ok := v.(*usbServerHandleWrapper)
	return hw, ok
}

func lookupDeviceIdentity(raw uintptr) (*deviceHandleWrapper, bool) {
	v, ok := deviceHandleRecords.Load(raw)
	if !ok {
		return nil, false
	}
	dhw, ok := v.(*deviceHandleWrapper)
	if !ok {
		return nil, false
	}

	hw := dhw.usbServer
	hw.lifecycleMu.Lock()
	defer hw.lifecycleMu.Unlock()
	if hw.state != serverActive && hw.state != serverCloseFailed {
		return nil, false
	}
	if hw.deviceHandleRecords[deviceHandle(raw)] != dhw {
		return nil, false
	}
	return dhw, true
}

func withActiveDeviceHandle(raw uintptr, action func(*deviceHandleWrapper) bool) bool {
	v, ok := deviceHandleRecords.Load(raw)
	if !ok {
		return false
	}
	dhw, ok := v.(*deviceHandleWrapper)
	if !ok {
		return false
	}

	hw := dhw.usbServer
	hw.lifecycleMu.Lock()
	if hw.state != serverActive || hw.deviceHandleRecords[deviceHandle(raw)] != dhw {
		warning := hw.takeMutationRejectedWarningLocked("typed-device-mutation")
		hw.lifecycleMu.Unlock()
		emitMutationRejectedWarning(warning)
		return false
	}
	defer hw.lifecycleMu.Unlock()
	return action(dhw)
}

func (hw *usbServerHandleWrapper) createDeviceLocked(busID uint32, dev viiperusb.Device, autoAttach bool) (deviceHandle, bool) {
	h, ok, _, _, _ := hw.createDeviceLockedPublic(busID, dev, autoAttach)
	hw.backendLogLogger = nil
	return h, ok
}

func (hw *usbServerHandleWrapper) createDeviceLockedPublic(busID uint32, dev viiperusb.Device, autoAttach bool) (deviceHandle, bool, mutationRejectedWarning, *rollbackDiagnostic, *deferredLogBatch) {
	backendLogs := newDeferredLogBatch()
	hw.backendLogLogger = backendLogs.logger
	if hw.state != serverActive {
		return 0, false, hw.takeMutationRejectedWarningLocked("typed-device-create"), nil, backendLogs
	}
	bus := hw.s.GetBus(busID)
	if bus == nil {
		return 0, false, mutationRejectedWarning{}, nil, backendLogs
	}

	devCtx, err := bus.Add(dev)
	if err != nil {
		return 0, false, mutationRejectedWarning{}, nil, backendLogs
	}
	exportMeta := device.GetDeviceMeta(devCtx)
	if exportMeta == nil {
		_, rollback := hw.rollbackCreatedDeviceLockedWithDiagnostic(busID, 0, func(d viiperusb.Device) error { return hw.ops.rollbackDevice(bus, d) }, dev, "device metadata was unavailable")
		return 0, false, mutationRejectedWarning{}, rollback, backendLogs
	}
	dhw := &deviceHandleWrapper{device: dev, exportMeta: exportMeta, usbServer: hw, attachment: deviceAttachmentRecord{state: attachmentDetached}}
	h := hw.registerDeviceLocked(dhw)
	if autoAttach {
		if !hw.attachDeviceLocked(dhw) {
			if dhw.attachment.state == attachmentOutcomeUnknown {
				hw.state = serverCloseFailed
				return h, false, mutationRejectedWarning{}, nil, backendLogs
			}
			if ok, rollback := hw.rollbackCreatedDeviceLockedWithDiagnostic(exportMeta.BusID, exportMeta.DevID, func(d viiperusb.Device) error { return hw.ops.rollbackDevice(bus, d) }, dev, "auto-attach failure"); !ok {
				return h, false, mutationRejectedWarning{}, rollback, backendLogs
			}
			hw.finalizeDeviceLocked(h)
			return 0, false, mutationRejectedWarning{}, nil, backendLogs
		}
	}
	return h, true, mutationRejectedWarning{}, nil, backendLogs
}

func (hw *usbServerHandleWrapper) registerDeviceLocked(dhw *deviceHandleWrapper) deviceHandle {
	h := deviceHandle(cgo.NewHandle(dhw))
	hw.deviceHandles[dhw.exportMeta.BusID] = append(hw.deviceHandles[dhw.exportMeta.BusID], h)
	hw.deviceHandleRecords[h] = dhw
	deviceHandleRecords.Store(uintptr(h), dhw)
	return h
}

// operationTiming carries behavior-neutral diagnostic durations out of the classified
// attach/detach resolvers. It is purely additive: a nil *operationTiming means "caller does not
// need timing," and every field defaults to its zero value on any path that never reaches the
// backend, so a caller never has to distinguish "not measured" from "measured as zero" itself.
type operationTiming struct {
	backendUs     int64
	backendCalled bool
}

func (hw *usbServerHandleWrapper) attachDeviceLocked(dhw *deviceHandleWrapper) bool {
	return hw.attachDeviceLockedResult(dhw, nil) == deviceAttachSuccess
}

// attachDeviceLockedResult is the single classified attach implementation. Both the legacy bool
// AttachUSBDevice and the classified AttachUSBDeviceEx export call this same operation; the bool
// export never performs an independent second mutation attempt. timing is optional and purely
// diagnostic: it is filled in only around the actual backend call and never changes control flow,
// the returned result, or any stored attachment/server state.
func (hw *usbServerHandleWrapper) attachDeviceLockedResult(dhw *deviceHandleWrapper, timing *operationTiming) deviceAttachResult {
	if !hw.s.DeviceTransportCanAccept(dhw.device.(viiperusb.Device)) {
		return deviceAttachRetryableFailure
	}
	switch dhw.attachment.state {
	case attachmentAttached:
		return deviceAttachSuccess
	case attachmentOutcomeUnknown:
		// Ownership evidence is already unsafe/unknown; do not invoke the backend again or
		// downgrade this to an invalid-handle result.
		return deviceAttachUnsafeOutcomeUnknown
	}
	backendStart := time.Now()
	attachment, err := hw.ops.attachLocalhostTracked(context.Background(), dhw.exportMeta, hw.s.GetListenPort(), true, hw.backendLoggerLocked())
	if timing != nil {
		timing.backendUs = time.Since(backendStart).Microseconds()
		timing.backendCalled = true
	}
	if err == nil && isValidLocalhostAttachment(attachment) {
		dhw.attachment = deviceAttachmentRecord{state: attachmentAttached, attachment: attachment}
		return deviceAttachSuccess
	}
	if err == nil || errors.Is(err, api.ErrAttachmentOutcomeUnknown) {
		dhw.attachment.state = attachmentOutcomeUnknown
		hw.state = serverCloseFailed
		return deviceAttachUnsafeOutcomeUnknown
	}
	return deviceAttachRetryableFailure
}

func isValidLocalhostAttachment(attachment api.LocalhostAttachment) bool {
	if attachment.Port <= 0 {
		return false
	}
	switch attachment.Backend {
	case api.LocalhostAttachmentBackendNativeIOCTL, api.LocalhostAttachmentBackendCommand:
		return true
	default:
		return false
	}
}

func (hw *usbServerHandleWrapper) detachDeviceLocked(dhw *deviceHandleWrapper) bool {
	return hw.detachDeviceLockedResult(dhw, nil) == deviceDetachSuccess
}

// detachDeviceLockedResult is the single classified detach implementation. Both the legacy bool
// DetachUSBDevice and the classified DetachUSBDeviceEx export call this same operation; the bool
// export never performs an independent second mutation attempt. timing follows the same
// optional/diagnostic-only contract as in attachDeviceLockedResult.
func (hw *usbServerHandleWrapper) detachDeviceLockedResult(dhw *deviceHandleWrapper, timing *operationTiming) deviceDetachResult {
	switch dhw.attachment.state {
	case attachmentDetached:
		return deviceDetachSuccess
	case attachmentOutcomeUnknown:
		// Ownership evidence is already unsafe/unknown; do not invoke the backend again or
		// downgrade this to an invalid-handle result.
		return deviceDetachUnsafeOutcomeUnknown
	}
	backendStart := time.Now()
	err := hw.ops.detachLocalhost(context.Background(), dhw.attachment.attachment, hw.backendLoggerLocked())
	if timing != nil {
		timing.backendUs = time.Since(backendStart).Microseconds()
		timing.backendCalled = true
	}
	if err == nil {
		dhw.attachment = deviceAttachmentRecord{state: attachmentDetached}
		return deviceDetachSuccess
	}
	if errors.Is(err, api.ErrDetachmentOutcomeUnknown) {
		dhw.attachment.state = attachmentOutcomeUnknown
		hw.state = serverCloseFailed
		return deviceDetachUnsafeOutcomeUnknown
	}
	return deviceDetachRetryableFailure
}

func (hw *usbServerHandleWrapper) backendLoggerLocked() *slog.Logger {
	if hw.backendLogLogger != nil {
		return hw.backendLogLogger
	}
	return hw.logger
}

func (hw *usbServerHandleWrapper) removeDeviceLocked(dhw *deviceHandleWrapper, h deviceHandle) bool {
	result := hw.removeDeviceLockedWithDrain(dhw, h)
	return result.ok
}

func (hw *usbServerHandleWrapper) removeDeviceLockedWithDrain(dhw *deviceHandleWrapper, h deviceHandle) transportTeardownResult {
	hw.clearDeviceCallbackLocked(dhw)
	var timing operationTiming
	if hw.detachDeviceLockedResult(dhw, &timing) != deviceDetachSuccess {
		return transportTeardownResult{detachBackendCalled: timing.backendCalled}
	}
	drain := hw.s.BeginDeviceDrain(dhw.device.(viiperusb.Device))
	if err := hw.ops.removeDevice(hw.s, dhw.exportMeta.BusID, fmt.Sprintf("%d", dhw.exportMeta.DevID)); err != nil {
		return transportTeardownResult{drains: []*usb.TransportDrain{drain}, detachBackendCalled: timing.backendCalled}
	}
	hw.finalizeDeviceLocked(h)
	hw.s.ForgetDeviceTransport(dhw.device.(viiperusb.Device))
	return transportTeardownResult{ok: true, drains: []*usb.TransportDrain{drain}, detachBackendCalled: timing.backendCalled}
}

func removeTypedDevice(handle uintptr, valid func(any) bool) bool {
	return removeTypedDeviceResult(handle, valid) == typedDeviceRemoveSuccess
}

func removeTypedDeviceResult(handle uintptr, valid func(any) bool) typedDeviceRemoveResult {
	opStart := time.Now()
	v, ok := deviceHandleRecords.Load(handle)
	if !ok {
		logTeardownDiagnostic(invalidHandleLoggerFunc(), teardownDiagnostic{operation: "typed-device-remove", phase: "preflight", result: "invalid"}, time.Since(opStart).Microseconds())
		return typedDeviceRemoveInvalid
	}
	dhw, ok := v.(*deviceHandleWrapper)
	if !ok {
		logTeardownDiagnostic(invalidHandleLoggerFunc(), teardownDiagnostic{operation: "typed-device-remove", phase: "preflight", result: "invalid"}, time.Since(opStart).Microseconds())
		return typedDeviceRemoveInvalid
	}
	hw := dhw.usbServer
	notifyLifecycleLockAttempt(hw, "remove")
	hw.lifecycleMu.Lock()
	backendLogs := newDeferredLogBatch()
	hw.backendLogLogger = backendLogs.logger
	stateBefore := dhw.attachment.state
	token := dhw.attachment.attachment
	d := teardownDiagnostic{operation: "typed-device-remove", phase: "preflight", serverStateBefore: hw.state, serverStateAfter: hw.state,
		busID: dhw.exportMeta.BusID, deviceID: fmt.Sprintf("%d", dhw.exportMeta.DevID), attachmentStateBefore: attachmentStateName(stateBefore), attachmentStateAfter: attachmentStateName(stateBefore),
		attachmentBackend: token.Backend, importPort: token.Port}
	d.serverStatePresent = true
	d.detachBackendCalledPresent = true
	if hw.deviceHandleRecords[deviceHandle(handle)] != dhw || !valid(dhw.device) {
		hw.backendLogLogger = nil
		hw.lifecycleMu.Unlock()
		backendLogs.replay(hw.logger)
		d.result = "invalid"
		logTeardownDiagnostic(hw.logger, d, time.Since(opStart).Microseconds())
		return typedDeviceRemoveInvalid
	}
	if dhw.attachment.state == attachmentOutcomeUnknown {
		hw.backendLogLogger = nil
		hw.lifecycleMu.Unlock()
		backendLogs.replay(hw.logger)
		d.result = "unsafe-outcome-unknown"
		d.phase = "detach"
		logTeardownDiagnostic(hw.logger, d, time.Since(opStart).Microseconds())
		return typedDeviceRemoveUnsafeOutcomeUnknown
	}
	if hw.state != serverActive {
		hw.backendLogLogger = nil
		hw.lifecycleMu.Unlock()
		backendLogs.replay(hw.logger)
		d.result = "invalid"
		logTeardownDiagnostic(hw.logger, d, time.Since(opStart).Microseconds())
		return typedDeviceRemoveInvalid
	}
	result := hw.removeDeviceLockedWithDrain(dhw, deviceHandle(handle))
	result.backendLogs = backendLogs
	unsafeOutcome := dhw.attachment.state == attachmentOutcomeUnknown || hw.state == serverCloseFailed
	d.attachmentStateAfter = attachmentStateName(dhw.attachment.state)
	d.serverStateAfter = hw.state
	d.detachBackendCalled = result.detachBackendCalled
	d.result = "success"
	d.phase = "complete"
	if !result.ok {
		if unsafeOutcome {
			d.result = "unsafe-outcome-unknown"
		} else {
			d.result = "retryable-failure"
		}
		if dhw.attachment.state == attachmentOutcomeUnknown || dhw.attachment.state == attachmentAttached {
			d.phase = "detach"
		} else {
			d.phase = "logical-remove"
		}
	}
	hw.backendLogLogger = nil
	hw.lifecycleMu.Unlock()
	waitTransportDrains(result.drains)
	operationTotalUs := time.Since(opStart).Microseconds()
	backendLogs.replay(hw.logger)
	logTeardownDiagnostic(hw.logger, d, operationTotalUs)
	if result.ok {
		return typedDeviceRemoveSuccess
	}
	if unsafeOutcome {
		return typedDeviceRemoveUnsafeOutcomeUnknown
	}
	return typedDeviceRemoveRetryableFailure
}

func attachResultTimingLabel(result deviceAttachResult) string {
	switch result {
	case deviceAttachSuccess:
		return "success"
	case deviceAttachRetryableFailure:
		return "retryable-failure"
	case deviceAttachUnsafeOutcomeUnknown:
		return "unsafe-outcome-unknown"
	default:
		return "invalid"
	}
}

func detachResultTimingLabel(result deviceDetachResult) string {
	switch result {
	case deviceDetachSuccess:
		return "success"
	case deviceDetachRetryableFailure:
		return "retryable-failure"
	case deviceDetachUnsafeOutcomeUnknown:
		return "unsafe-outcome-unknown"
	default:
		return "invalid"
	}
}

// logCanonicalAttachmentTiming emits one behavior-neutral "attachment-timing" summary per
// canonical Attach/Detach operation (layer=canonical), covering both the bool and classified Ex
// exports since they share this exact resolver. It is diagnostic-only and must always be called
// after hw.lifecycleMu has been released: the funcLogHandler bridge invokes the embedding
// consumer's C log callback synchronously, so logging while still holding the lock would let a
// slow/reentrant callback stall unrelated lifecycle operations (or deadlock one that re-enters a
// VIIPER lifecycle API) and would pollute the very lockWaitUs measurement this exists to produce.
// Fields follow the stable diagnostic vocabulary used by this instrumentation: operation, layer,
// result, backend, backendCalled, totalUs, lockWaitUs, backendUs, plus operation-specific
// identity fields snapshotted under the lock before it was released.
func logCanonicalAttachmentTiming(logger *slog.Logger, operation, result string, timing operationTiming, totalUs, lockWaitUs int64, identity ...any) {
	backend := "none"
	if timing.backendCalled {
		backend = "tracked"
	}
	args := []any{
		"operation", operation,
		"layer", "canonical",
		"result", result,
		"backend", backend,
		"backendCalled", timing.backendCalled,
		"totalUs", totalUs,
		"lockWaitUs", lockWaitUs,
		"backendUs", timing.backendUs,
	}
	args = append(args, identity...)
	logger.Info("attachment-timing", args...)
}

func attachmentStateName(state deviceAttachmentState) string {
	switch state {
	case attachmentDetached:
		return "detached"
	case attachmentAttached:
		return "attached"
	case attachmentOutcomeUnknown:
		return "outcome-unknown"
	default:
		return "invalid"
	}
}

// attachUSBDeviceResult resolves and locks the handle exactly like withActiveDeviceHandle, but
// returns the classified attachDeviceLockedResult instead of a bare bool so a rejected/invalid
// handle (deviceAttachInvalid) can never be confused with a zero-value success.
func attachUSBDeviceResult(handle uintptr) deviceAttachResult {
	opStart := time.Now()
	v, ok := deviceHandleRecords.Load(handle)
	if !ok {
		logCanonicalAttachmentTiming(invalidHandleLoggerFunc(), "attach", attachResultTimingLabel(deviceAttachInvalid), operationTiming{}, time.Since(opStart).Microseconds(), 0)
		return deviceAttachInvalid
	}
	dhw, ok := v.(*deviceHandleWrapper)
	if !ok {
		logCanonicalAttachmentTiming(invalidHandleLoggerFunc(), "attach", attachResultTimingLabel(deviceAttachInvalid), operationTiming{}, time.Since(opStart).Microseconds(), 0)
		return deviceAttachInvalid
	}
	hw := dhw.usbServer
	lockWaitStart := time.Now()
	notifyLifecycleLockAttempt(hw, "attach")
	hw.lifecycleMu.Lock()
	lockWaitUs := time.Since(lockWaitStart).Microseconds()
	backendLogs := newDeferredLogBatch()
	hw.backendLogLogger = backendLogs.logger

	var result deviceAttachResult
	var timing operationTiming
	stateBefore := dhw.attachment.state
	serverStateBefore := hw.state
	switch {
	case hw.deviceHandleRecords[deviceHandle(handle)] != dhw:
		result = deviceAttachInvalid
	// Check the device's own unsafe/unknown ownership evidence before the server-wide active
	// check: an unknown outcome must keep reporting UNSAFE_OUTCOME_UNKNOWN even after it has
	// pushed the server into close-failed, never downgrade to INVALID.
	case dhw.attachment.state == attachmentOutcomeUnknown:
		result = deviceAttachUnsafeOutcomeUnknown
	case hw.state != serverActive:
		result = deviceAttachInvalid
	default:
		result = hw.attachDeviceLockedResult(dhw, &timing)
	}
	stateAfter := dhw.attachment.state
	serverStateAfter := hw.state
	attachmentBackend := dhw.attachment.attachment.Backend
	importPort := dhw.attachment.attachment.Port
	listenPort := hw.s.GetListenPort()
	// exportMeta is set once at creation and never mutated, but snapshot it into locals anyway
	// (rather than reading dhw after unlock) so this function never depends on that remaining true.
	busID, deviceID := dhw.exportMeta.BusID, dhw.exportMeta.DevID
	logger := hw.logger
	hw.backendLogLogger = nil
	hw.lifecycleMu.Unlock()
	operationTotalUs := time.Since(opStart).Microseconds()
	if hw.onAttachmentTimingSnapshot != nil {
		hw.onAttachmentTimingSnapshot(operationTotalUs)
	}
	backendLogs.replay(logger)

	logCanonicalAttachmentTiming(logger, "attach", attachResultTimingLabel(result), timing, operationTotalUs, lockWaitUs,
		"busID", busID, "deviceID", deviceID, "listenPort", listenPort,
		"attachmentStateBefore", attachmentStateName(stateBefore), "attachmentStateAfter", attachmentStateName(stateAfter),
		"serverStateBefore", serverStateBefore.String(), "serverStateAfter", serverStateAfter.String(),
		"attachmentBackend", attachmentBackend, "importPort", importPort)
	return result
}

// detachUSBDeviceResult mirrors attachUSBDeviceResult for the classified detach path.
func detachUSBDeviceResult(handle uintptr) deviceDetachResult {
	opStart := time.Now()
	v, ok := deviceHandleRecords.Load(handle)
	if !ok {
		logCanonicalAttachmentTiming(invalidHandleLoggerFunc(), "detach", detachResultTimingLabel(deviceDetachInvalid), operationTiming{}, time.Since(opStart).Microseconds(), 0)
		return deviceDetachInvalid
	}
	dhw, ok := v.(*deviceHandleWrapper)
	if !ok {
		logCanonicalAttachmentTiming(invalidHandleLoggerFunc(), "detach", detachResultTimingLabel(deviceDetachInvalid), operationTiming{}, time.Since(opStart).Microseconds(), 0)
		return deviceDetachInvalid
	}
	hw := dhw.usbServer
	lockWaitStart := time.Now()
	notifyLifecycleLockAttempt(hw, "detach")
	hw.lifecycleMu.Lock()
	lockWaitUs := time.Since(lockWaitStart).Microseconds()
	backendLogs := newDeferredLogBatch()
	hw.backendLogLogger = backendLogs.logger

	// Snapshot the attachment token that is authoritative going into this call, before any
	// mutation: a successful detach clears dhw.attachment.attachment back to its zero value, and
	// the timing log for that exact success is the one case where the real backend/port most
	// matters for the real hardware analysis this instrumentation exists for.
	trackedBackend, trackedPort := dhw.attachment.attachment.Backend, dhw.attachment.attachment.Port

	var result deviceDetachResult
	var timing operationTiming
	stateBefore := dhw.attachment.state
	serverStateBefore := hw.state
	switch {
	case hw.deviceHandleRecords[deviceHandle(handle)] != dhw:
		result = deviceDetachInvalid
	// Same ordering rationale as attachUSBDeviceResult: unsafe/unknown ownership evidence takes
	// priority over the server-wide active check so it never gets downgraded to INVALID.
	case dhw.attachment.state == attachmentOutcomeUnknown:
		result = deviceDetachUnsafeOutcomeUnknown
	case hw.state != serverActive:
		result = deviceDetachInvalid
	default:
		result = hw.detachDeviceLockedResult(dhw, &timing)
	}
	stateAfter := dhw.attachment.state
	serverStateAfter := hw.state
	listenPort := hw.s.GetListenPort()
	busID, deviceID := dhw.exportMeta.BusID, dhw.exportMeta.DevID
	logger := hw.logger
	hw.backendLogLogger = nil
	hw.lifecycleMu.Unlock()
	operationTotalUs := time.Since(opStart).Microseconds()
	if hw.onAttachmentTimingSnapshot != nil {
		hw.onAttachmentTimingSnapshot(operationTotalUs)
	}
	backendLogs.replay(logger)

	logCanonicalAttachmentTiming(logger, "detach", detachResultTimingLabel(result), timing, operationTotalUs, lockWaitUs,
		"busID", busID, "deviceID", deviceID, "listenPort", listenPort,
		"attachmentStateBefore", attachmentStateName(stateBefore), "attachmentStateAfter", attachmentStateName(stateAfter),
		"serverStateBefore", serverStateBefore.String(), "serverStateAfter", serverStateAfter.String(),
		"attachmentBackend", trackedBackend, "importPort", trackedPort)
	return result
}

type deviceAttachmentQueryState uint8

const (
	deviceAttachmentQueryDetached deviceAttachmentQueryState = iota
	deviceAttachmentQueryAttached
	deviceAttachmentQueryOutcomeUnknown
)

// queryDeviceAttachmentState is a read-only diagnostic snapshot of a device's tracked
// localhost attachment ownership -- never the attach/detach backend. It follows the same
// active-or-close-failed diagnostic-handle model as lookupDeviceIdentity, but unlike
// lookupDeviceIdentity it translates the mutable attachment.state field to the public
// enum while still holding lifecycleMu, since that field (unlike the immutable exportMeta
// identity fields lookupDeviceIdentity's callers read) changes under Attach/Detach and must
// never be read after the lock has been released.
func queryDeviceAttachmentState(handle uintptr) (deviceAttachmentQueryState, bool) {
	v, ok := deviceHandleRecords.Load(handle)
	if !ok {
		return 0, false
	}
	dhw, ok := v.(*deviceHandleWrapper)
	if !ok {
		return 0, false
	}
	hw := dhw.usbServer
	hw.lifecycleMu.Lock()
	defer hw.lifecycleMu.Unlock()
	if hw.deviceHandleRecords[deviceHandle(handle)] != dhw {
		return 0, false
	}
	if hw.state != serverActive && hw.state != serverCloseFailed {
		return 0, false
	}
	switch dhw.attachment.state {
	case attachmentDetached:
		return deviceAttachmentQueryDetached, true
	case attachmentAttached:
		return deviceAttachmentQueryAttached, true
	case attachmentOutcomeUnknown:
		return deviceAttachmentQueryOutcomeUnknown, true
	default:
		// An unrecognized private state must never be fabricated into a public value.
		return 0, false
	}
}

func (hw *usbServerHandleWrapper) hasUnknownAttachmentLocked() bool {
	for _, dhw := range hw.deviceHandleRecords {
		if dhw.attachment.state == attachmentOutcomeUnknown {
			return true
		}
	}
	return false
}

func (hw *usbServerHandleWrapper) preflightBusDevicesLocked(busID uint32) (*deviceHandleWrapper, bool) {
	for _, h := range slices.Clone(hw.deviceHandles[busID]) {
		dhw := hw.deviceHandleRecords[h]
		if dhw == nil || dhw.attachment.state == attachmentOutcomeUnknown {
			return dhw, false
		}
	}
	return nil, true
}

func (hw *usbServerHandleWrapper) detachBusDevicesLocked(busID uint32) (*deviceHandleWrapper, bool, bool, teardownDiagnostic) {
	if dhw, ok := hw.preflightBusDevicesLocked(busID); !ok {
		return dhw, false, false, teardownDiagnostic{}
	}
	for _, h := range slices.Clone(hw.deviceHandles[busID]) {
		if dhw := hw.deviceHandleRecords[h]; dhw != nil {
			stateBefore := dhw.attachment.state
			token := dhw.attachment.attachment
			var timing operationTiming
			if hw.detachDeviceLockedResult(dhw, &timing) != deviceDetachSuccess {
				return dhw, false, timing.backendCalled, teardownDiagnostic{
					deviceID:              fmt.Sprintf("%d", dhw.exportMeta.DevID),
					attachmentStateBefore: attachmentStateName(stateBefore),
					attachmentStateAfter:  attachmentStateName(dhw.attachment.state),
					attachmentBackend:     token.Backend, importPort: token.Port,
					detachBackendCalled: timing.backendCalled, detachBackendCalledPresent: true,
				}
			}
		}
	}
	return nil, true, false, teardownDiagnostic{}
}

type rollbackDiagnostic struct {
	logger   *slog.Logger
	busID    uint32
	deviceID uint32
	reason   string
	err      string
}

func (hw *usbServerHandleWrapper) rollbackCreatedDeviceLocked(busID, deviceID uint32, rollback func(viiperusb.Device) error, dev viiperusb.Device, reason string) bool {
	ok, _ := hw.rollbackCreatedDeviceLockedWithDiagnostic(busID, deviceID, rollback, dev, reason)
	return ok
}

func (hw *usbServerHandleWrapper) rollbackCreatedDeviceLockedWithDiagnostic(busID, deviceID uint32, rollback func(viiperusb.Device) error, dev viiperusb.Device, reason string) (bool, *rollbackDiagnostic) {
	if err := rollback(dev); err == nil {
		return true, nil
	} else {
		hw.state = serverCloseFailed
		return false, &rollbackDiagnostic{logger: hw.logger, busID: busID, deviceID: deviceID, reason: reason, err: err.Error()}
	}
}

func emitRollbackDiagnostic(d *rollbackDiagnostic) {
	if d != nil {
		d.logger.Error("failed to roll back logical device", "operation", "typed-device-create", "serverState", serverCloseFailed.String(), "busID", d.busID, "deviceID", d.deviceID, "reason", d.reason, "error", d.err)
	}
}

func (s serverLifecycleState) String() string {
	switch s {
	case serverActive:
		return "active"
	case serverClosing:
		return "closing"
	case serverCloseFailed:
		return "close-failed"
	case serverClosed:
		return "closed"
	default:
		return "unknown"
	}
}

type mutationRejectedWarning struct {
	logger    *slog.Logger
	operation string
	state     string
	emit      bool
}

func (hw *usbServerHandleWrapper) takeMutationRejectedWarningLocked(operation string) mutationRejectedWarning {
	key := operation + ":" + hw.state.String()
	if hw.rejectionWarnings[key] {
		return mutationRejectedWarning{}
	}
	hw.rejectionWarnings[key] = true
	return mutationRejectedWarning{logger: hw.logger, operation: operation, state: hw.state.String(), emit: true}
}

func emitMutationRejectedWarning(warning mutationRejectedWarning) {
	if !warning.emit {
		return
	}
	warning.logger.Warn("server mutation rejected", "operation", warning.operation, "serverState", warning.state)
}

func (hw *usbServerHandleWrapper) finalizeDeviceLocked(h deviceHandle) {
	dhw, ok := hw.deviceHandleRecords[h]
	if !ok {
		return
	}
	delete(hw.deviceHandleRecords, h)
	deviceHandleRecords.Delete(uintptr(h))
	busID := dhw.exportMeta.BusID
	hw.deviceHandles[busID] = slices.DeleteFunc(hw.deviceHandles[busID], func(candidate deviceHandle) bool {
		return candidate == h
	})
	hw.ops.deleteHandle(cgo.Handle(h))
}

func (hw *usbServerHandleWrapper) finalizeBusLocked(busID uint32) {
	for _, h := range slices.Clone(hw.deviceHandles[busID]) {
		hw.finalizeDeviceLocked(h)
	}
	delete(hw.deviceHandles, busID)
}

// ---

type funcLogHandler struct{ fn func(slog.Level, string) }

func (h *funcLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *funcLogHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *funcLogHandler) WithGroup(string) slog.Handler            { return h }
func (h *funcLogHandler) Handle(_ context.Context, r slog.Record) error {
	msg := r.Message
	r.Attrs(func(a slog.Attr) bool {
		msg += fmt.Sprintf(" %s=%v", a.Key, a.Value)
		return true
	})
	h.fn(r.Level, strings.TrimSpace(msg))
	return nil
}
