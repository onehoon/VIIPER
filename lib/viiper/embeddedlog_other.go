//go:build !windows

package main

// resolveEmbeddedLogPathBesideModule has no non-Windows fallback in this fork: its tracked
// localhost attachment story is Windows-only, and module-path-beside-the-loaded-shared-library
// discovery is a Windows-specific mechanism. A caller-supplied diagnostic directory still works;
// without one, non-Windows builds get no file sink and VIIPERLogCallback remains unaffected.
func resolveEmbeddedLogPathBesideModule() (string, bool) {
	return "", false
}
