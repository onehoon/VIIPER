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

	"github.com/Alia5/VIIPER/usbip"
)

type localhostAttachAttempt func(context.Context, *usbip.ExportMeta, uint16, *slog.Logger) (LocalhostAttachment, error)

// attachLocalhostClientWithFallback makes one layer responsible for native to
// command fallback. Unknown outcomes deliberately bypass fallback.
func attachLocalhostClientWithFallback(ctx context.Context, meta *usbip.ExportMeta, port uint16, useNativeIOCTL bool, logger *slog.Logger, native, command localhostAttachAttempt) (LocalhostAttachment, error) {
	if !useNativeIOCTL {
		return command(ctx, meta, port, logger)
	}

	attachment, err := native(ctx, meta, port, logger)
	if err == nil {
		return attachment, nil
	}
	if errors.Is(err, ErrAttachmentOutcomeUnknown) {
		return LocalhostAttachment{}, err
	}

	logger.Error("native USB/IP attach failed before ownership could be created; using command fallback", "error", err)
	return command(ctx, meta, port, logger)
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
		"attach", "-r", "localhost", "-b", busID, "--terse",
	}
}

func usbipDetachCommandArgs(port int32) ([]string, error) {
	if port <= 0 {
		return nil, fmt.Errorf("invalid USB/IP import port %d", port)
	}
	return []string{"detach", "-p", strconv.FormatInt(int64(port), 10)}, nil
}
