//go:build windows

package main

import (
	"fmt"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

const getModuleHandleExFlagFromAddress = 0x00000004

// embeddedLogFileName is fixed and deliberately unconfigurable in this PR: libVIIPER owns exactly
// one diagnostic file, written beside the loaded shared library.
const embeddedLogFileName = "libVIIPER.log"

var (
	kernel32               = windows.NewLazySystemDLL("kernel32.dll")
	procGetModuleHandleExW = kernel32.NewProc("GetModuleHandleExW")
	procGetModuleFileNameW = kernel32.NewProc("GetModuleFileNameW")
)

// moduleAnchor is never read; only its address is used, as an address that is guaranteed to live
// inside this loaded module's image (a Go global variable is placed in the module's own data
// section when built with -buildmode=c-shared).
var moduleAnchor byte

// resolveEmbeddedLogPath resolves libVIIPER.log beside the directory containing the actually
// loaded libVIIPER.dll module -- not the process executable, not the current working directory,
// and not any application-specific path. Returns ok=false (never an error the caller must
// surface) if module-path discovery fails for any reason; the caller treats that identically to
// "no file sink available."
func resolveEmbeddedLogPath() (string, bool) {
	dir, err := loadedModuleDir()
	if err != nil || dir == "" {
		return "", false
	}
	return filepath.Join(dir, embeddedLogFileName), true
}

func loadedModuleDir() (string, error) {
	var handle windows.Handle
	// GET_MODULE_HANDLE_EX_FLAG_FROM_ADDRESS (as opposed to the deprecated
	// ...UNCHANGED_REFCOUNT flag, and never PIN) increments this module's reference count on
	// success; that reference must be balanced with FreeLibrary once the filename has been
	// copied, or every diagnostic-log write leaks one module reference.
	r0, _, callErr := procGetModuleHandleExW.Call(
		uintptr(getModuleHandleExFlagFromAddress),
		uintptr(unsafe.Pointer(&moduleAnchor)),
		uintptr(unsafe.Pointer(&handle)),
	)
	if r0 == 0 {
		return "", fmt.Errorf("GetModuleHandleExW failed: %w", callErr)
	}
	defer func() {
		// Diagnostic-only: a FreeLibrary failure here must never affect routing, and this
		// module's own code stays mapped regardless (this call only releases the extra
		// reference GetModuleHandleExW just added, not the loader's original one).
		_ = windows.FreeLibrary(handle)
	}()
	path, err := moduleFileName(handle)
	if err != nil {
		return "", err
	}
	return filepath.Dir(path), nil
}

func moduleFileName(handle windows.Handle) (string, error) {
	buf := make([]uint16, 260)
	for {
		r0, _, callErr := procGetModuleFileNameW.Call(
			uintptr(handle),
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(uint32(len(buf))),
		)
		n := uint32(r0)
		if n == 0 {
			return "", fmt.Errorf("GetModuleFileNameW failed: %w", callErr)
		}
		if int(n) < len(buf) {
			return windows.UTF16ToString(buf[:n]), nil
		}
		buf = make([]uint16, len(buf)*2)
	}
}
