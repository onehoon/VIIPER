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

	var logger *slog.Logger
	if logCallback != nil {
		logger = slog.New(&funcLogHandler{
			func(level slog.Level, msg string) {
				if logCallback == nil {
					return
				}
				cMsg := C.CString(msg)
				defer C.free(unsafe.Pointer(cMsg))
				C.viiper_call_log(logCallback, C.VIIPERLogLevel(level), cMsg)
			},
		})
	} else {
		logger = slog.New(slog.DiscardHandler)
	}
	slog.SetDefault(logger)

	s := usb.New(usb.ServerConfig{
		Addr:                    addr,
		ConnectionTimeout:       connectionTimeout,
		BusCleanupTimeout:       busCleanupTimeout,
		WriteBatchFlushInterval: writeBatchFlushInterval,
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
	hw.lifecycleMu.Lock()
	defer hw.lifecycleMu.Unlock()
	if !hw.closeLocked() {
		return false
	}
	serverHandleRecords.Delete(uintptr(handle))
	cgo.Handle(handle).Delete()
	return true
}

// closeLocked tears down a server while lifecycleMu is held.
func (hw *usbServerHandleWrapper) closeLocked() bool {
	if hw.state != serverActive && hw.state != serverCloseFailed {
		hw.warnMutationRejectedLocked("CloseUSBServer")
		return false
	}
	if hw.state == serverCloseFailed {
		hw.logger.Warn("retrying a previously failed server close", "operation", "CloseUSBServer", "serverState", hw.state.String())
	}
	if hw.hasUnknownAttachmentLocked() {
		hw.state = serverCloseFailed
		return false
	}
	hw.state = serverClosing

	busIDs := hw.s.ListBuses()
	slices.Sort(busIDs)
	for _, busID := range busIDs {
		if !hw.detachBusDevicesLocked(busID) {
			hw.state = serverCloseFailed
			return false
		}
		if err := hw.ops.removeBus(hw.s, busID); err != nil {
			if hw.s.GetBus(busID) == nil {
				hw.finalizeBusLocked(busID)
				continue
			}
			hw.state = serverCloseFailed
			hw.logger.Error("failed to remove bus during server close", "operation", "CloseUSBServer", "serverState", hw.state.String(), "busID", busID, "remainingBusCount", len(hw.s.ListBuses()), "error", err)
			return false
		}
		hw.finalizeBusLocked(busID)
	}

	if err := hw.ops.close(hw.s); err != nil {
		hw.state = serverCloseFailed
		hw.logger.Error("failed to close USB server", "operation", "CloseUSBServer", "serverState", hw.state.String(), "remainingBusCount", len(hw.s.ListBuses()), "error", err)
		return false
	}
	hw.state = serverClosed
	hw.logger.Info("USB server closed", "operation", "CloseUSBServer", "serverState", hw.state.String())
	return true
}
