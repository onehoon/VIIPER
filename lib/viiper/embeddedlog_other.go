//go:build !windows

package main

// resolveEmbeddedLogPath has no non-Windows implementation in this PR: this fork's tracked
// localhost attachment story is Windows-only, and module-path-beside-the-loaded-shared-library
// discovery is a Windows-specific mechanism. Non-Windows builds get no file sink; a
// VIIPERLogCallback, if supplied, still works normally.
func resolveEmbeddedLogPath() (string, bool) {
	return "", false
}
