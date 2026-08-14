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
	// attachLocalhost remains a test-only compatibility seam for the PR1
	// lifecycle tests. Canonical production uses attachLocalhostTracked.
	attachLocalhost        func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) error
	attachLocalhostTracked func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error)
	detachLocalhost        func(context.Context, api.LocalhostAttachment, *slog.Logger) error
	removeBus              func(*usb.Server, uint32) error
	close                  func(*usb.Server) error
	deleteHandle           func(cgo.Handle)
}

func defaultServerOperations() serverOperations {
	return serverOperations{
		attachLocalhostTracked: api.AttachLocalhostClientTracked,
		detachLocalhost:        api.DetachLocalhostClient,
		removeBus:              func(s *usb.Server, busID uint32) error { return s.RemoveBus(busID) },
		close:                  func(s *usb.Server) error { return s.Close() },
		deleteHandle:           func(h cgo.Handle) { h.Delete() },
	}
}

type usbServerHandleWrapper struct {
	s                   *usb.Server
	lifecycleMu         sync.Mutex
	mtx                 sync.Mutex // Legacy wrapper synchronization; lifecycleMu gates mutations.
	state               serverLifecycleState
	deviceHandles       map[uint32][]deviceHandle
	deviceHandleRecords map[deviceHandle]*deviceHandleWrapper
	ops                 serverOperations
	logger              *slog.Logger
	rejectionWarnings   map[string]bool
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
		hw.rollbackCreatedDeviceLocked(busID, 0, bus, dev, "device metadata was unavailable")
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
			hw.rollbackCreatedDeviceLocked(exportMeta.BusID, exportMeta.DevID, bus, dev, "auto-attach failure")
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
	switch dhw.attachment.state {
	case attachmentAttached:
		return true
	case attachmentOutcomeUnknown:
		return false
	}
	if hw.ops.attachLocalhost != nil {
		return hw.ops.attachLocalhost(context.Background(), dhw.exportMeta, hw.s.GetListenPort(), true, hw.logger) == nil
	}
	attachment, err := hw.ops.attachLocalhostTracked(context.Background(), dhw.exportMeta, hw.s.GetListenPort(), true, hw.logger)
	if err == nil && attachment.Port > 0 {
		dhw.attachment = deviceAttachmentRecord{state: attachmentAttached, attachment: attachment}
		return true
	}
	if err == nil || errors.Is(err, api.ErrAttachmentOutcomeUnknown) {
		dhw.attachment.state = attachmentOutcomeUnknown
		hw.state = serverCloseFailed
	}
	return false
}

func (hw *usbServerHandleWrapper) detachDeviceLocked(dhw *deviceHandleWrapper) bool {
	switch dhw.attachment.state {
	case attachmentDetached:
		return true
	case attachmentOutcomeUnknown:
		return false
	}
	err := hw.ops.detachLocalhost(context.Background(), dhw.attachment.attachment, hw.logger)
	if err == nil {
		dhw.attachment = deviceAttachmentRecord{state: attachmentDetached}
		return true
	}
	if errors.Is(err, api.ErrDetachmentOutcomeUnknown) {
		dhw.attachment.state = attachmentOutcomeUnknown
		hw.state = serverCloseFailed
	}
	return false
}

func (hw *usbServerHandleWrapper) removeDeviceLocked(dhw *deviceHandleWrapper, h deviceHandle) bool {
	if !hw.detachDeviceLocked(dhw) {
		return false
	}
	if err := hw.s.RemoveDeviceByIDWithoutBusCleanup(dhw.exportMeta.BusID, fmt.Sprintf("%d", dhw.exportMeta.DevID)); err != nil {
		return false
	}
	hw.finalizeDeviceLocked(h)
	return true
}

func (hw *usbServerHandleWrapper) hasUnknownAttachmentLocked() bool {
	for _, dhw := range hw.deviceHandleRecords {
		if dhw.attachment.state == attachmentOutcomeUnknown {
			return true
		}
	}
	return false
}

func (hw *usbServerHandleWrapper) detachBusDevicesLocked(busID uint32) bool {
	for _, h := range slices.Clone(hw.deviceHandles[busID]) {
		dhw := hw.deviceHandleRecords[h]
		if dhw == nil || dhw.attachment.state == attachmentOutcomeUnknown {
			return false
		}
	}
	for _, h := range slices.Clone(hw.deviceHandles[busID]) {
		if dhw := hw.deviceHandleRecords[h]; dhw != nil && !hw.detachDeviceLocked(dhw) {
			return false
		}
	}
	return true
}

func (hw *usbServerHandleWrapper) rollbackCreatedDeviceLocked(busID, deviceID uint32, bus interface{ Remove(viiperusb.Device) error }, dev viiperusb.Device, reason string) {
	if err := bus.Remove(dev); err == nil {
		return
	} else {
		hw.state = serverCloseFailed
		hw.logger.Error("failed to roll back logical device", "operation", "typed-device-create", "serverState", hw.state.String(), "busID", busID, "deviceID", deviceID, "reason", reason, "error", err)
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
