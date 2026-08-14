package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"reflect"
	"testing"

	"github.com/Alia5/VIIPER/usbip"
)

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
	wantAttach := []string{"--tcp-port", "3241", "attach", "-r", "localhost", "-b", "9-12", "--terse"}
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
