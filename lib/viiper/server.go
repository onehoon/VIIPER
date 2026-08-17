package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	char* addr; // default "0.0.0.0:3241"
	uint64_t connection_timeout_ms; // default 30000 (30s)
	uint64_t device_handler_connect_timeout_ms; // default 5000 (5s)
	uint32_t write_batch_flush_interval_ms; // default 1 (1ms)
} USBServerConfig;

typedef uintptr_t USBServerHandle;

typedef enum {
    VIIPER_LOG_DEBUG = -4,
    VIIPER_LOG_INFO  = 0,
    VIIPER_LOG_WARN  = 4,
    VIIPER_LOG_ERROR = 8,
} VIIPERLogLevel;

typedef void (*VIIPERLogCallback)(VIIPERLogLevel level, const char* message);

static void viiper_call_log(VIIPERLogCallback fn, VIIPERLogLevel level, const char* msg) {
	fn(level, msg);
}
*/
import "C"

import (
	"log/slog"
	"runtime/cgo"
	"slices"
	"time"
	"unsafe"

	"github.com/Alia5/VIIPER/internal/server/usb"
)

// NewUSBServer creates a new USB server with the given configuration and returns a handle to it.
// The server will run in the background and can be stopped by calling CloseUSBServer with the returned handle.
// @param config Server configuration
// @param outHandle Output parameter for the created server handle
// @param logCallback Optional callback function for log messages from the USB server
//
//export NewUSBServer
func NewUSBServer(config *C.USBServerConfig, outHandle *C.USBServerHandle, logCallback C.VIIPERLogCallback) bool {
	if !hasRequiredUSBServerPointers(unsafe.Pointer(config), unsafe.Pointer(outHandle)) {
		return false
	}
	addr := C.GoString(config.addr)
	connectionTimeout := time.Duration(config.connection_timeout_ms) * time.Millisecond
	busCleanupTimeout := time.Duration(config.device_handler_connect_timeout_ms) * time.Millisecond
	writeBatchFlushInterval := time.Duration(config.write_batch_flush_interval_ms) * time.Millisecond

	if addr == "" {
		addr = ":3241"
	}
	if connectionTimeout == 0 {
		connectionTimeout = 30 * time.Second
	}
	if busCleanupTimeout == 0 {
		busCleanupTimeout = 5 * time.Second
	}

	// libVIIPER owns its diagnostic log: openRealEmbeddedLogFileHandler is libVIIPER.log beside
	// the loaded shared library, whose sink is attempted independently of logCallback (module-path
	// resolution or the file open can still fail, in which case there is simply no file handler --
	// see embeddedlog.go). logCallback, when supplied, is an additional observer -- never a
	// replacement for the file sink, and never a reason to write a record into the file twice.
	// This never calls slog.SetDefault: the embedding process's own global default logger is left
	// alone.
	var callbackHandler slog.Handler
	if logCallback != nil {
		callbackHandler = &funcLogHandler{
			func(level slog.Level, msg string) {
				if logCallback == nil {
					return
				}
				cMsg := C.CString(msg)
				defer C.free(unsafe.Pointer(cMsg))
				C.viiper_call_log(logCallback, C.VIIPERLogLevel(level), cMsg)
			},
		}
	}
	logger := buildEmbeddedLogger(openRealEmbeddedLogFileHandler(), callbackHandler)

	s := usb.New(usb.ServerConfig{
		Addr:                      addr,
		ConnectionTimeout:         connectionTimeout,
		BusCleanupTimeout:         busCleanupTimeout,
		WriteBatchFlushInterval:   writeBatchFlushInterval,
		DisableAutoBusCleanup:     true,
		ManagedTransportLifecycle: true,
	}, logger, nil)

	readyChan := s.Ready()
	errChan := make(chan error, 1)

	go func() {
		errChan <- s.ListenAndServe()
	}()

	select {
	case <-readyChan:
		hw := &usbServerHandleWrapper{
			s:                   s,
			state:               serverActive,
			deviceHandles:       make(map[uint32][]deviceHandle),
			deviceHandleRecords: make(map[deviceHandle]*deviceHandleWrapper),
			ops:                 defaultServerOperations(),
			logger:              logger,
			rejectionWarnings:   make(map[string]bool),
		}
		h := cgo.NewHandle(hw)
		*outHandle = C.USBServerHandle(h)
		serverHandleRecords.Store(uintptr(h), hw)
		logger.Info("USB server started", "operation", "NewUSBServer", "serverState", serverActive.String())
		return true
	case err := <-errChan:
		logger.Error("NewUSBServer: ListenAndServe failed", "error", err)
		return false
	}
}

func hasRequiredUSBServerPointers(config, outHandle unsafe.Pointer) bool {
	return config != nil && outHandle != nil
}

// CloseUSBServer closes the USB server associated with the given handle.
// Automatically removes busses and devices associated with the server.
// @param handle Handle to the USB server to close.
//
//export CloseUSBServer
func CloseUSBServer(handle C.USBServerHandle) bool {
	hw, ok := lookupServerHandle(uintptr(handle))
	if !ok {
		return false
	}
	notifyLifecycleLockAttempt(hw, "close")
	hw.lifecycleMu.Lock()
	if hw.closePhase == transportClosePending {
		if hw.state != serverCloseFailed {
			hw.lifecycleMu.Unlock()
			return false
		}
		hw.state = serverClosing
		hw.lifecycleMu.Unlock()
		return hw.finishTransportClose(uintptr(handle))
	}
	result := hw.beginLogicalCloseLocked()
	hw.lifecycleMu.Unlock()
	waitTransportDrains(result.drains)
	if !result.ok {
		return false
	}
	return hw.finishTransportClose(uintptr(handle))
}

func (hw *usbServerHandleWrapper) beginLogicalCloseLocked() transportTeardownResult {
	if hw.state != serverActive && hw.state != serverCloseFailed {
		hw.warnMutationRejectedLocked("CloseUSBServer")
		return transportTeardownResult{}
	}
	if hw.state == serverCloseFailed {
		hw.logger.Warn("retrying a previously failed server close", "operation", "CloseUSBServer", "serverState", hw.state.String())
	}
	if hw.hasUnknownAttachmentLocked() {
		hw.state = serverCloseFailed
		return transportTeardownResult{}
	}
	hw.state = serverClosing
	hw.logicalCloseInProgress = true

	busIDs := hw.s.ListBuses()
	slices.Sort(busIDs)
	hw.clearAllCallbacksLocked()
	var allDrains []*usb.TransportDrain
	for _, busID := range busIDs {
		result := hw.removeBusLockedWithDrains(busID)
		allDrains = append(allDrains, result.drains...)
		if !result.ok {
			hw.state = serverCloseFailed
			hw.logicalCloseInProgress = false
			result.drains = allDrains
			return result
		}
	}
	hw.logicalCloseInProgress = false
	hw.closePhase = transportClosePending
	return transportTeardownResult{ok: true, drains: allDrains}
}

func (hw *usbServerHandleWrapper) finishTransportClose(handle uintptr) bool {
	err := hw.ops.close(hw.s)
	hw.lifecycleMu.Lock()
	if err != nil {
		hw.state = serverCloseFailed
		logger, state, remainingBusCount := hw.logger, hw.state, len(hw.s.ListBuses())
		hw.lifecycleMu.Unlock()
		// Logging happens after the lock is released, same as the classified Attach/Detach path:
		// lifecycle state transition, consumer callback latency, and filesystem flush latency
		// must never be serialized behind lifecycleMu.
		logger.Error("failed to close USB server", "operation", "CloseUSBServer", "serverState", state.String(), "remainingBusCount", remainingBusCount, "error", err)
		return false
	}
	hw.state = serverClosed
	hw.closePhase = closeComplete
	serverHandleRecords.Delete(handle)
	cgo.Handle(handle).Delete()
	logger, state := hw.logger, hw.state
	hw.lifecycleMu.Unlock()

	logger.Info("USB server closed", "operation", "CloseUSBServer", "serverState", state.String())
	// Best-effort only, and only after the lock is released: the result is never surfaced and
	// never changes CloseUSBServer's own result, which has already succeeded by this point. A
	// stuck/slow filesystem must never make server close itself appear to hang or fail, and must
	// never hold lifecycleMu for up to ~1s (asyncLogFlushTimeout in both directions) while doing so.
	flushEmbeddedLogBestEffort()
	return true
}

// closeLocked is retained as an internal synchronous test seam. Public
// CloseUSBServer uses the two-phase path above so transport waits occur after
// lifecycleMu is released.
func (hw *usbServerHandleWrapper) closeLocked() bool {
	if hw.closePhase == transportClosePending {
		if err := hw.ops.close(hw.s); err != nil {
			hw.state = serverCloseFailed
			return false
		}
		hw.state = serverClosed
		hw.closePhase = closeComplete
		return true
	}
	result := hw.beginLogicalCloseLocked()
	if !result.ok {
		return false
	}
	if err := hw.ops.close(hw.s); err != nil {
		hw.state = serverCloseFailed
		return false
	}
	hw.state = serverClosed
	hw.closePhase = closeComplete
	hw.logger.Info("USB server closed", "operation", "CloseUSBServer", "serverState", hw.state.String())
	return true
}
