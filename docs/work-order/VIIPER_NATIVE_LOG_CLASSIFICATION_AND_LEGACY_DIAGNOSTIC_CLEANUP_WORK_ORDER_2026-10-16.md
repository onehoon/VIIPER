# Work Order — VIIPER Native Log Classification and Legacy Diagnostic Cleanup

> **Repository:** `onehoon/VIIPER`  
> **Baseline reviewed:** `main` at `17b9334645b4ede72de81a8d09d967189fb3fdc4` (2026-10-16)  
> **Implementer:** local Codex  
> **Delivery:** one small, focused PR  
> **Scope:** actual erroneous native log severity, one demonstrably redundant import message, and verification of retired Gordon D-pad diagnostics. **No logging redesign.**

## 1. Decision and guardrails

**Do not change the VIIPER default file logging level**: `lib/viiper/embeddedlog.go` deliberately uses `slog.LevelDebug`. The amount of logging observed is small; DEBUG records remain useful.

**Do not introduce** `SetDiagnosticFileLogLevel`, `slog.LevelVar`, an OFF/INFO/DEBUG preference, new public exports, configuration, per-consumer log controls, callback forwarding, new queues, caching, sampling, dedupe state, or other logging infrastructure. **Do not connect the Addon logging preference to VIIPER.** Do not modify the standalone Addon or CTW in this PR. Their copies of the VIIPER binary/header are updated separately through each consumer's existing adoption process, not by changing any consumer source here.

Keep these existing, intentionally implemented contracts unchanged:

- Native/canonical typed controller initialization, attachment/detachment, classified success/retryable/unknown/invalid return values, exact import-port ownership, close-failed and fail-close behavior, callback rooting, partial teardown, retry policy, and driver/USB/IP behavior.
- The independent `libVIIPER.log` file sink, `SetDiagnosticLogDirectory`, optional `VIIPERLogCallback`, default DEBUG verbosity, and original C ABI.
- Async bounded logging queue and best-effort close flush; one daily-retained file, local-midnight reset, and Windows file-path/append-handle fix from PR #57.
- The independent logging CLI; no global `slog.SetDefault` behavior change.
- Physical and virtual D-pad behavior, Steam Controller/Gordon reports, Steam Deck/Xbox360/DualSense Edge devices, and all real controller lifecycle diagnostic evidence.
- No CTW-specific product integration: the standalone Addon and CTW are merely independent downstream VIIPER consumers.

Review these sources before making changes:

- `docs/libviiper/fork-api.md` (notably “Close and partial failure,” “Classified attach/detach results,” and “Diagnostic logging”);
- `docs/work-order/XBOX360_RUMBLE_DIAGNOSTIC_TRACE_CLEANUP_WORK_ORDER_2026-09-28.md` (preserves the generic low-volume file sink and ABI);
- `docs/work-order/WINDOWS_LIBVIIPER_DAILY_ROLLOVER_TRUNCATE_FIX_WORK_ORDER_2026-10-03.md`;
- For the **standalone Addon's actual lifecycle contract**, `onehoon/SteamAddonforClaw/docs/Full 1902 Implementation/README.md` and its named authority documents; it is not necessary to modify those documents or implement CTW integration.

## 2. Field evidence and why this is not a bulk log-reduction PR

Actual `libVIIPER.log` captured on 2026-10-15 from Addon/Log/AV2M/1015-1: **246** records over approximately 4 hours 36 minutes, **170 INFO / 76 DEBUG / 0 WARN / 0 ERROR**. This is not excessive volume. The `attachment-timing` records accounted for 51 lines, and `EP0 control unhandled` for 32 DEBUG lines.

An attachment legitimately emits different information at `native-ioctl`, `fallback`, and `canonical` layers. **Those are complementary measurements, not duplicate writes.** Retain each layer and all timing fields. Do not downgrade or delete ordinary successful `attachment-timing`, `EP0`, IOCTL, URB, per-connection, or teardown records solely on quantity.

Two instances of the actual *unsafe* detach classification appeared as:

```text
level=INFO msg=attachment-timing operation=detach layer=canonical
result=unsafe-outcome-unknown attachmentStateAfter=outcome-unknown
serverStateAfter=close-failed
```

Those are serious **existing results**, not evidence that the log call itself caused the failure. Classify their *log severity* correctly, without changing native result, retry or state transitions.

Separate captures from 2026-10-11 contain:

```text
level=ERROR msg="Connection handler error"
error="read URB header: read tcp ...: use of closed network connection"
```

These occur around session shutdown/transport teardown and are a realistic normal-close classification gap in `isClientDisconnect`. Fix the categorization; do not silently swallow other transport failures.

## 3. Required code edits

### A. Correct canonical attachment/detachment diagnostic severity

**File:** `lib/viiper/viiper.go`.

`logCanonicalAttachmentTiming` currently logs **every** outcome with `logger.Info("attachment-timing", args...)`. The same file already contains `teardownResultLogLevel(result string)`, which applies the proper severity mapping to classified teardown:

| `result` | Canonical attachment/detachment severity |
| --- | --- |
| `success` | INFO (unchanged) |
| `retryable-failure` | WARN |
| `unsafe-outcome-unknown` | ERROR |
| `invalid` | DEBUG (consistent with existing teardown semantics) |

Reuse that existing mapping, optionally renaming it to the more accurate `canonicalResultLogLevel` and updating its existing teardown call site, rather than adding a second mapper or independently maintaining two severity policies.

Illustrative minimal change (after reusing/renaming the helper):

```go
logger.Log(
    context.Background(),
    canonicalResultLogLevel(result),
    "attachment-timing",
    args...,
)
```

Keep the existing helper's case-to-level mapping unchanged. Keep `args` unchanged, including before/after attachment and server state, backend/import-port, duration, lock wait, and backend-called evidence. Keep the logging invocation **after `lifecycleMu` is released**, including the deferred backend-log replay contract. Nothing about `deviceAttachResult`, `deviceDetachResult`, or their behavior may change.

**Severity scope is canonical only:** do **not** blindly promote the accompanying `native-ioctl` and `fallback` measurement lines to ERROR as well; that would produce multiple primary ERRORs for a single unsafe classified operation. The backend measurements retain their existing detail and levels, while the authoritative final canonical result becomes unambiguously ERROR/WARN. Preserve any specific real backend error record, which can contain the actual failure reason.

### B. Classify the expected closed-network-connection case correctly

**File:** `internal/server/usb/server.go`, `isClientDisconnect(err error)`.

The existing classifier accepts EOF, reset, broken pipe, and certain textual Windows resets, but it does not explicitly recognize `net.ErrClosed` carried by a closed TCP connection's `*net.OpError`. Add the narrow standard-library sentinel without expanding the string heuristic to arbitrary I/O errors.

Illustrative minimal edit:

```go
if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
    return true
}
```

Retain the existing `ECONNRESET`, `EPIPE`, and Windows reset detection, and the existing separate `managed` and `unmanaged` connection handling. Expected closure continues to produce `"Client disconnected"` at INFO; genuinely unexpected handler failures continue to produce `"Connection handler error"` at ERROR. **Do not** change `handleConn`, URB request/response processing, cancel/drain behavior, connection ownership, close order, or raw transport behavior.

Avoid categorizing all connection errors as disconnects just because the local session happens to be closing. A wrapped `net.ErrClosed` is an established, demonstrable contract and sufficient for this PR.

### C. Delete one true redundant successful import message

**File:** `internal/server/usb/server.go`.

In `handleConn`, the `usbip.OpReqImport` branch logs the context-free `s.logger.Info("OP_REQ_IMPORT")`; once the request's bus ID is decoded, `readImportBusID` logs `s.logger.Info("Import request", "busid", reqBus)`.

Remove the first bare `OP_REQ_IMPORT` INFO call. Keep the second, more useful `Import request` event and all validation and error diagnostics for incomplete/malformed imports. Keep `OP_REQ_DEVLIST` because it has no corresponding bus-ID request message. No protocol or import flow changes. No rate-limiting framework.

This is the **only clearly demonstrated normal-path duplicate to delete now**. The following are not duplicates and must remain:

- `USBIP server listening` versus `USB server started` (transport readiness versus canonical successful initialization);
- `Client connected` versus `Import request` (transport versus protocol);
- `Auto-attaching ... IOCTL` versus `Successfully attached ...` (attempt versus verified backend success);
- `attachment-timing` at `native-ioctl`, `fallback`, and `canonical` (distinct measurements and final owner state);
- `typed-device-remove teardown`, `RemoveUSBBus teardown`, and `CloseUSBServer teardown` (different lifecycle boundaries);
- `URB completion after disconnect` DEBUG versus the high-level connection result (different event/packet evidence).

If an additional **exact** duplicate is proven by code and traces, mention it in the PR description; do not expand this order into a broad log-volume cleanup.

### D. Gordon D-pad diagnostic archaeology: **verify absence before editing**

The Addon already removed its old managed `CanonicalViiperDiagnosticLog` callback in PR #804. That callback used to forward messages prefixed `VIIPER.DPad` from the earlier Gordon/classic Steam Controller diagnostic campaign.

**Current `onehoon/VIIPER/main` source review found no active `VIIPER.DPad` producer**, including the inspected `device/steamcontroller/device.go`, `handler.go`, `inputstate.go`, and `lib/viiper/steamcontroller.go`. The earlier native Gordon-specific debug PR #15 did not land.

Codex must perform a final repository-wide source search for `VIIPER.DPad`, diagnostic-only `ABIDecoded` / `ReportInvariant` stages, or equivalent temporary Gordon-specific tracing. If there are **no remaining runtime producers, diagnostic-only helper files, stale test fixtures, or imported symbols**, **do not modify anything for this item**. Record the negative finding in the PR description.

If a real orphan from that same investigation is found, remove **only** that orphan and tests exclusively for it, with an explicit justification. **Never delete functional Gordon support**, D-pad report encoding, relevant tests, generic callback ABI, `VIIPERLogCallback`, or generic embedded file logging. Do not delete references merely because the word "Gordon" or "DPad" occurs.

## 4. Tests and documentation

Existing focused test locations:

- `lib/viiper/attachment_timing_test.go`;
- `lib/viiper/teardown_diagnostics_test.go`;
- `lib/viiper/attachment_backend_logging_test.go`;
- `internal/server/usb/transport_lifecycle_test.go` (or a neighboring USB server test file);
- `internal/server/usb/transport_import_test.go`;
- `lib/viiper/embeddedlog_test.go`, `dailyrollover_test.go`, and `asynclog_test.go`.

Required deterministic regression coverage:

1. Successful canonical attach and detach continue to emit exactly **one** canonical timing record per operation, with unchanged fields and INFO severity.
2. Canonical retryable failure emits WARN, keeps the same result and attachment state, and does not retry the native backend merely because logging changed.
3. Canonical unsafe unknown emits ERROR with `attachmentStateAfter=outcome-unknown` and `serverStateAfter=close-failed`; unsafe/fail-close classification and retained token are unchanged.
4. Canonical invalid outcome emits DEBUG, no native mutation is attempted. Existing typed removal/teardown severity mappings remain unchanged.
5. Logging occurs only after the current serialized native lifecycle lock is released; preserve existing callback/lock regression coverage.
6. `isClientDisconnect` returns true for `net.ErrClosed` and a wrapped `*net.OpError` containing `net.ErrClosed`; existing EOF/reset/broken-pipe cases remain true, while a representative unexpected connection or protocol error remains false. Cover wrapped errors like `fmt.Errorf("read URB header: %w", opErr)`.
7. Valid import handling continues with exactly one bus-ID-bearing `Import request` diagnostic and correct device selection, without a duplicate bare `OP_REQ_IMPORT`. Malformed import requests still produce a diagnosable failure.
8. Baseline embedded logging remains DEBUG, with no preference/API change; file/callback independence and daily rollover are unchanged.

Adjust log-level assertions in the existing timing test handler (`slog.Record.Level`) rather than creating a second testing/logging framework. Test the *same* helpers used by production.

Update only the relevant paragraph of `docs/libviiper/fork-api.md` to clarify the canonical timing severity (INFO success, WARN retryable, ERROR unsafe, DEBUG invalid), while retaining the independent backend measurement logs and default DEBUG file behavior. Do not rewrite the logging architecture documentation.

Run appropriate repository checks, e.g.:

```bash
go test -count=1 ./lib/viiper ./internal/server/usb ./internal/server/api
go test -count=1 ./...
git diff --check
```

Respect the repository's Windows/cgo build requirements and established workflow. If a test is known to depend on unsupported CI infrastructure, report the limitation rather than redesigning production code.

**No real device/USB/IP/Steam/handheld validation is a required Codex task or PR merge blocker.** Physical validation is the user's role after merge.

## 5. Exclusions and final acceptance

Do not:

- add a file logger level setter, change default `embeddedLogLevel`, or synchronize preferences from another app;
- change any `NewUSBServer`, `SetDiagnosticLogDirectory`, `VIIPERLogCallback` or device ABI/export;
- edit the Addon/CTW repositories or their packaged binaries;
- modify native IOCTL attach/detach, USB/IP state, tokens, classifications, teardown or recovery flow;
- remove detailed DEBUG logs such as `EP0 control unhandled` / `URB completion after disconnect` / backend timing;
- demote unsafe outcomes or remove evidence of physical-device/transport failure;
- add rate limiters, caches, new synchronization, process-global logging state, or future-proof abstractions;
- pursue theoretical callback/queue races that have no realistic lifecycle impact.

**Definition of done:** One focused PR removes the demonstrably redundant import preamble, classifies real expected socket closure correctly, emits the final *canonical* unsafe/retryable attachment and detachment log at the correct severity, and verifies the old Gordon D-pad log producers are already absent (or narrowly removes a proven orphan). All existing lifecycle semantics, DEBUG richness, consumer compatibility, and deterministic tests remain intact.

**Review policy:** Only realistically reachable, materially harmful regressions are blocking. Do not require artificial timing-interleaving tests or physical-device testing as a merge precondition.
