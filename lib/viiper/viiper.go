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
	s                      *usb.Server
	lifecycleMu            sync.Mutex
	mtx                    sync.Mutex // Legacy wrapper synchronization; lifecycleMu gates mutations.
	state                  serverLifecycleState
	deviceHandles          map[uint32][]deviceHandle
	deviceHandleRecords    map[deviceHandle]*deviceHandleWrapper
	ops                    serverOperations
	logger                 *slog.Logger
	rejectionWarnings      map[string]bool
	onCallbackCleared      func(*deviceHandleWrapper)
	closePhase             canonicalClosePhase
	logicalCloseInProgress bool
}

type canonicalClosePhase uint8

const (
	logicalTeardownPending canonicalClosePhase = iota
	transportClosePending
	closeComplete
)

type transportTeardownResult struct {
	ok     bool
	drains []*usb.TransportDrain
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
	defer hw.lifecycleMu.Unlock()
	if hw.state != serverActive || hw.deviceHandleRecords[deviceHandle(raw)] != dhw {
		hw.warnMutationRejectedLocked("typed-device-mutation")
		return false
	}
	return action(dhw)
}

func (hw *usbServerHandleWrapper) createDeviceLocked(busID uint32, dev viiperusb.Device, autoAttach bool) (deviceHandle, bool) {
	if hw.state != serverActive {
		hw.warnMutationRejectedLocked("typed-device-create")
		return 0, false
	}
	bus := hw.s.GetBus(busID)
	if bus == nil {
		return 0, false
	}

	devCtx, err := bus.Add(dev)
	if err != nil {
		return 0, false
	}
	exportMeta := device.GetDeviceMeta(devCtx)
	if exportMeta == nil {
		hw.rollbackCreatedDeviceLocked(busID, 0, func(d viiperusb.Device) error { return hw.ops.rollbackDevice(bus, d) }, dev, "device metadata was unavailable")
		return 0, false
	}
	dhw := &deviceHandleWrapper{device: dev, exportMeta: exportMeta, usbServer: hw, attachment: deviceAttachmentRecord{state: attachmentDetached}}
	h := hw.registerDeviceLocked(dhw)
	if autoAttach {
		if !hw.attachDeviceLocked(dhw) {
			if dhw.attachment.state == attachmentOutcomeUnknown {
				hw.state = serverCloseFailed
				return h, false
			}
			if !hw.rollbackCreatedDeviceLocked(exportMeta.BusID, exportMeta.DevID, func(d viiperusb.Device) error { return hw.ops.rollbackDevice(bus, d) }, dev, "auto-attach failure") {
				return h, false
			}
			hw.finalizeDeviceLocked(h)
			return 0, false
		}
	}
	return h, true
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
	attachment, err := hw.ops.attachLocalhostTracked(context.Background(), dhw.exportMeta, hw.s.GetListenPort(), true, hw.logger)
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
	err := hw.ops.detachLocalhost(context.Background(), dhw.attachment.attachment, hw.logger)
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

func (hw *usbServerHandleWrapper) removeDeviceLocked(dhw *deviceHandleWrapper, h deviceHandle) bool {
	result := hw.removeDeviceLockedWithDrain(dhw, h)
	return result.ok
}

func (hw *usbServerHandleWrapper) removeDeviceLockedWithDrain(dhw *deviceHandleWrapper, h deviceHandle) transportTeardownResult {
	hw.clearDeviceCallbackLocked(dhw)
	if !hw.detachDeviceLocked(dhw) {
		return transportTeardownResult{}
	}
	drain := hw.s.BeginDeviceDrain(dhw.device.(viiperusb.Device))
	if err := hw.ops.removeDevice(hw.s, dhw.exportMeta.BusID, fmt.Sprintf("%d", dhw.exportMeta.DevID)); err != nil {
		return transportTeardownResult{drains: []*usb.TransportDrain{drain}}
	}
	hw.finalizeDeviceLocked(h)
	hw.s.ForgetDeviceTransport(dhw.device.(viiperusb.Device))
	return transportTeardownResult{ok: true, drains: []*usb.TransportDrain{drain}}
}

func removeTypedDevice(handle uintptr, valid func(any) bool) bool {
	return removeTypedDeviceResult(handle, valid) == typedDeviceRemoveSuccess
}

func removeTypedDeviceResult(handle uintptr, valid func(any) bool) typedDeviceRemoveResult {
	v, ok := deviceHandleRecords.Load(handle)
	if !ok {
		return typedDeviceRemoveInvalid
	}
	dhw, ok := v.(*deviceHandleWrapper)
	if !ok {
		return typedDeviceRemoveInvalid
	}
	hw := dhw.usbServer
	hw.lifecycleMu.Lock()
	if hw.deviceHandleRecords[deviceHandle(handle)] != dhw || !valid(dhw.device) {
		hw.lifecycleMu.Unlock()
		return typedDeviceRemoveInvalid
	}
	if dhw.attachment.state == attachmentOutcomeUnknown {
		hw.lifecycleMu.Unlock()
		return typedDeviceRemoveUnsafeOutcomeUnknown
	}
	if hw.state != serverActive {
		hw.lifecycleMu.Unlock()
		return typedDeviceRemoveInvalid
	}
	result := hw.removeDeviceLockedWithDrain(dhw, deviceHandle(handle))
	unsafeOutcome := dhw.attachment.state == attachmentOutcomeUnknown || hw.state == serverCloseFailed
	hw.lifecycleMu.Unlock()
	waitTransportDrains(result.drains)
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

// attachUSBDeviceResult resolves and locks the handle exactly like withActiveDeviceHandle, but
// returns the classified attachDeviceLockedResult instead of a bare bool so a rejected/invalid
// handle (deviceAttachInvalid) can never be confused with a zero-value success.
func attachUSBDeviceResult(handle uintptr) deviceAttachResult {
	opStart := time.Now()
	v, ok := deviceHandleRecords.Load(handle)
	if !ok {
		logCanonicalAttachmentTiming(slog.Default(), "attach", attachResultTimingLabel(deviceAttachInvalid), operationTiming{}, time.Since(opStart).Microseconds(), 0)
		return deviceAttachInvalid
	}
	dhw, ok := v.(*deviceHandleWrapper)
	if !ok {
		logCanonicalAttachmentTiming(slog.Default(), "attach", attachResultTimingLabel(deviceAttachInvalid), operationTiming{}, time.Since(opStart).Microseconds(), 0)
		return deviceAttachInvalid
	}
	hw := dhw.usbServer
	lockWaitStart := time.Now()
	hw.lifecycleMu.Lock()
	lockWaitUs := time.Since(lockWaitStart).Microseconds()

	var result deviceAttachResult
	var timing operationTiming
	switch {
	case hw.deviceHandleRecords[deviceHandle(handle)] != dhw:
		result = deviceAttachInvalid
	// Check the device's own unsafe/unknown ownership evidence before the server-wide active
	// check: an unknown outcome must keep reporting UNSAFE_OUTCOME_UNKNOWN even after it has
	// pushed the server into close-failed, never downgrade to INVALID.
	case dhw.attachment.state == attachmentOutcomeUnknown:
		result = deviceAttachUnsafeOutcomeUnknown
	case hw.state != serverActive:
		hw.warnMutationRejectedLocked("typed-device-mutation")
		result = deviceAttachInvalid
	default:
		result = hw.attachDeviceLockedResult(dhw, &timing)
	}
	// exportMeta is set once at creation and never mutated, but snapshot it into locals anyway
	// (rather than reading dhw after unlock) so this function never depends on that remaining true.
	busID, deviceID := dhw.exportMeta.BusID, dhw.exportMeta.DevID
	logger := hw.logger
	hw.lifecycleMu.Unlock()

	logCanonicalAttachmentTiming(logger, "attach", attachResultTimingLabel(result), timing, time.Since(opStart).Microseconds(), lockWaitUs,
		"busID", busID, "deviceID", deviceID)
	return result
}

// detachUSBDeviceResult mirrors attachUSBDeviceResult for the classified detach path.
func detachUSBDeviceResult(handle uintptr) deviceDetachResult {
	opStart := time.Now()
	v, ok := deviceHandleRecords.Load(handle)
	if !ok {
		logCanonicalAttachmentTiming(slog.Default(), "detach", detachResultTimingLabel(deviceDetachInvalid), operationTiming{}, time.Since(opStart).Microseconds(), 0)
		return deviceDetachInvalid
	}
	dhw, ok := v.(*deviceHandleWrapper)
	if !ok {
		logCanonicalAttachmentTiming(slog.Default(), "detach", detachResultTimingLabel(deviceDetachInvalid), operationTiming{}, time.Since(opStart).Microseconds(), 0)
		return deviceDetachInvalid
	}
	hw := dhw.usbServer
	lockWaitStart := time.Now()
	hw.lifecycleMu.Lock()
	lockWaitUs := time.Since(lockWaitStart).Microseconds()

	// Snapshot the attachment token that is authoritative going into this call, before any
	// mutation: a successful detach clears dhw.attachment.attachment back to its zero value, and
	// the timing log for that exact success is the one case where the real backend/port most
	// matters for the real hardware analysis this instrumentation exists for.
	trackedBackend, trackedPort := dhw.attachment.attachment.Backend, dhw.attachment.attachment.Port

	var result deviceDetachResult
	var timing operationTiming
	switch {
	case hw.deviceHandleRecords[deviceHandle(handle)] != dhw:
		result = deviceDetachInvalid
	// Same ordering rationale as attachUSBDeviceResult: unsafe/unknown ownership evidence takes
	// priority over the server-wide active check so it never gets downgraded to INVALID.
	case dhw.attachment.state == attachmentOutcomeUnknown:
		result = deviceDetachUnsafeOutcomeUnknown
	case hw.state != serverActive:
		hw.warnMutationRejectedLocked("typed-device-mutation")
		result = deviceDetachInvalid
	default:
		result = hw.detachDeviceLockedResult(dhw, &timing)
	}
	logger := hw.logger
	hw.lifecycleMu.Unlock()

	logCanonicalAttachmentTiming(logger, "detach", detachResultTimingLabel(result), timing, time.Since(opStart).Microseconds(), lockWaitUs,
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

func (hw *usbServerHandleWrapper) preflightBusDevicesLocked(busID uint32) bool {
	for _, h := range slices.Clone(hw.deviceHandles[busID]) {
		dhw := hw.deviceHandleRecords[h]
		if dhw == nil || dhw.attachment.state == attachmentOutcomeUnknown {
			return false
		}
	}
	return true
}

func (hw *usbServerHandleWrapper) detachBusDevicesLocked(busID uint32) bool {
	if !hw.preflightBusDevicesLocked(busID) {
		return false
	}
	for _, h := range slices.Clone(hw.deviceHandles[busID]) {
		if dhw := hw.deviceHandleRecords[h]; dhw != nil && !hw.detachDeviceLocked(dhw) {
			return false
		}
	}
	return true
}

func (hw *usbServerHandleWrapper) rollbackCreatedDeviceLocked(busID, deviceID uint32, rollback func(viiperusb.Device) error, dev viiperusb.Device, reason string) bool {
	if err := rollback(dev); err == nil {
		return true
	} else {
		hw.state = serverCloseFailed
		hw.logger.Error("failed to roll back logical device", "operation", "typed-device-create", "serverState", hw.state.String(), "busID", busID, "deviceID", deviceID, "reason", reason, "error", err)
		return false
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

func (hw *usbServerHandleWrapper) warnMutationRejectedLocked(operation string) {
	key := operation + ":" + hw.state.String()
	if hw.rejectionWarnings[key] {
		return
	}
	hw.rejectionWarnings[key] = true
	hw.logger.Warn("server mutation rejected", "operation", operation, "serverState", hw.state.String())
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
