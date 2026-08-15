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

func (hw *usbServerHandleWrapper) attachDeviceLocked(dhw *deviceHandleWrapper) bool {
	return hw.attachDeviceLockedResult(dhw) == deviceAttachSuccess
}

// attachDeviceLockedResult is the single classified attach implementation. Both the legacy bool
// AttachUSBDevice and the classified AttachUSBDeviceEx export call this same operation; the bool
// export never performs an independent second mutation attempt.
func (hw *usbServerHandleWrapper) attachDeviceLockedResult(dhw *deviceHandleWrapper) deviceAttachResult {
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
	attachment, err := hw.ops.attachLocalhostTracked(context.Background(), dhw.exportMeta, hw.s.GetListenPort(), true, hw.logger)
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
	return hw.detachDeviceLockedResult(dhw) == deviceDetachSuccess
}

// detachDeviceLockedResult is the single classified detach implementation. Both the legacy bool
// DetachUSBDevice and the classified DetachUSBDeviceEx export call this same operation; the bool
// export never performs an independent second mutation attempt.
func (hw *usbServerHandleWrapper) detachDeviceLockedResult(dhw *deviceHandleWrapper) deviceDetachResult {
	switch dhw.attachment.state {
	case attachmentDetached:
		return deviceDetachSuccess
	case attachmentOutcomeUnknown:
		// Ownership evidence is already unsafe/unknown; do not invoke the backend again or
		// downgrade this to an invalid-handle result.
		return deviceDetachUnsafeOutcomeUnknown
	}
	err := hw.ops.detachLocalhost(context.Background(), dhw.attachment.attachment, hw.logger)
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

// attachUSBDeviceResult resolves and locks the handle exactly like withActiveDeviceHandle, but
// returns the classified attachDeviceLockedResult instead of a bare bool so a rejected/invalid
// handle (deviceAttachInvalid) can never be confused with a zero-value success.
func attachUSBDeviceResult(handle uintptr) deviceAttachResult {
	v, ok := deviceHandleRecords.Load(handle)
	if !ok {
		return deviceAttachInvalid
	}
	dhw, ok := v.(*deviceHandleWrapper)
	if !ok {
		return deviceAttachInvalid
	}
	hw := dhw.usbServer
	hw.lifecycleMu.Lock()
	defer hw.lifecycleMu.Unlock()
	if hw.deviceHandleRecords[deviceHandle(handle)] != dhw {
		return deviceAttachInvalid
	}
	// Check the device's own unsafe/unknown ownership evidence before the server-wide active
	// check: an unknown outcome must keep reporting UNSAFE_OUTCOME_UNKNOWN even after it has
	// pushed the server into close-failed, never downgrade to INVALID.
	if dhw.attachment.state == attachmentOutcomeUnknown {
		return deviceAttachUnsafeOutcomeUnknown
	}
	if hw.state != serverActive {
		hw.warnMutationRejectedLocked("typed-device-mutation")
		return deviceAttachInvalid
	}
	return hw.attachDeviceLockedResult(dhw)
}

// detachUSBDeviceResult mirrors attachUSBDeviceResult for the classified detach path.
func detachUSBDeviceResult(handle uintptr) deviceDetachResult {
	v, ok := deviceHandleRecords.Load(handle)
	if !ok {
		return deviceDetachInvalid
	}
	dhw, ok := v.(*deviceHandleWrapper)
	if !ok {
		return deviceDetachInvalid
	}
	hw := dhw.usbServer
	hw.lifecycleMu.Lock()
	defer hw.lifecycleMu.Unlock()
	if hw.deviceHandleRecords[deviceHandle(handle)] != dhw {
		return deviceDetachInvalid
	}
	// Same ordering rationale as attachUSBDeviceResult: unsafe/unknown ownership evidence takes
	// priority over the server-wide active check so it never gets downgraded to INVALID.
	if dhw.attachment.state == attachmentOutcomeUnknown {
		return deviceDetachUnsafeOutcomeUnknown
	}
	if hw.state != serverActive {
		hw.warnMutationRejectedLocked("typed-device-mutation")
		return deviceDetachInvalid
	}
	return hw.detachDeviceLockedResult(dhw)
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
