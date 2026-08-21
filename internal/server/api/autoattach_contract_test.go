package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"reflect"
	"sync"
	"testing"

	"github.com/Alia5/VIIPER/usbip"
)

// timingRecordingHandler is a minimal slog.Handler that records every emitted record so a test
// can assert on the "attachment-timing" log line without depending on any real logging backend.
// Shared by the fallback-layer tests here and the native-ioctl/command-layer tests in
// autoattach_windows_test.go.
type timingRecordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *timingRecordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *timingRecordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *timingRecordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *timingRecordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *timingRecordingHandler) timingRecords() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []slog.Record
	for _, r := range h.records {
		if r.Message == "attachment-timing" {
			out = append(out, r)
		}
	}
	return out
}

func recordAttrs(r slog.Record) map[string]any {
	m := make(map[string]any, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.Any()
		return true
	})
	return m
}

func requireNonNegativeInt64(t *testing.T, attrs map[string]any, key string) {
	t.Helper()
	v, ok := attrs[key]
	if !ok {
		t.Fatalf("missing timing field %q", key)
	}
	n, ok := v.(int64)
	if !ok {
		t.Fatalf("timing field %q = %v (%T), want int64", key, v, v)
	}
	if n < 0 {
		t.Fatalf("timing field %q = %d, want >= 0", key, n)
	}
}

func TestParseUSBIPTerseAttachPort(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		want    int32
		wantErr bool
	}{
		{name: "positive port", output: "17\n", want: 17},
		{name: "whitespace around a single port", output: " 42 \r\n", want: 42},
		//nolint:misspell // usbip-win2 v0.9.7.7 emits this exact historical spelling.
		{name: "normal output is not terse", output: "succesfully attached to port 17\n", wantErr: true},
		{name: "unrelated numbers", output: "server 3241, port 17", wantErr: true},
		{name: "zero", output: "0", wantErr: true},
		{name: "negative", output: "-1", wantErr: true},
		{name: "multiple ports", output: "17\n18\n", wantErr: true},
		{name: "empty", output: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseUSBIPTerseAttachPort(tt.output)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseUSBIPTerseAttachPort(%q) error = %v, wantErr %v", tt.output, err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("parseUSBIPTerseAttachPort(%q) = %d, want %d", tt.output, got, tt.want)
			}
		})
	}
}

func TestUSBIPCommandArgumentsTargetOneExactPort(t *testing.T) {
	attach := usbipAttachCommandArgs(3241, "9-12")
	wantAttach := []string{"--tcp-port", "3241", "attach", "-r", "127.0.0.1", "-b", "9-12", "--terse"}
	if !reflect.DeepEqual(attach, wantAttach) {
		t.Fatalf("attach arguments = %#v, want %#v", attach, wantAttach)
	}

	for _, port := range []int32{1, 217} {
		got, err := usbipDetachCommandArgs(port)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"detach", "-p", map[int32]string{1: "1", 217: "217"}[port]}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("detach arguments for port %d = %#v, want %#v", port, got, want)
		}
	}
	for _, port := range []int32{0, -1, -2} {
		if _, err := usbipDetachCommandArgs(port); err == nil {
			t.Fatalf("detach arguments accepted unsafe port %d", port)
		}
	}
}

func TestTrackedAttachFallbackIsSingleLayerAndFailClosed(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	meta := &usbip.ExportMeta{BusID: 1, DevID: 2}

	t.Run("known native failure falls back exactly once", func(t *testing.T) {
		commandCalls := 0
		attachment, err := attachLocalhostClientWithFallback(context.Background(), meta, 3241, true, logger,
			func(context.Context, *usbip.ExportMeta, uint16, *slog.Logger) (LocalhostAttachment, error) {
				return LocalhostAttachment{}, errors.New("device interface absent")
			},
			func(context.Context, *usbip.ExportMeta, uint16, *slog.Logger) (LocalhostAttachment, error) {
				commandCalls++
				return LocalhostAttachment{Backend: LocalhostAttachmentBackendCommand, Port: 11}, nil
			},
		)
		if err != nil || attachment.Port != 11 || commandCalls != 1 {
			t.Fatalf("attachment=%+v err=%v commandCalls=%d", attachment, err, commandCalls)
		}
	})

	t.Run("unknown native outcome does not fall back", func(t *testing.T) {
		commandCalls := 0
		_, err := attachLocalhostClientWithFallback(context.Background(), meta, 3241, true, logger,
			func(context.Context, *usbip.ExportMeta, uint16, *slog.Logger) (LocalhostAttachment, error) {
				return LocalhostAttachment{}, ErrAttachmentOutcomeUnknown
			},
			func(context.Context, *usbip.ExportMeta, uint16, *slog.Logger) (LocalhostAttachment, error) {
				commandCalls++
				return LocalhostAttachment{Backend: LocalhostAttachmentBackendCommand, Port: 11}, nil
			},
		)
		if !errors.Is(err, ErrAttachmentOutcomeUnknown) {
			t.Fatalf("error = %v, want ErrAttachmentOutcomeUnknown", err)
		}
		if commandCalls != 0 {
			t.Fatalf("command fallback called %d times after unknown outcome", commandCalls)
		}
	})
}

func TestAttachmentFallbackTimingRecordsNativeAttemptedAndFallbackUsed(t *testing.T) {
	meta := &usbip.ExportMeta{BusID: 1, DevID: 2}

	t.Run("known native failure: nativeAttempted and fallbackUsed both true", func(t *testing.T) {
		handler := &timingRecordingHandler{}
		logger := slog.New(handler)
		attachment, err := attachLocalhostClientWithFallback(context.Background(), meta, 3241, true, logger,
			func(context.Context, *usbip.ExportMeta, uint16, *slog.Logger) (LocalhostAttachment, error) {
				return LocalhostAttachment{}, errors.New("device interface absent")
			},
			func(context.Context, *usbip.ExportMeta, uint16, *slog.Logger) (LocalhostAttachment, error) {
				return LocalhostAttachment{Backend: LocalhostAttachmentBackendCommand, Port: 11}, nil
			},
		)
		if err != nil || attachment.Port != 11 {
			t.Fatalf("attachment=%+v err=%v", attachment, err)
		}
		records := handler.timingRecords()
		if len(records) != 1 {
			t.Fatalf("timing records = %d, want exactly 1", len(records))
		}
		attrs := recordAttrs(records[0])
		if attrs["layer"] != "fallback" || attrs["result"] != "success" || attrs["backend"] != "command" {
			t.Fatalf("unexpected timing attrs: %+v", attrs)
		}
		if attrs["nativeAttempted"] != true || attrs["fallbackUsed"] != true {
			t.Fatalf("nativeAttempted/fallbackUsed = %+v, want true/true", attrs)
		}
		requireNonNegativeInt64(t, attrs, "totalUs")
	})

	t.Run("unknown native outcome: fallbackUsed stays false", func(t *testing.T) {
		handler := &timingRecordingHandler{}
		logger := slog.New(handler)
		commandCalls := 0
		_, err := attachLocalhostClientWithFallback(context.Background(), meta, 3241, true, logger,
			func(context.Context, *usbip.ExportMeta, uint16, *slog.Logger) (LocalhostAttachment, error) {
				return LocalhostAttachment{}, ErrAttachmentOutcomeUnknown
			},
			func(context.Context, *usbip.ExportMeta, uint16, *slog.Logger) (LocalhostAttachment, error) {
				commandCalls++
				return LocalhostAttachment{Backend: LocalhostAttachmentBackendCommand, Port: 11}, nil
			},
		)
		if !errors.Is(err, ErrAttachmentOutcomeUnknown) || commandCalls != 0 {
			t.Fatalf("err=%v commandCalls=%d, want ErrAttachmentOutcomeUnknown and 0 fallback calls", err, commandCalls)
		}
		records := handler.timingRecords()
		if len(records) != 1 {
			t.Fatalf("timing records = %d, want exactly 1", len(records))
		}
		attrs := recordAttrs(records[0])
		if attrs["result"] != "unsafe-outcome-unknown" || attrs["nativeAttempted"] != true || attrs["fallbackUsed"] != false || attrs["backend"] != "native-ioctl" {
			t.Fatalf("unexpected timing attrs: %+v", attrs)
		}
	})

	t.Run("useNativeIOCTL=false: nativeAttempted false, command runs once", func(t *testing.T) {
		handler := &timingRecordingHandler{}
		logger := slog.New(handler)
		commandCalls := 0
		_, err := attachLocalhostClientWithFallback(context.Background(), meta, 3241, false, logger,
			func(context.Context, *usbip.ExportMeta, uint16, *slog.Logger) (LocalhostAttachment, error) {
				t.Fatal("native must not be called when useNativeIOCTL is false")
				return LocalhostAttachment{}, nil
			},
			func(context.Context, *usbip.ExportMeta, uint16, *slog.Logger) (LocalhostAttachment, error) {
				commandCalls++
				return LocalhostAttachment{Backend: LocalhostAttachmentBackendCommand, Port: 12}, nil
			},
		)
		if err != nil || commandCalls != 1 {
			t.Fatalf("err=%v commandCalls=%d, want nil/1", err, commandCalls)
		}
		attrs := recordAttrs(handler.timingRecords()[0])
		if attrs["nativeAttempted"] != false || attrs["fallbackUsed"] != false {
			t.Fatalf("unexpected timing attrs: %+v", attrs)
		}
	})
}

func TestClassifyUSBIPAttachCommandResult(t *testing.T) {
	port, err := classifyUSBIPAttachCommandResult([]byte("21\n"), nil)
	if err != nil || port != 21 {
		t.Fatalf("successful command result = port %d, err %v", port, err)
	}
	if _, err := classifyUSBIPAttachCommandResult([]byte("unexpected 21"), nil); !errors.Is(err, ErrAttachmentOutcomeUnknown) {
		t.Fatalf("unparseable successful command error = %v, want unknown", err)
	}
	startErr := &exec.Error{Name: "usbip", Err: errors.New("not found")}
	if _, err := classifyUSBIPAttachCommandResult(nil, startErr); errors.Is(err, ErrAttachmentOutcomeUnknown) {
		t.Fatalf("process start failure must remain a known failure: %v", err)
	}
	if _, err := classifyUSBIPAttachCommandResult(nil, &exec.ExitError{}); !errors.Is(err, ErrAttachmentOutcomeUnknown) {
		t.Fatalf("started process failure error = %v, want unknown", err)
	}
}

func TestDetachOutcomeClassification(t *testing.T) {
	if err := classifyUSBIPDetachCommandResult(nil); err != nil {
		t.Fatalf("successful detach classification = %v", err)
	}
	startErr := &exec.Error{Name: "usbip", Err: errors.New("not found")}
	if err := classifyUSBIPDetachCommandResult(startErr); errors.Is(err, ErrDetachmentOutcomeUnknown) {
		t.Fatalf("process-start failure must remain known: %v", err)
	}
	for _, err := range []error{&exec.ExitError{}, context.Canceled} {
		if got := classifyUSBIPDetachCommandResult(err); !errors.Is(got, ErrDetachmentOutcomeUnknown) {
			t.Fatalf("started command error %T = %v, want unknown", err, got)
		}
	}
	if got := classifyNativeDetachResult(errors.New("ioctl failed")); !errors.Is(got, ErrDetachmentOutcomeUnknown) {
		t.Fatalf("submitted IOCTL error = %v, want unknown", got)
	}
}
