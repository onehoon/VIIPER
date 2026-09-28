# Work Order — Remove Temporary Xbox360 Rumble / USB-IP Boundary Diagnostics

- Target repository: `onehoon/VIIPER`
- Target branch for implementation: create a focused cleanup branch from the latest `main`
- Reviewed runtime baseline before this work-order commit: `main@973f072365cd40ac6c0a03d5d07b8eedf1a8b338`
- Primary diagnostic commits to unwind:
  - PR #47 / `e13595e6441a7fae7087619232455df8e7b0e7ed` — opt-in Xbox360 rumble boundary tracing
  - PR #48 / `6fb885ef1e5db7a8fc02e92128ca8fea203e7304` — forced-on CTW nightly rumble diagnostics
  - PR #52 / `abc9bd0aa1dc061aca92f75cdf6069e259de9476` — Xbox360 USB/IP OUT receive/writer boundary trace
- Must preserve:
  - PR #50 zero-copy Windows localhost attach policy
  - PR #53 usbip-win2 0.9.8.0 / 0.9.8.1 native attach ABI support
  - generic `libVIIPER.log` infrastructure
  - `SetDiagnosticLogDirectory`
  - `VIIPERLogCallback`
  - generic attachment / teardown diagnostics
  - canonical managed transport drain and fail-close behavior

## 1. Goal

Remove the temporary high-frequency Xbox360 rumble diagnostic instrumentation that was added to locate the missing terminal rumble STOP.

The diagnostic campaign has already localized the original failure above the VIIPER server receive boundary, and the maintainer has validated the usbip-win2 0.9.8.1 path without the prior rumble latch in the controlled hardware run. The remaining nightly / field soak is a release-confidence gate, not a reason to keep permanent per-rumble packet tracing in the production fork.

This cleanup must return the steady-state Xbox360 path to the simple behavior it had before PR #47:

```text
USB/IP EP1 OUT
  -> Xbox360.HandleTransfer
  -> recognize normal 8-byte rumble packet
  -> snapshot registered callback under callbackMu
  -> invoke callback outside callbackMu
  -> normal RET_SUBMIT path
```

No trace session, trace sequence, trace finalizer, diagnostic-only transport hook, or per-rumble file record should remain in production code.

This is a cleanup PR, not another rumble investigation.

---

## 2. Execution gate

Prepare and implement this cleanup now, but do not treat removal of the diagnostics as evidence that the usbip-win2 0.9.8.1 field validation is complete.

Merge / downstream artifact adoption should occur only after the maintainer accepts the nightly soak.

If a real user reproduction of the old latch appears before merge, keep the cleanup branch separate and use the current diagnostic build for that investigation instead of re-adding partial trace machinery to the cleanup branch.

Do not add a runtime switch just to preserve the old diagnostics "in case they are needed later."

The historical implementation remains available in Git history at PR #47 / #48 / #52.

---

## 3. Source review findings

### 3.1 The temporary diagnostic stack is larger than the log statements themselves

Current `main` contains a full diagnostic ownership/lifecycle stack:

```text
device/xbox360/device.go
  -> atomic.Pointer[RumbleTrace]

device/xbox360/rumble_trace.go
  -> RumbleTrace
  -> TraceSessionID
  -> TraceSeq
  -> Start / Raw / Parsed / CallbackDispatch / End / Abort
  -> USB/IP ingress / writer-accepted records

lib/viiper/xbox360.go
  -> forced InstallRumbleTrace on every canonical Xbox360 create
  -> rumbleTraceLoggerFactory
  -> identity-ready creation hook
  -> delayed trace abort after transport drain

lib/viiper/viiper.go
  -> rumbleTraceFinalizer
  -> rumbleTraceAbortAfterDrain
  -> rumble trace collection in teardown results
  -> trace-specific creation return value
  -> trace-specific auto-attach rollback handling

lib/viiper/bus.go
  -> collect trace sessions during bus teardown
  -> finish traces after transport drains

lib/viiper/server.go
  -> collect trace sessions during server teardown
  -> finish traces after transport drains

internal/server/usb/server.go
  -> optional usbipOutBoundaryTracer
  -> X360USBIPOutIngress hook
  -> X360USBIPOutWriterAccepted hook
```

All of the above exists to make the temporary diagnostic stream complete and correctly ordered across teardown.

Once per-rumble tracing is removed, this trace-specific lifecycle ownership has no product responsibility and should be removed with it.

### 3.2 Generic transport drain is NOT diagnostic code

Do not confuse the trace finalization layer with VIIPER's canonical transport drain.

The following is real lifecycle infrastructure and must remain:

- `Server.BeginDeviceDrain(...)`;
- `TransportDrain.Wait()`;
- device removal waiting for its normal transport drain;
- bus removal waiting for the collected normal transport drains;
- server close waiting for the collected normal transport drains;
- `ForgetDeviceTransport(...)` at the existing committed teardown boundaries.

The cleanup removes only the extra rumble-trace objects that were carried alongside those drains so `TraceEnd` / `TraceAbort` could be emitted after in-flight traffic completed.

### 3.3 The current Xbox360 callback behavior is already protected elsewhere

Do not keep the trace tests merely to preserve rumble functionality.

Existing non-diagnostic coverage already protects the real behavior:

- `device/xbox360/xbox360_test.go::TestRumble`
  - zero / STOP
  - mid rumble
  - full rumble
- `device/xbox360/callback_lifecycle_test.go`
  - callback clear / re-entry
  - in-flight callback semantics
  - callback setter vs transfer synchronization
- `lib/viiper/callback_lifecycle_test.go`
  - canonical Xbox360 callback clearing during typed/bus/server lifecycle

These tests remain and are the correct product-level regression coverage.

---

## 4. Keep the generic libVIIPER diagnostic log

Do **not** revert PR #48 wholesale.

PR #48 mixed temporary rumble tracing with a useful generic logging API.

The following must remain:

### 4.1 `SetDiagnosticLogDirectory`

Keep:

- `lib/viiper/diagnosticlog.go`;
- exported `SetDiagnosticLogDirectory`;
- `diagnosticLogDirectoryConfig`;
- `embeddedDiagnosticLogDirectory`;
- the pre-initialization `set` / `freeze` behavior;
- configured-directory path resolution.

CTW currently uses this export to place `libVIIPER.log` in its normal diagnostic bundle.

SteamAddonforClaw does not currently bind this export in its managed canonical native wrapper, so keeping it has no runtime downside for the Addon.

Do not change either consumer in this VIIPER cleanup PR.

### 4.2 Generic `libVIIPER.log`

Keep the existing low-volume library-owned logging stack:

- `buildEmbeddedLogger`;
- `openRealEmbeddedLogFileHandler`;
- `embeddedFileHandler`;
- `asyncLogWriter`;
- daily rollover;
- invalid-handle file-only logger;
- best-effort close flush;
- callback/file multi-handler behavior;
- ordinary lifecycle / attachment / teardown records.

Keep these files and their generic tests:

- `lib/viiper/diagnosticlog.go`
- `lib/viiper/embeddedlog.go`
- `lib/viiper/embeddedlog_path.go`
- `lib/viiper/embeddedlog_windows.go`
- `lib/viiper/embeddedlog_other.go`
- `lib/viiper/asynclog.go`
- `lib/viiper/dailyrollover.go`
- corresponding generic tests

Only the Xbox360 rumble-marker-specific pieces inside those files/tests should be removed.

### 4.3 `VIIPERLogCallback`

Do not change `NewUSBServer`'s existing callback ABI or semantics.

The embedding callback remains an optional synchronous observer.

This cleanup must not route per-rumble events through it and must not change its lifetime.

---

## 5. Exact production-code cleanup

### 5.1 Delete `device/xbox360/rumble_trace.go`

Delete the file completely.

Remove all definitions owned only by the temporary diagnostic:

- `RumbleTrace`;
- `rumbleTraceSessionCounter`;
- `rumbleTraceProcessID`;
- `InstallRumbleTrace`;
- `RumbleTraceSession`;
- `ClearRumbleTrace`;
- `End`;
- `Abort`;
- `finish`;
- `recordPacket`;
- `recordUSBIPOutIngress`;
- `recordUSBIPOutWriterAccepted`;
- all `X360Rumble*` and `X360USBIPOut*` structured event generation.

Do not replace it with a disabled implementation.

Do not leave a no-op trace object behind.

### 5.2 Restore `device/xbox360/device.go` to product-only rumble behavior

Remove:

```go
rumbleTrace atomic.Pointer[RumbleTrace]
```

from `Xbox360`.

Do not remove the `sync/atomic` import merely because the trace field is gone: `tick` still uses `atomic.AddUint64`.

Remove the trace branch from `HandleTransfer`:

```go
if trace := x.rumbleTrace.Load(); trace != nil {
    trace.recordPacket(...)
}
```

Prefer restoring the simple pre-PR47 OUT shape:

```go
if dir == usbip.DirOut && ep == 1 {
    if len(out) >= 8 && out[0] == 0x00 && out[1] == 0x08 {
        rumble := XRumbleState{
            LeftMotor:  out[3],
            RightMotor: out[4],
        }
        x.callbackMu.RLock()
        callback := x.rumbleFunc
        x.callbackMu.RUnlock()
        if callback != nil {
            callback(rumble)
        }
    }
}
```

Behavioral requirements:

- zero/zero must still invoke the registered callback;
- non-zero rumble must be unchanged;
- unknown/non-rumble OUT reports remain ignored by the rumble callback;
- callback lookup remains protected by `callbackMu`;
- callback invocation remains outside `callbackMu`;
- no new queue / goroutine / retry / debounce.

### 5.3 Simplify canonical Xbox360 creation in `lib/viiper/xbox360.go`

Remove:

- `rumbleTraceLoggerFactory`;
- forced `InstallRumbleTrace(...)`;
- the trace identity-ready hook;
- `traceAbort`;
- `finishRumbleTraceAbortAfterDrain(traceAbort)`.

Return Xbox360 creation to the normal typed-device creation path:

```go
shw.lifecycleMu.Lock()
h, ok, warning, rollback, backendLogs :=
    shw.createDeviceLockedPublic(busID, d, autoAttachLocalhost)
shw.backendLogLogger = nil
shw.lifecycleMu.Unlock()

backendLogs.replay(shw.logger)
emitMutationRejectedWarning(warning)
emitRollbackDiagnostic(rollback)
```

Do not change the exported Xbox360 C ABI.

Do not change:

- `CreateXbox360Device`;
- `SetXbox360DeviceState`;
- `SetXbox360RumbleCallback`;
- `RemoveXbox360Device`;
- `RemoveXbox360DeviceEx`.

### 5.4 Remove trace-specific creation plumbing from `lib/viiper/viiper.go`

Remove:

- `rumbleTraceFinalizer`;
- `rumbleTraceAbortAfterDrain`;
- `rumbleTraceFor`;
- `finishRumbleTracesAfterDrain`;
- `finishRumbleTraceAbortAfterDrain`;
- `transportTeardownResult.rumbleTraces`;
- the `device/xbox360` import if no non-diagnostic use remains.

Collapse:

```go
createDeviceLockedPublicWithIdentityHook(...)
```

back into the normal:

```go
createDeviceLockedPublic(...)
```

implementation.

The `onIdentityReady` callback exists only so the rumble trace can start after BusID/DeviceID are known and before auto-attach. Once tracing is removed, do not retain this extra creation abstraction.

Restore the normal known auto-attach failure rollback path after successful `rollbackCreatedDeviceLockedWithDiagnostic(...)`:

```go
hw.finalizeDeviceLocked(h)
return 0, false, mutationRejectedWarning{}, nil, backendLogs
```

Remove the Xbox360-only special path that creates a transport drain solely so `X360RumbleTraceAbort` can be delayed until diagnostic traffic drains.

Important:

- this instruction applies only to the trace-specific creation rollback path;
- do not remove generic drains from normal typed device removal, bus removal, or server close;
- do not change outcome-unknown fail-close behavior.

### 5.5 Remove trace collection from typed removal

In `removeDeviceLockedWithDrain`, keep the normal drain:

```go
drain := hw.s.BeginDeviceDrain(...)
```

Keep existing device removal, finalization, and `ForgetDeviceTransport`.

Only remove:

```go
rumbleTraces: []rumbleTraceFinalizer{...}
```

from the returned `transportTeardownResult`.

The result should again carry the normal drain plus the existing detach diagnostic state only.

### 5.6 Remove trace collection from `lib/viiper/bus.go`

Keep:

- bus preflight;
- callback clearing;
- attachment detach policy;
- normal per-device `BeginDeviceDrain`;
- bus removal;
- handle finalization;
- `ForgetDeviceTransport`;
- waiting on `result.drains`;
- existing teardown diagnostics.

Remove:

- `finishRumbleTracesAfterDrain(result.rumbleTraces)`;
- `rumbleTracesForDevices`;
- temporary `traces := ...` variables;
- all `rumbleTraces:` result fields.

After cleanup the flow should be:

```text
serialized logical teardown
  -> collect normal TransportDrain objects
  -> remove/finalize bus ownership
  -> release lifecycle lock
  -> wait normal TransportDrain objects
  -> replay deferred backend logs
  -> emit teardown diagnostic
```

No trace finalization phase remains.

### 5.7 Remove trace collection from `lib/viiper/server.go`

Keep the existing two-phase server close architecture and all normal transport-drain behavior.

Remove only:

- `finishRumbleTracesAfterDrain(result.rumbleTraces)`;
- `allRumbleTraces`;
- accumulation of `result.rumbleTraces`;
- assignment of the aggregate trace list into failure/success results.

Do not change:

- `logicalTeardownPending / transportClosePending / closeComplete`;
- lifecycle lock release before transport wait;
- fail-close handling;
- unknown attachment handling;
- best-effort log flush after successful close.

### 5.8 Remove USB/IP OUT boundary hooks from `internal/server/usb/server.go`

Delete the optional diagnostic interface:

```go
type usbipOutBoundaryTracer interface {
    TraceUSBIPOutIngress(...)
    TraceUSBIPOutWriterAccepted(...)
}
```

Delete:

```go
outBoundaryTracer, _ := dev.(usbipOutBoundaryTracer)
```

Delete the ingress hook after full OUT payload read:

```go
if dir == usbip.DirOut && ep == 1 && outBoundaryTracer != nil {
    outBoundaryTracer.TraceUSBIPOutIngress(...)
}
```

Delete the writer-accepted hook after successful `writeRet`:

```go
if dir == usbip.DirOut && ep == 1 && outBoundaryTracer != nil {
    outBoundaryTracer.TraceUSBIPOutWriterAccepted(...)
}
```

Do not otherwise edit `handleUrbStream`.

In particular, preserve exactly:

- reusable `outPayloadScratch`;
- inline OUT processing;
- async non-EP0 IN workers;
- `pending` / UNLINK behavior;
- `lastInResp`;
- endpoint interval timeout behavior;
- `writeMu`;
- batching writer;
- `writeRet`;
- EP0 flush semantics;
- 1 ms caller-selected batching behavior.

This cleanup is not a transport redesign.

### 5.9 Remove only the rumble-specific embedded logger helper

In `lib/viiper/embeddedlog.go`, remove:

- `x360RumbleDiagnosticMarkerOnce`;
- `buildEmbeddedRumbleTraceLogger`;
- `logXbox360RumbleDiagnosticBuildMarker`;
- event `X360RumbleDiagnosticBuild`;
- `Mode=forced-on`.

Keep everything else in the generic embedded log implementation.

The process-wide owned file handler, async writer, path override, daily rollover, invalid-handle logger, and flush remain.

---

## 6. Test cleanup

### 6.1 Delete tests that exist only for the removed diagnostic subsystem

Delete:

- `device/xbox360/rumble_trace_test.go`;
- `lib/viiper/xbox360_rumble_trace_test.go`;
- `internal/server/usb/usbip_out_trace_test.go`.

These are tests of code that should no longer exist.

Do not replace them with equivalent dormant-trace tests.

### 6.2 Remove the forced-build-marker test only

In `lib/viiper/embeddedlog_path_test.go`, remove:

```go
TestXbox360DiagnosticBuildMarkerIsEmittedOnce
```

Remove the now-unused `sync` import if that test was its final consumer.

Keep all tests for:

- configured diagnostic directory ownership;
- replacement before freeze;
- invalid/late path rejection;
- configured path resolution.

### 6.3 Preserve real rumble tests

Keep and run:

- `device/xbox360/xbox360_test.go::TestRumble`;
- all `device/xbox360/callback_lifecycle_test.go` tests;
- all canonical callback lifecycle tests involving Xbox360.

If cleanup requires changing those product tests, stop and inspect why. Removing diagnostic code should not require changing actual rumble semantics.

### 6.4 Preserve generic logging tests

Keep all tests that validate:

- file + callback fan-out;
- callback-only logging;
- safe discard when no handlers exist;
- async log writing;
- rollover;
- configured directory;
- loaded-module fallback path;
- lifecycle result independence from logging;
- invalid-handle logger isolation;
- lock release before final log/flush.

---

## 7. Public ABI contract

Expected C ABI change:

```text
NONE
```

PR #47 / #52 did not require new C exports. Their Go methods are internal implementation details.

The cleanup must preserve all current generated exports, including:

```text
NewUSBServer
CloseUSBServer
CreateUSBBus
RemoveUSBBus

GetUSBDeviceIdentity
AttachUSBDevice
DetachUSBDevice
AttachUSBDeviceEx
DetachUSBDeviceEx
GetUSBDeviceAttachmentState

CreateSteamDeckDevice
SetSteamDeckDeviceState
SetSteamDeckOutputCallback
RemoveSteamDeckDevice
RemoveSteamDeckDeviceEx

CreateXbox360Device
SetXbox360DeviceState
SetXbox360RumbleCallback
RemoveXbox360Device
RemoveXbox360DeviceEx

SetDiagnosticLogDirectory
```

Do not remove `SetDiagnosticLogDirectory`.

Do not add a replacement rumble diagnostic export.

After the Windows c-shared build:

- generated `libVIIPER.h` must retain `SetDiagnosticLogDirectory`;
- generated `libVIIPER.def` must retain the same public export surface;
- no trace-specific C export should appear because none is needed.

The DLL hash will naturally change after production code cleanup. Downstream CTW / Addon artifact provenance updates are separate consumer work and are not part of this source cleanup PR.

---

## 8. Explicitly preserved usbip-win2 support

Do not alter PR #53.

Current `internal/server/api/autoattach_windows.go` must continue supporting:

```text
usbip-win2 0.9.8.0
  -> 1120-byte plugin_hardware ABI

usbip-win2 0.9.8.1
  -> 1124-byte plugin_hardware ABI
  -> LocationHash field present / zero-initialized on input
```

Keep:

- installed `DisplayVersion` detection;
- exact ABI selection;
- unknown-version fail-closed behavior;
- zero-copy / `WskEvents=false`;
- positive imported-port ownership;
- common detach ABI;
- attachment timing diagnostics.

Do not use this cleanup as an opportunity to drop 0.9.8.0 support unless a separate product decision explicitly requests it.

---

## 9. Explicitly preserved transport behavior

No change is allowed to:

- zero-copy Windows localhost attach;
- WSK mode;
- `WriteBatchFlushInterval`;
- batching writer buffer size / threshold;
- async non-EP0 IN;
- `inputCh`;
- Xbox360 descriptor;
- X360 IN interval;
- cached IN response behavior;
- `pending` URBs;
- UNLINK;
- writer serialization;
- RET_SUBMIT status;
- RET_SUBMIT flush policy;
- server connection deadlines;
- callback ABI;
- callback locking;
- attachment ownership;
- transport ownership;
- teardown fail-close semantics.

Do not reintroduce historical sequential IN.

Do not add a transport mode switch.

Do not add a rumble retry or synthetic STOP.

---

## 10. Historical documents

Keep the old diagnostic work orders as historical records.

Do not delete or rewrite:

- `docs/work-order/XBOX360_RUMBLE_NATIVE_BOUNDARY_DIAGNOSTIC_WORK_ORDER_2026-09-24.md`;
- `docs/work-order/PR48_XBOX360_USBIP_SUBMIT_BOUNDARY_TRACE_ADDENDUM_2026-09-26.md`;
- `docs/work-order/XBOX360_USBIP_OUT_RECEIVE_BOUNDARY_DIAGNOSTIC_WORK_ORDER_2026-09-27.md`;
- related historical investigation documents.

They describe why the temporary instrumentation existed and remain useful Git/research evidence.

Current architecture/API documents already describe the generic low-volume `libVIIPER.log` contract rather than making the temporary rumble trace a product contract. Do not broaden this cleanup into a documentation rewrite unless implementation finds a current non-historical document that incorrectly claims forced rumble tracing is permanent.

---

## 11. No downstream application code changes in this PR

Do not modify SteamAddonforClaw or ClawTweaks in this VIIPER cleanup PR.

### CTW

CTW may continue to call:

```text
SetDiagnosticLogDirectory
```

and continue collecting generic `libVIIPER.log`.

Once the cleaned VIIPER artifact is adopted later, the high-frequency `X360Rumble*` / `X360USBIPOut*` records will simply no longer exist.

### SteamAddonforClaw

SteamAddon currently consumes the normal `VIIPERLogCallback` path and does not bind `SetDiagnosticLogDirectory` in its managed canonical native wrapper.

The cleanup therefore requires no Addon source change.

Consumer artifact/hash/provenance refresh is separate from this PR.

---

## 12. Required validation

Run from the latest implementation branch after cleanup.

### 12.1 Formatting and unit tests

```text
just fmt
just test
```

Equivalent direct command for the test gate:

```text
go test -count=1 -v ./...
```

### 12.2 Lint

```text
just lint
```

### 12.3 Windows libVIIPER build

On the normal Windows build environment:

```text
just build-libVIIPER Release
```

The c-shared build and postbuild/export verification must succeed.

### 12.4 Focused source checks

No runtime/test Go source should still contain any of the removed diagnostic symbols/events:

```text
RumbleTrace
rumbleTraceLoggerFactory
buildEmbeddedRumbleTraceLogger
x360RumbleDiagnosticMarkerOnce
createDeviceLockedPublicWithIdentityHook
rumbleTraceFinalizer
rumbleTraceAbortAfterDrain
finishRumbleTracesAfterDrain
finishRumbleTraceAbortAfterDrain
usbipOutBoundaryTracer
TraceUSBIPOutIngress
TraceUSBIPOutWriterAccepted

X360RumbleDiagnosticBuild
X360RumbleTraceStart
X360RumbleRaw
X360RumbleParsed
X360RumbleCallbackDispatch
X360RumbleTraceEnd
X360RumbleTraceAbort
X360USBIPOutIngress
X360USBIPOutWriterAccepted
```

Historical Markdown documents are exempt from this grep check.

### 12.5 Required retained-symbol checks

Verify the implementation still contains and builds:

```text
SetDiagnosticLogDirectory
buildEmbeddedLogger
openRealEmbeddedLogFileHandler
flushEmbeddedLogBestEffort
VIIPERLogCallback
BeginDeviceDrain
ForgetDeviceTransport
```

Verify the generated C ABI still exports `SetDiagnosticLogDirectory`.

---

## 13. Review checklist

During review, focus on real regressions rather than theoretical races.

Block the PR if it:

- changes rumble callback parsing or motor values;
- stops zero/zero from reaching the callback;
- invokes application callbacks while holding `callbackMu`;
- removes a generic transport drain;
- changes detach/fail-close ownership;
- changes usbip-win2 0.9.8.1 ABI support;
- changes zero-copy;
- changes async IN;
- changes batching/flush behavior;
- removes `SetDiagnosticLogDirectory`;
- breaks generic `libVIIPER.log`;
- changes the C ABI unexpectedly;
- leaves trace-specific lifecycle state behind after deleting trace emission.

Do not block for a hypothetical timing interleaving that does not represent a realistic supported lifecycle.

Do not request a new trace abstraction, feature flag, observer interface, or manager merely to preserve a diagnostic capability that Git history already preserves.

---

## 14. Acceptance criteria

The cleanup is complete when all of the following are true:

1. Xbox360 rumble packet tracing is completely absent from runtime code.
2. USB/IP EP1 OUT boundary tracing added by PR #52 is completely absent from runtime code.
3. Trace session identity / sequence / start/end/abort machinery is removed.
4. Trace-only lifecycle plumbing is removed from create/remove/bus/server paths.
5. Generic transport drains still run at their existing real teardown boundaries.
6. Xbox360 rumble callback semantics remain unchanged, including terminal zero/zero.
7. Existing real rumble and callback-lifecycle tests pass unchanged.
8. Generic `libVIIPER.log` remains functional.
9. `SetDiagnosticLogDirectory` remains functional and exported.
10. `VIIPERLogCallback` remains unchanged.
11. usbip-win2 0.9.8.0 / 0.9.8.1 native ABI support remains unchanged.
12. zero-copy, 1 ms caller configuration, async IN, UNLINK, writer locking, and RET_SUBMIT behavior remain unchanged.
13. Windows `libVIIPER` builds successfully.
14. Generated public ABI/export surface has no unintended change.
15. No new abstraction or dormant diagnostic mode is introduced.

The intended final architecture is deliberately simple:

```text
Xbox360 OUT
  -> parse
  -> callback

USB/IP
  -> normal OUT processing
  -> normal RET_SUBMIT

libVIIPER logging
  -> low-volume lifecycle / ownership / failure diagnostics only
```

That is the product state this cleanup should restore.
