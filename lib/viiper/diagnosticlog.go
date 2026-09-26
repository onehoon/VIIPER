package main

import "C"

// SetDiagnosticLogDirectory selects the directory for libVIIPER.log. The UTF-8 input is copied
// before returning. It must be called before libVIIPER initializes its owned embedded file sink;
// a late or invalid call returns false and leaves the current logging configuration unchanged.
// If it is never called, Windows retains the existing loaded-module-directory fallback.
//
//export SetDiagnosticLogDirectory
func SetDiagnosticLogDirectory(directory *C.char) bool {
	if directory == nil {
		return false
	}
	return embeddedDiagnosticLogDirectory.set(C.GoString(directory))
}
