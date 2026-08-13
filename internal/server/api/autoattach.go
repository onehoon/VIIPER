package api

import (
	"context"
	"errors"
	"log/slog"

	"github.com/Alia5/VIIPER/usbip"
)

// LocalhostAttachmentBackend identifies the Windows mechanism that created an
// imported USB/IP device. A later detach must use the same proven mechanism.
type LocalhostAttachmentBackend uint8

const (
	localhostAttachmentBackendUnknown LocalhostAttachmentBackend = iota
	LocalhostAttachmentBackendNativeIOCTL
	LocalhostAttachmentBackendCommand
)

// LocalhostAttachment is the backend-level ownership token for one imported
// Windows USB/IP device. Port is the only valid detach target; BusID and DevID
// are attach inputs and must never be used as a detach substitute.
type LocalhostAttachment struct {
	Backend LocalhostAttachmentBackend
	Port    int32
}

// ErrAttachmentOutcomeUnknown means an attach operation may have reached the
// operating system, but no exact import port was established. Callers must not
// retry with another attach mechanism, because doing so could duplicate an
// attachment that can no longer be safely owned or detached.
var ErrAttachmentOutcomeUnknown = errors.New("usbip attachment outcome unknown")

func AttachLocalhostClient(ctx context.Context, deviceExportMeta *usbip.ExportMeta, usbipServerPort uint16, useNativeIOCTL bool, logger *slog.Logger) error {
	return attachLocalhostClientImpl(ctx, deviceExportMeta, usbipServerPort, useNativeIOCTL, logger)
}

// AttachLocalhostClientTracked returns the exact Windows USB/IP attachment
// token required for a later ownership-specific detach. It is an internal
// backend primitive; public libVIIPER attach/detach APIs are intentionally not
// exposed yet.
func AttachLocalhostClientTracked(ctx context.Context, deviceExportMeta *usbip.ExportMeta, usbipServerPort uint16, useNativeIOCTL bool, logger *slog.Logger) (LocalhostAttachment, error) {
	return attachLocalhostClientTrackedImpl(ctx, deviceExportMeta, usbipServerPort, useNativeIOCTL, logger)
}

// DetachLocalhostClient detaches exactly the port recorded by a successful
// tracked attach. Platform implementations reject non-positive ports before
// they can reach all-port driver semantics.
func DetachLocalhostClient(ctx context.Context, attachment LocalhostAttachment, logger *slog.Logger) error {
	return detachLocalhostClientImpl(ctx, attachment, logger)
}
