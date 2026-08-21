package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Alia5/VIIPER/usbip"
)

type localhostAttachAttempt func(context.Context, *usbip.ExportMeta, uint16, *slog.Logger) (LocalhostAttachment, error)

// attachLocalhostClientWithFallback makes one layer responsible for native to
// command fallback. Unknown outcomes deliberately bypass fallback.
//
// port is the real runtime USB/IP server listen port (hw.s.GetListenPort() in lib/viiper); every
// call site visible to this package's linter run happens to pass the test fixture value 3241,
// but the contract genuinely needs a variable port and must not be narrowed.
//
//nolint:unparam
func attachLocalhostClientWithFallback(ctx context.Context, meta *usbip.ExportMeta, port uint16, useNativeIOCTL bool, logger *slog.Logger, native, command localhostAttachAttempt) (LocalhostAttachment, error) {
	start := time.Now()

	if !useNativeIOCTL {
		attachment, err := command(ctx, meta, port, logger)
		logAttachmentFallbackTiming(logger, false, false, "command", err, time.Since(start))
		return attachment, err
	}

	attachment, err := native(ctx, meta, port, logger)
	if err == nil {
		logAttachmentFallbackTiming(logger, true, false, "native-ioctl", nil, time.Since(start))
		return attachment, nil
	}
	if errors.Is(err, ErrAttachmentOutcomeUnknown) {
		logAttachmentFallbackTiming(logger, true, false, "native-ioctl", err, time.Since(start))
		return LocalhostAttachment{}, err
	}

	logger.Error("native USB/IP attach failed before ownership could be created; using command fallback", "error", err)
	attachment, cmdErr := command(ctx, meta, port, logger)
	logAttachmentFallbackTiming(logger, true, true, "command", cmdErr, time.Since(start))
	return attachment, cmdErr
}

// logAttachmentFallbackTiming emits one behavior-neutral "attachment-timing" summary
// (layer=fallback) describing the fallback decision itself: whether native IOCTL was attempted,
// whether the command fallback was used, and which backend the final result actually came from.
// This is purely diagnostic and runs after the real fallback decision above has already been
// made; it never influences it.
func logAttachmentFallbackTiming(logger *slog.Logger, nativeAttempted, fallbackUsed bool, finalBackend string, err error, total time.Duration) {
	logger.Info("attachment-timing",
		"operation", "attach",
		"layer", "fallback",
		"result", attachmentTimingResultLabel(err),
		"backend", finalBackend,
		"nativeAttempted", nativeAttempted,
		"fallbackUsed", fallbackUsed,
		"totalUs", total.Microseconds(),
	)
}

// attachmentTimingResultLabel maps an attach/detach error to the stable timing-log result
// vocabulary (success/unsafe-outcome-unknown/retryable-failure). It never affects the actual
// returned error.
func attachmentTimingResultLabel(err error) string {
	switch {
	case err == nil:
		return "success"
	case errors.Is(err, ErrAttachmentOutcomeUnknown), errors.Is(err, ErrDetachmentOutcomeUnknown):
		return "unsafe-outcome-unknown"
	default:
		return "retryable-failure"
	}
}

func parseUSBIPTerseAttachPort(output string) (int32, error) {
	value := strings.TrimSpace(output)
	if value == "" || strings.ContainsAny(value, " \t\r\n") {
		return 0, fmt.Errorf("unparseable usbip terse attach output")
	}
	port, err := strconv.ParseInt(value, 10, 32)
	if err != nil || port <= 0 || port > math.MaxInt32 {
		return 0, fmt.Errorf("invalid usbip import port %q", value)
	}
	return int32(port), nil
}

func classifyUSBIPAttachCommandResult(output []byte, err error) (int32, error) {
	if err != nil {
		var startErr *exec.Error
		if errors.As(err, &startErr) {
			return 0, err
		}
		return 0, fmt.Errorf("%w: usbip attach process started but did not return a trustworthy port: %v", ErrAttachmentOutcomeUnknown, err)
	}
	port, err := parseUSBIPTerseAttachPort(string(output))
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrAttachmentOutcomeUnknown, err)
	}
	return port, nil
}

// classifyUSBIPDetachCommandResult distinguishes a command that could not be
// started from one that ran but did not report a successful detach.
func classifyUSBIPDetachCommandResult(err error) error {
	if err == nil {
		return nil
	}
	var startErr *exec.Error
	if errors.As(err, &startErr) {
		return err
	}
	return fmt.Errorf("%w: usbip detach process started but did not report success: %v", ErrDetachmentOutcomeUnknown, err)
}

// classifyNativeDetachResult is called only after PLUGOUT_HARDWARE has been
// submitted. A returned error therefore leaves the outcome unknown.
func classifyNativeDetachResult(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: PLUGOUT_HARDWARE DeviceIoControl failed: %v", ErrDetachmentOutcomeUnknown, err)
}

func usbipAttachCommandArgs(usbipServerPort uint16, busID string) []string {
	return []string{
		"--tcp-port", strconv.FormatUint(uint64(usbipServerPort), 10),
		"attach", "-r", "127.0.0.1", "-b", busID, "--terse",
	}
}

func usbipDetachCommandArgs(port int32) ([]string, error) {
	if port <= 0 {
		return nil, fmt.Errorf("invalid USB/IP import port %d", port)
	}
	return []string{"detach", "-p", strconv.FormatInt(int64(port), 10)}, nil
}
