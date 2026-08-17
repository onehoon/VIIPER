package main

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"runtime/cgo"
	"sync"
	"testing"

	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/usbip"
)

type lifecycleFamily struct {
	name   string
	create func(uintptr, *deviceHandle, uint32) bool
	remove func(uintptr) typedDeviceRemoveResult
}

var deckAndXboxLifecycleFamilies = []lifecycleFamily{
	{name: "Steam Deck", create: func(s uintptr, h *deviceHandle, bus uint32) bool {
		return createSteamDeckDevice(s, h, bus, false, 0, 0)
	}, remove: removeSteamDeckDeviceResult},
	{name: "Xbox360", create: func(s uintptr, h *deviceHandle, bus uint32) bool {
		return createXbox360Device(s, h, bus, false, 0, 0, 0)
	}, remove: removeXbox360DeviceResult},
}

type attachmentCall struct {
	attachment api.LocalhostAttachment
	err        error
}

func closeUSBServerForTest(handle uintptr) bool {
	fn := reflect.ValueOf(CloseUSBServer)
	arg := reflect.New(fn.Type().In(0)).Elem()
	arg.SetUint(uint64(handle))
	return fn.Call([]reflect.Value{arg})[0].Bool()
}

func publicServerHandleForTest(t *testing.T, hw *usbServerHandleWrapper) uintptr {
	t.Helper()
	h := cgo.NewHandle(hw)
	raw := uintptr(h)
	serverHandleRecords.Store(raw, hw)
	t.Cleanup(func() {
		if _, ok := serverHandleRecords.Load(raw); ok {
			serverHandleRecords.Delete(raw)
			h.Delete()
		}
	})
	return raw
}

func installBlockingAttach(t *testing.T, hw *usbServerHandleWrapper, result attachmentCall) (started, release chan struct{}, calls *int) {
	t.Helper()
	started, release = make(chan struct{}), make(chan struct{})
	count := 0
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		count++
		close(started)
		<-release
		return result.attachment, result.err
	}
	return started, release, &count
}

func requireContendedLifecycleAttempt(t *testing.T, hw *usbServerHandleWrapper, operation string) <-chan struct{} {
	t.Helper()
	contended := make(chan struct{})
	var once sync.Once
	hw.onLifecycleLockAttempt = func(attemptedOperation string) {
		if attemptedOperation != operation {
			return
		}
		if hw.lifecycleMu.TryLock() {
			hw.lifecycleMu.Unlock()
			return
		}
		once.Do(func() { close(contended) })
	}
	return contended
}

func TestTypedAttachAndRemoveSerializeAcrossDeckAndXbox(t *testing.T) {
	for i, family := range deckAndXboxLifecycleFamilies {
		t.Run(family.name, func(t *testing.T) {
			busID := uint32(9800 + i)
			hw, bus := newLifecycleTestServer(t, busID)
			started, release, attachCalls := installBlockingAttach(t, hw, attachmentCall{attachment: api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: int32(4100 + i)}})
			detachCalls := 0
			var detached api.LocalhostAttachment
			hw.ops.detachLocalhost = func(_ context.Context, a api.LocalhostAttachment, _ *slog.Logger) error {
				detachCalls++
				detached = a
				return nil
			}
			serverHandle := serverHandleForTest(t, hw)
			var h deviceHandle
			if !family.create(serverHandle, &h, busID) {
				t.Fatal("detached-ready create failed")
			}
			attachResult := make(chan deviceAttachResult, 1)
			removeResult := make(chan typedDeviceRemoveResult, 1)
			go func() { attachResult <- attachUSBDeviceResult(uintptr(h)) }()
			<-started
			removeContended := requireContendedLifecycleAttempt(t, hw, "remove")
			go func() { removeResult <- family.remove(uintptr(h)) }()
			<-removeContended
			close(release)
			if got := <-attachResult; got != deviceAttachSuccess {
				t.Fatalf("attach=%d", got)
			}
			if got := <-removeResult; got != typedDeviceRemoveSuccess {
				t.Fatalf("remove=%d", got)
			}
			if *attachCalls != 1 || detachCalls != 1 {
				t.Fatalf("attach calls=%d detach calls=%d", *attachCalls, detachCalls)
			}
			if detached.Backend != api.LocalhostAttachmentBackendCommand || detached.Port != int32(4100+i) {
				t.Fatalf("detached token=%#v", detached)
			}
			if lookupIdentityExists(uintptr(h)) || len(bus.Devices()) != 0 || hw.state != serverActive {
				t.Fatalf("remove did not finalize safely: state=%s devices=%d", hw.state, len(bus.Devices()))
			}
		})
	}
}

func TestQueuedRemoveAfterAttachFailureAndUnknown(t *testing.T) {
	for i, family := range deckAndXboxLifecycleFamilies {
		t.Run(family.name, func(t *testing.T) {
			for _, tc := range []struct {
				name       string
				err        error
				wantAttach deviceAttachResult
				wantRemove typedDeviceRemoveResult
			}{
				{name: "known", err: errors.New("known attach failure"), wantAttach: deviceAttachRetryableFailure, wantRemove: typedDeviceRemoveSuccess},
				{name: "unknown", err: api.ErrAttachmentOutcomeUnknown, wantAttach: deviceAttachUnsafeOutcomeUnknown, wantRemove: typedDeviceRemoveUnsafeOutcomeUnknown},
			} {
				t.Run(tc.name, func(t *testing.T) {
					hw, bus := newLifecycleTestServer(t, uint32(9820+i*2))
					started, release, calls := installBlockingAttach(t, hw, attachmentCall{err: tc.err})
					detachCalls := 0
					hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error { detachCalls++; return nil }
					var h deviceHandle
					if !family.create(serverHandleForTest(t, hw), &h, uint32(9820+i*2)) {
						t.Fatal("create failed")
					}
					ar := make(chan deviceAttachResult, 1)
					rr := make(chan typedDeviceRemoveResult, 1)
					go func() { ar <- attachUSBDeviceResult(uintptr(h)) }()
					<-started
					removeContended := requireContendedLifecycleAttempt(t, hw, "remove")
					go func() { rr <- family.remove(uintptr(h)) }()
					<-removeContended
					close(release)
					if got := <-ar; got != tc.wantAttach {
						t.Fatalf("attach=%d want=%d", got, tc.wantAttach)
					}
					if got := <-rr; got != tc.wantRemove {
						t.Fatalf("remove=%d want=%d", got, tc.wantRemove)
					}
					if *calls != 1 || detachCalls != 0 {
						t.Fatalf("attach calls=%d detach calls=%d", *calls, detachCalls)
					}
					if tc.name == "unknown" {
						if lookupIdentityExists(uintptr(h)) == false || hw.state != serverCloseFailed {
							t.Fatalf("unknown ownership was lost: state=%s", hw.state)
						}
						if len(bus.Devices()) == 0 {
							t.Fatal("unknown device was logically removed")
						}
					} else if lookupIdentityExists(uintptr(h)) || len(bus.Devices()) != 0 || hw.state != serverActive {
						t.Fatalf("known failure remove did not finalize: state=%s", hw.state)
					}
				})
			}
		})
	}
}

func TestQueuedRemoveAfterExplicitDetachDoesNotDoubleDetach(t *testing.T) {
	for i, family := range deckAndXboxLifecycleFamilies {
		t.Run(family.name, func(t *testing.T) {
			hw, bus := newLifecycleTestServer(t, uint32(9840+i))
			attachCalls, detachCalls := 0, 0
			hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
				attachCalls++
				return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendNativeIOCTL, Port: int32(4200 + i)}, nil
			}
			started, release := make(chan struct{}), make(chan struct{})
			hw.ops.detachLocalhost = func(_ context.Context, a api.LocalhostAttachment, _ *slog.Logger) error {
				detachCalls++
				if detachCalls == 1 {
					close(started)
					<-release
				}
				if a.Port != int32(4200+i) {
					t.Fatalf("wrong token=%#v", a)
				}
				return nil
			}
			var h deviceHandle
			if !family.create(serverHandleForTest(t, hw), &h, uint32(9840+i)) || attachUSBDeviceResult(uintptr(h)) != deviceAttachSuccess {
				t.Fatal("attach setup failed")
			}
			dr := make(chan deviceDetachResult, 1)
			rr := make(chan typedDeviceRemoveResult, 1)
			go func() { dr <- detachUSBDeviceResult(uintptr(h)) }()
			<-started
			removeContended := requireContendedLifecycleAttempt(t, hw, "remove")
			go func() { rr <- family.remove(uintptr(h)) }()
			<-removeContended
			close(release)
			if <-dr != deviceDetachSuccess || <-rr != typedDeviceRemoveSuccess {
				t.Fatal("detach/remove failed")
			}
			if attachCalls != 1 || detachCalls != 1 || lookupIdentityExists(uintptr(h)) || len(bus.Devices()) != 0 {
				t.Fatalf("attach=%d detach=%d devices=%d", attachCalls, detachCalls, len(bus.Devices()))
			}
		})
	}
}

func TestUnknownDetachAndQueuedRemoveStayFailClosed(t *testing.T) {
	for i, family := range deckAndXboxLifecycleFamilies {
		t.Run(family.name, func(t *testing.T) {
			hw, _ := newLifecycleTestServer(t, uint32(9860+i))
			calls := 0
			hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
				return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: int32(4300 + i)}, nil
			}
			started, release := make(chan struct{}), make(chan struct{})
			hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
				calls++
				if calls == 1 {
					close(started)
					<-release
				}
				return api.ErrDetachmentOutcomeUnknown
			}
			var h deviceHandle
			if !family.create(serverHandleForTest(t, hw), &h, uint32(9860+i)) || attachUSBDeviceResult(uintptr(h)) != deviceAttachSuccess {
				t.Fatal("attach setup failed")
			}
			dr := make(chan deviceDetachResult, 1)
			rr := make(chan typedDeviceRemoveResult, 1)
			go func() { dr <- detachUSBDeviceResult(uintptr(h)) }()
			<-started
			removeContended := requireContendedLifecycleAttempt(t, hw, "remove")
			go func() { rr <- family.remove(uintptr(h)) }()
			<-removeContended
			close(release)
			if <-dr != deviceDetachUnsafeOutcomeUnknown || <-rr != typedDeviceRemoveUnsafeOutcomeUnknown {
				t.Fatal("unknown detach classification changed")
			}
			if calls != 1 || !lookupIdentityExists(uintptr(h)) || hw.state != serverCloseFailed {
				t.Fatalf("calls=%d exists=%t state=%s", calls, lookupIdentityExists(uintptr(h)), hw.state)
			}
		})
	}
}

func TestPublicCloseSerializesWithExplicitAttach(t *testing.T) {
	hw, bus := newLifecycleTestServer(t, 9880)
	started, release, attachCalls := installBlockingAttach(t, hw, attachmentCall{attachment: api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 4400}})
	detachCalls := 0
	var detached api.LocalhostAttachment
	hw.ops.detachLocalhost = func(_ context.Context, a api.LocalhostAttachment, _ *slog.Logger) error {
		detachCalls++
		detached = a
		return nil
	}
	var h deviceHandle
	serverHandle := publicServerHandleForTest(t, hw)
	if !createXbox360Device(serverHandle, &h, 9880, false, 0, 0, 0) {
		t.Fatal("create failed")
	}
	ar := make(chan deviceAttachResult, 1)
	cr := make(chan bool, 1)
	go func() { ar <- attachUSBDeviceResult(uintptr(h)) }()
	<-started
	closeContended := requireContendedLifecycleAttempt(t, hw, "close")
	go func() { cr <- closeUSBServerForTest(serverHandle) }()
	<-closeContended
	close(release)
	if <-ar != deviceAttachSuccess || !<-cr {
		t.Fatal("public attach/close failed")
	}
	if attachCalls == nil || *attachCalls != 1 || detachCalls != 1 || detached.Port != 4400 {
		t.Fatalf("attach=%d detach=%d token=%#v", *attachCalls, detachCalls, detached)
	}
	if _, serverStillExists := lookupServerHandle(serverHandle); lookupIdentityExists(uintptr(h)) || serverStillExists || len(bus.Devices()) != 0 {
		t.Fatalf("public close left ownership evidence: device=%t server=%t buses=%d", lookupIdentityExists(uintptr(h)), serverStillExists, len(bus.Devices()))
	}
}

func TestPublicCloseTransportPhaseSerializesAndRetries(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9881)
	started, release := make(chan struct{}), make(chan struct{})
	closeCalls := 0
	hw.ops.close = func(*usb.Server) error {
		closeCalls++
		if closeCalls == 1 {
			close(started)
			<-release
		}
		return nil
	}
	h := cgo.NewHandle(hw)
	serverHandleRecords.Store(uintptr(h), hw)
	first := make(chan bool, 1)
	second := make(chan bool, 1)
	go func() { first <- closeUSBServerForTest(uintptr(h)) }()
	<-started
	go func() { second <- closeUSBServerForTest(uintptr(h)) }()
	secondOK := <-second
	close(release)
	firstOK := <-first
	if !firstOK || secondOK || closeCalls != 1 {
		t.Fatalf("first/second close or duplicate transport close: first=%t second=%t calls=%d", firstOK, secondOK, closeCalls)
	}
	if _, ok := lookupServerHandle(uintptr(h)); ok {
		t.Fatal("server handle remained registered after successful close")
	}
}

func TestPublicCloseRetriesKnownTransportFailure(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9882)
	closeCalls := 0
	hw.ops.close = func(*usb.Server) error {
		closeCalls++
		if closeCalls == 1 {
			return errors.New("known transport close failure")
		}
		return nil
	}
	h := publicServerHandleForTest(t, hw)
	if closeUSBServerForTest(h) || hw.state != serverCloseFailed || closeCalls != 1 {
		t.Fatalf("first close state=%s calls=%d", hw.state, closeCalls)
	}
	if !closeUSBServerForTest(h) || hw.state != serverClosed || closeCalls != 2 {
		t.Fatalf("retry close state=%s calls=%d", hw.state, closeCalls)
	}
}

func TestPublicCloseFailsClosedAfterUnknownAttach(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9883)
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		return api.LocalhostAttachment{}, api.ErrAttachmentOutcomeUnknown
	}
	serverHandle := publicServerHandleForTest(t, hw)
	var h deviceHandle
	if !createXbox360Device(serverHandle, &h, 9883, false, 0, 0, 0) || attachUSBDeviceResult(uintptr(h)) != deviceAttachUnsafeOutcomeUnknown {
		t.Fatal("unknown attach setup failed")
	}
	if closeUSBServerForTest(serverHandle) || hw.state != serverCloseFailed || !lookupIdentityExists(uintptr(h)) {
		t.Fatalf("unknown attach was destructively closed: state=%s handle=%t", hw.state, lookupIdentityExists(uintptr(h)))
	}
}

func TestDeckAndXboxCoexistOnCallerOwnedBus(t *testing.T) {
	hw, bus := newLifecycleTestServer(t, 9890)
	attachCalls, detachCalls := 0, 0
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		attachCalls++
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: int32(4500 + attachCalls)}, nil
	}
	hw.ops.detachLocalhost = func(_ context.Context, a api.LocalhostAttachment, _ *slog.Logger) error {
		detachCalls++
		wantPort := int32(4502)
		if detachCalls == 2 {
			wantPort = 4501
		}
		if a.Port != wantPort {
			t.Fatalf("crossed attachment token=%#v", a)
		}
		return nil
	}
	serverHandle := serverHandleForTest(t, hw)
	var deck, xbox deviceHandle
	if !createSteamDeckDevice(serverHandle, &deck, 9890, false, 0, 0) || !createXbox360Device(serverHandle, &xbox, 9890, false, 0, 0, 0) {
		t.Fatal("coexistence create failed")
	}
	deckID, deckOK := lookupDeviceIdentity(uintptr(deck))
	xboxID, xboxOK := lookupDeviceIdentity(uintptr(xbox))
	if !deckOK || !xboxOK || deckID.exportMeta.DevID == xboxID.exportMeta.DevID {
		t.Fatalf("device identities=%#v/%#v", deckID, xboxID)
	}
	if attachUSBDeviceResult(uintptr(deck)) != deviceAttachSuccess || attachUSBDeviceResult(uintptr(xbox)) != deviceAttachSuccess {
		t.Fatal("coexistence attach failed")
	}
	if detachUSBDeviceResult(uintptr(xbox)) != deviceDetachSuccess || removeXbox360DeviceResult(uintptr(xbox)) != typedDeviceRemoveSuccess {
		t.Fatal("Xbox360 cleanup failed")
	}
	if !setSteamDeckDeviceState(uintptr(deck), steamDeckState{A: true}) || queryDeviceAttachmentStateValue(t, uintptr(deck)) != deviceAttachmentQueryAttached {
		t.Fatal("Deck was invalidated by Xbox360 removal")
	}
	if detachUSBDeviceResult(uintptr(deck)) != deviceDetachSuccess || removeSteamDeckDeviceResult(uintptr(deck)) != typedDeviceRemoveSuccess || len(bus.Devices()) != 0 {
		t.Fatal("Deck cleanup failed")
	}
	if attachCalls != 2 || detachCalls != 2 || hw.state != serverActive {
		t.Fatalf("calls attach=%d detach=%d state=%s", attachCalls, detachCalls, hw.state)
	}
}

func queryDeviceAttachmentStateValue(t *testing.T, handle uintptr) deviceAttachmentQueryState {
	t.Helper()
	state, ok := queryDeviceAttachmentState(handle)
	if !ok {
		t.Fatal("attachment state query failed")
	}
	return state
}

func TestServerCloseFailedIsServerWideForDeckAndXbox(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9891)
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 4600}, nil
	}
	detachCalls := 0
	hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
		detachCalls++
		if detachCalls == 1 {
			return api.ErrDetachmentOutcomeUnknown
		}
		return nil
	}
	serverHandle := serverHandleForTest(t, hw)
	var deck, xbox deviceHandle
	if !createSteamDeckDevice(serverHandle, &deck, 9891, false, 0, 0) || !createXbox360Device(serverHandle, &xbox, 9891, false, 0, 0, 0) {
		t.Fatal("create failed")
	}
	if attachUSBDeviceResult(uintptr(deck)) != deviceAttachSuccess || attachUSBDeviceResult(uintptr(xbox)) != deviceAttachSuccess {
		t.Fatal("attach failed")
	}
	if detachUSBDeviceResult(uintptr(xbox)) != deviceDetachUnsafeOutcomeUnknown || hw.state != serverCloseFailed {
		t.Fatal("server did not enter close-failed")
	}
	if !lookupIdentityExists(uintptr(deck)) || !lookupIdentityExists(uintptr(xbox)) || setSteamDeckDeviceState(uintptr(deck), steamDeckState{A: true}) {
		t.Fatal("server-wide fail-closed boundary was bypassed")
	}
	if detachCalls != 1 {
		t.Fatalf("automatic retry occurred: detach calls=%d", detachCalls)
	}
}

func TestIndependentServersRemainIsolatedWithDistinctVirtualBusIDs(t *testing.T) {
	hwA, _ := newLifecycleTestServer(t, 9900)
	hwB, _ := newLifecycleTestServer(t, 9901)
	hwA.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 4700}, nil
	}
	hwA.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
		return api.ErrDetachmentOutcomeUnknown
	}
	hwB.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 4701}, nil
	}
	hwB.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error { return nil }
	var a, b deviceHandle
	serverA, serverB := serverHandleForTest(t, hwA), publicServerHandleForTest(t, hwB)
	if !createXbox360Device(serverA, &a, 9900, false, 0, 0, 0) || !createSteamDeckDevice(serverB, &b, 9901, false, 0, 0) {
		t.Fatal("independent create failed")
	}
	if attachUSBDeviceResult(uintptr(a)) != deviceAttachSuccess || attachUSBDeviceResult(uintptr(b)) != deviceAttachSuccess {
		t.Fatal("independent attach failed")
	}
	if detachUSBDeviceResult(uintptr(a)) != deviceDetachUnsafeOutcomeUnknown || hwA.state != serverCloseFailed || hwB.state != serverActive {
		t.Fatal("server isolation state changed")
	}
	if detachUSBDeviceResult(uintptr(b)) != deviceDetachSuccess || removeSteamDeckDeviceResult(uintptr(b)) != typedDeviceRemoveSuccess || !closeUSBServerForTest(serverB) {
		t.Fatal("independent server B lifecycle failed")
	}
	if !lookupIdentityExists(uintptr(a)) || hwA.state != serverCloseFailed {
		t.Fatal("server A ownership evidence changed")
	}
}
