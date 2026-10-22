# Work Order — Steam Deck EP3 Output Diagnostics with One Build-Time Switch

Date: 2026-10-22
Repository: onehoon/VIIPER
Target branch/baseline: `main` at `5ef921e99f41d76b18b6e6eddf4f0e2ee1a6b222` (PR #71 merged)
Target implementation: one small, reviewable diagnostics PR
Owner: local Codex implements and performs automated code tests. Real MSI Claw/Steam hardware verification belongs to the user **after merge** and is not a PR or CI blocker.

## 1. Feasibility decision (confirmed from current source)

**Yes. One constant is sufficient.** The report-production and USB/IP completion facts required for this diagnostic are already observable together from `internal/server/usb/server.go` without changing the Steam Deck device-clock implementation or adding a second diagnostic owner:

- `device/steamdeck/device.go`: `clockedControllerInputReport` independently claims the 4 ms slot and increments its report-owned `Frame`; `HandleInterruptIN` then returns the encoded controller report.
- `device/steamdeck/inputstate.go`: the returned 64-byte report includes Frame at bytes 4..7 (LE uint32), accel at 24..29 (LE int16 x3) and gyro at 30..35 (LE int16 x3).
- `device/steamdeck/const.go`: Steam Deck report ID is `0x09` and report size is 64.
- `internal/server/usb/server.go`: `runIN` sees the actual result of `HandleInterruptIN`, checks cancellation, competes with `CMD_UNLINK` through the existing `pendingMu` ownership boundary, and calls `writeRet` to serialize/flush `RET_SUBMIT`.
- `DeviceOwnsInterruptINWait(ep)` already identifies the Deck-owned controller endpoint (EP3). Reuse this existing interface to constrain the observer; no new device API, C ABI, runtime setting, framework, tracing service or inter-package dependency is required.
- `s.logger.Info` already routes ordinary USB server diagnostics through VIIPER's existing logger. Do not create a new log sink or a separate file.

A single compile-time constant in the USB server package should default to **false**:

```go
// internal/server/usb/steamdeck_output_diagnostics.go
// Change this ONE value to true for a diagnostic VIIPER build.
// Return it to false for the normal release build.
const enableSteamDeckOutputDiagnostics = false
```

When false, the observer is not constructed; it must perform no clocks, packet decoding, locks, memory allocation, per-report logging, goroutine creation or timer work. A cheap nil/constant-gated check is acceptable. Go's compile-time constant permits dead-code elimination of the enabling branch.

**Operational consequence:** This is a build-time switch, NOT a live UI toggle. Flipping it requires rebuilding `libVIIPER.dll` and then adopting the matching verified artifact/hash in SteamAddonforClaw through its existing dependency pinning workflow. Do **not** bypass its DLL hash/preflight gate or hot-swap the loaded library. The code can remain committed indefinitely with `false`; turning it on for a future targeted diagnostic build requires editing only this constant. Do not add an environment variable or a second runtime knob unless separately requested.

## 2. Goal and scope

Provide an evidence-based, low-perturbation summary of what VIIPER's Steam Deck controller EP3 actually **generated** and what VIIPER's USB/IP server successfully **wrote/flushed** as `RET_SUBMIT`. This bridges the existing Addon physical sensor Probe and Addon's ~250 Hz `SetSteamDeckDeviceState` publisher diagnostics.

Goals:
1. Observe real Steam Deck controller **EP3 interrupt-IN** reports, not `SetState` calls, non-Deck endpoints, or `GET_REPORT` EP0 replies.
2. Account separately for report construction versus USB/IP submission, so an UNLINK/cancelled request is not incorrectly counted as delivered.
3. Summarize the report-owned Frame stream, output cadence, gyro/accel payloads, and cancellations/write errors without logging individual 4 ms reports.
4. Keep the ordinary production build **off by default** and leave existing routing, latency, cleanup, and native ABI untouched.

Non-goals:
- Do not alter PR #70/#71's Frame, slot, phase, lock, timer, last-state sampling, or deadline semantics.
- Do not attempt to guarantee 250 successful Steam reads/s; polling, OS scheduling and USB/IP conditions are external.
- Do not introduce a separate synthetic gyro producer, per-report averaging, interpolation, filter, PnP recovery, owner, thread, scheduler, service, GUI or Addon Probe modification.
- Do not claim that a successful `RET_SUBMIT` socket write proves usbip-win2/SDL/Steam actually consumed the report.
- Do not make hardware execution mandatory for the Codex PR, automated CI, or merge.

## 3. Proposed surgical implementation

**Preferred files**
- NEW: `internal/server/usb/steamdeck_output_diagnostics.go` — the one constant, small bounded collector, report decoder, summary and support functions.
- NEW: `internal/server/usb/steamdeck_output_diagnostics_test.go` — pure, deterministic collector and parser tests.
- EDIT: `internal/server/usb/server.go` — narrowly scoped observer initialization and calls in `HandleUrbStream` / `runIN`, plus an optional final summary after workers drain.
- Edit no `device/steamdeck`, `lib/viiper`, `usbip`, Addon code, generated headers or DLL in this PR unless a concrete implementation constraint requires a very small change; document any such necessity in the PR.

### 3.1 One toggle and Steam Deck filtering

Inside the USB/IP stream handler, allocate a collector only when the constant is enabled **and** the device exposes `deviceOwnedInterruptINWait` with `DeviceOwnsInterruptINWait(3) == true`. At observation time further require:
- IN endpoint == `3`;
- returned data length == exactly `64`;
- byte 0 == `0x01`, byte 2 == `0x09` and byte 3 == `64`.

The Deck implementation currently uniquely provides this combination. Do not require a fixed VID/PID because the typed Steam Deck device may permit identity overrides. Never tag Xbox360, DualSense Edge, keyboard/mouse endpoints, EP0, or unrelated USB/IP traffic as Deck gyro reports.

Sketch (adapt to existing code structure; it is guidance, not a demand for more abstraction):

```go
// One collector per active USB/IP stream; nil in normal builds.
var deckDiag *steamDeckOutputDiagnostics
if enableSteamDeckOutputDiagnostics {
    if clockOwner, ok := dev.(deviceOwnedInterruptINWait); ok &&
        clockOwner.DeviceOwnsInterruptINWait(3) {
        deckDiag = newSteamDeckOutputDiagnostics(s.logger)
    }
}

// runIN: only after HandleInterruptIN has returned a valid EP3 report,
// and BEFORE the cancellation/pending ownership checks.
if deckDiag != nil && isDeckEP3Report(job.ep, respData) {
    deckDiag.observeGenerated(time.Now(), respData)
}

// If cancellation/UNLINK suppresses an already-generated response:
// record the suppression, do not record a successful RET_SUBMIT.

// Record write outcome only AFTER writeRet(...) has returned; its true
// success includes the existing buffered-writer flush behavior.
```

Avoid logging under `pendingMu`, `writeMu`, a device mutex, or a collector mutex. Do not change which party wins an existing IN-vs-UNLINK race.

### 3.2 Exact event definitions

| Field | Definition |
| --- | --- |
| `Generated` | Valid Deck EP3 64-byte report returned from `HandleInterruptIN` (record before an already-cancelled job is discarded). |
| `SubmitAttempted` | A generated report whose job won `pendingMu` ownership and is about to call `writeRet`. |
| `RetSubmitWriteSucceeded` | Existing `writeRet` returned nil after `writer.Write` and (where configured) `bw.Flush`. This is a host-stream write success, **not** proof of Steam consumption. |
| `RetSubmitWriteFailed` | Existing `writeRet` returned an error. Preserve existing error/disconnect logging. |
| `GeneratedButSuppressed` | A generated response that is not submitted because its original job context was cancelled or the UNLINK/teardown side already removed its `pending` entry. Count once per response. Do not claim all such events were specifically UNLINK: they may also be detach/disconnect. |
| `GeneratedFrameJump / Duplicate` | Relative to the previous **observed generated** report's uint32 Frame. Success stream Frame discontinuities are tracked separately; use uint32 modular delta for natural wrap. |
| `SuccessfulFrameJump / Duplicate` | Relative to previous successful `RET_SUBMIT` report; gaps can legitimately reflect cancelled generated reports and are not automatically a defect. |
| `GyroNonZeroCount` | Count valid EP3 reports whose signed gyro Pitch/Yaw/Roll raw int16 fields are not all zero. |
| `GyroSamePayloadAsPreviousCount` | Consecutive valid EP3 reports carry identical raw Pitch/Yaw/Roll values. **Do not** label this an unchanged physical sample: two distinct sensor reads may numerically match. |
| `AccelNonZeroCount` | Count valid reports whose accel raw int16 x3 are not all zero. |

For a generated report, parse uint32 LE `[4:8]`, int16 LE gyro `[30:32]`, `[32:34]`, `[34:36]` and accel `[24:30]`. Keep the meaningful first/last gyro triplet or bounded component min/max for comparison with Addon values. Aggregate integers only. **Do not emit raw 64-byte packets every 4 ms.** Avoid logging physical device paths, user identifiers, controller serials, game names or unneeded payload fields.

The report-owned `Frame` is incremented before the device hands data to the server. Consequently a generated-but-suppressed report may create a gap in the successful write stream. This is allowed by PR #71's contract; **do not roll back Frame**.

A Frame gap is **not** a direct measurement of skipped 4 ms slots: Frame advances on generated reports, not on missed slot deadlines. Do not invent an exact `SkippedSlotCount` by treating Frame jumps or wall-clock gaps as device-slot losses. If useful, record `LongWriteIntervalCount` above a documented diagnostic threshold as an approximate symptom, never a normative missed-slot metric.

### 3.3 Cadence measurement and reporting

- Emit one structured `Info` diagnostic under event name `SteamDeckOutputDiagnosticsSummary` approximately every **10 seconds of observed EP3 activity**, using the existing `s.logger` and hence the existing `libVIIPER.log` pipeline.
- Window fields: `WindowMs`, `Generated`, `GeneratedHz`, `SubmitAttempted`, `RetSubmitWriteSucceeded`, `RetSubmitWriteHz`, `RetSubmitWriteFailed`, `GeneratedButSuppressed`, `GeneratedFrameJumpCount`, `SuccessfulFrameJumpCount`, `DuplicateFrameCount`, `GyroNonZeroCount`, `GyroSamePayloadAsPreviousCount`, `AccelNonZeroCount`.
- Include successful write-completion interval `MeanMs` and `MaxMs`. If adding `P95Ms` is straightforward with a bounded bucket histogram or a small fixed window sample buffer, include it; otherwise prefer an accurate mean/max to an inaccurately calculated percentile. Do not add dependencies for percentile computation.
- Log output identifiers/keys clearly: name the time measurement `WriteCompletionInterval` to avoid claiming actual kernel/Steam-read timestamps.
- Construct a bounded summary outside any transport or collector lock. Do not create a new 4 ms logging task, background manager or dedicated timer.
- If a stream is closed before the next 10 s boundary, optionally flush its nonempty final partial-window summary **after its IN workers have drained**; preserve the existing defer ordering. Avoid creating a new teardown hazard just for logging.
- When no EP3 report arrived, do not fabricate a rate or a periodic heartbeat. The current device may simply not be polled.

Diagnostic ON path may use one small mutex for a **per-stream** collector because `runIN` can execute on the endpoint worker and fallbacks concurrently. Reuse the existing stream's lifetime; do not add global ownership state or synchronization to the Steam Deck device clock. Log the summary after releasing the diagnostics mutex.

### 3.4 Keep the transport's existing behavior

`runIN` currently:
1. Calls `HandleInterruptIN` (or the fallback).
2. Checks the original job context.
3. Claims/removes `pending[job.seq]` under `pendingMu`.
4. On success, invokes `writeRet`; the latter serializes writes with `writeMu`.

Preserve this exact ownership sequence and the current USB/IP backpressure/flush behavior. Place only observational callbacks before/after these existing operations. No extra retry, cancellation, barrier, lock order or report generation is permitted.

Avoid adding a pointer/list of all URBs merely to count exact `CMD_UNLINK` events by endpoint. `GeneratedButSuppressed` plus the existing USB/IP UNLINK logs and the completed/failed counters are sufficient for the stated purpose.

## 4. Test plan — fully automated/local

Use existing Go tests and deterministic fake timestamps/report buffers. No Claw, Steam client, usbip-win2 installation, Addon, or real hardware is needed for PR completion.

1. **Flag-off default:** assert the source default is `false` and the normal stream path creates no observer; avoid alloc/time/log work on disabled path. Source/benchmark evidence is sufficient; do not require impossible absolute performance guarantees.
2. **Filtering:** valid 64-byte Deck EP3 report accepted; non-EP3, other 64-byte packets, wrong header/report ID, truncated and oversized packets ignored without panic.
3. **Fields:** golden raw Frame, positive/negative Pitch/Yaw/Roll and accel vectors decode exactly with little endian, including negative int16.
4. **Report vs SetState:** diagnostic collector is called on an actual returned EP3 IN report; producer `SetState` and `GET_REPORT` alone cannot increment generated/submitted counters.
5. **Generated vs completed:** report generated and successfully written increments Generated, Attempted, Succeeded once; synthetic write failure increments Failed but not Succeeded.
6. **Cancellation:** after report generation but before pending completion ownership, a job cancelled or UNLINK-owned increments GeneratedButSuppressed; no Succeeded or Failed. Cancellation before generating a report increments none of the report counters. Do not require pathological instruction-level interleavings.
7. **Frame continuity:** 1→2→3, repeated, gap 4→7, and wrap `0xFFFFFFFF→0` behave correctly on both generated and successful streams. A cancelled generated Frame may create a legitimate successful stream gap.
8. **Independent gyro repetition:** identical gyro payload with different Frames counts as repeated payload, not duplicate Frame; changed gyro values update summary. All-zero gyro is not counted as nonzero, and accel is independent.
9. **Window math:** use injected/fake time for deterministic 10s boundary, rates, zero-sample interval fields and summary rollover; final partial summary does not duplicate a full-window emission. Keep clock injection local to the diagnostic helper; do not modify production device-clock code.
10. **Concurrency:** a small deterministic concurrent test of the collector if the implementation allows more than one `runIN` observer; no test-only timing-race complexity in product code.
11. **Regression:** existing `device/steamdeck` clock/Frame suite, `internal/server/usb` pending-URB/UNLINK tests, and Xbox360/DualSense Edge tests remain green.

Suggested commands:
```text
go test ./device/steamdeck ./internal/server/usb
go test ./...
```

If an existing Windows/c-shared CI job builds `libVIIPER`, ensure the default-off binary still builds. Do not require a hardware log attachment to merge this diagnostic PR.

## 5. Post-merge user-run verification (informational; NOT a Codex/PR blocker)

1. Build an explicitly diagnostic-enabled VIIPER by changing the **one constant** to `true`. Adopt the built DLL/header and all matching pins/hashes through SteamAddonforClaw's existing verified dependency workflow; the running Go DLL cannot be hot-swapped.
2. Use an actual Steam Deck presentation under Steam/BPM with gyro enabled. Compare the existing Addon `MotionReaderDiagnosticsSummary`, `MotionImuPublishDiagnosticsSummary`, and new `SteamDeckOutputDiagnosticsSummary` over corresponding windows.
3. Observe idle, steady rotation and faster hand motion. In steady polling, successful EP3 reports should generally approach ~250/s. Expected rates are observational, not a strict CI invariant.
4. Interpret differences: sensor unique-receive ≠ SetState published ≠ EP3 generated ≠ successful RET_SUBMIT writes ≠ Steam consumed. Check Frame continuity, intervals and signed payloads before attributing jitter to the device clock.
5. For actual angular integration accuracy, use a separate SDL/Steam consumer measurement if desired. Neither a successful socket write nor the synthetic clock test proves Steam integrated 0.998–1.001x on Claw hardware.
6. When finished, change the same constant back to `false` for normal shipping builds and repeat the verified DLL dependency adoption. Keep the diagnostic code in place.

## 6. Acceptance criteria / PR boundaries

- One clearly documented **single** `const` switch; **false by default**.
- Only Steam Deck EP3 observed. Existing 64-byte wire report and C ABI untouched.
- Honest and separately named generated / attempted / write-succeeded / suppressed / failed counters.
- Frame and IMU raw fields decoded from outgoing bytes, not reconstructed from sensor snapshots.
- Log at most about once per 10 seconds of EP3 activity, to existing `libVIIPER.log`; no per-report messages.
- OFF path does not acquire diagnostic locks, timestamps, allocate collectors, or emit diagnostic summaries.
- No changes to report timing, pending-map authority, Unlink behavior, output bytes, Windows routing, physical ownership, fail-close or teardown.
- Relevant automated tests pass. **Do not require real-device validation before merge**.

## 7. Source references

- [VIIPER PR #70 — report-owned Frame](https://github.com/onehoon/VIIPER/pull/70)
- [VIIPER PR #71 — device-owned 4 ms grid](https://github.com/onehoon/VIIPER/pull/71)
- [VIIPER Steam Deck device/clock](https://github.com/onehoon/VIIPER/blob/5ef921e99f41d76b18b6e6eddf4f0e2ee1a6b222/device/steamdeck/device.go)
- [VIIPER Steam Deck report encoding](https://github.com/onehoon/VIIPER/blob/5ef921e99f41d76b18b6e6eddf4f0e2ee1a6b222/device/steamdeck/inputstate.go)
- [VIIPER USB/IP runIN, pending, UNLINK and RET_SUBMIT](https://github.com/onehoon/VIIPER/blob/5ef921e99f41d76b18b6e6eddf4f0e2ee1a6b222/internal/server/usb/server.go)
- [HIDMaestro #72 author report, confirmed device-clock motivation](https://github.com/hifihedgehog/HIDMaestro/issues/72#issuecomment-6327520811)
- [Addon existing publisher/physical source](https://github.com/onehoon/SteamAddonforClaw/blob/4720a70e57692579a6b8eb3cf4ead093a479c5d4/src/SteamInputAddonforClaw/VirtualOutput/Viiper/CanonicalSteamDeckInputPublisher.cs)
- [Addon VIIPER pin and adoption policy](https://github.com/onehoon/SteamAddonforClaw/blob/4720a70e57692579a6b8eb3cf4ead093a479c5d4/docs/VIIPER_INTEGRATION.md)
