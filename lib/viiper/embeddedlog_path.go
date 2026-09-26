package main

import "path/filepath"

func resolveEmbeddedLogPath(directoryOverride string) (string, bool) {
	if directoryOverride != "" {
		return filepath.Join(directoryOverride, embeddedLogFileName), true
	}
	return resolveEmbeddedLogPathBesideModule()
}
