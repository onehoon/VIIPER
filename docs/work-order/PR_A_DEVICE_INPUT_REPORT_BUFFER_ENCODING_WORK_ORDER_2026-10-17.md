# Work Order — PR-A: Device Input Report Buffer Encoding (Xbox360 / Steam Deck / DualSense Edge)

**Date:** 2026-10-17  
**Target repository / branch:** `onehoon/VIIPER` / `main`  
**Reviewed code baseline:** `7c9dc6d85841cb9fa542ba0cb17f8d2050f2a3ca` (PR #62 squash merge)  
**Implementer:** Local Codex  
**Deliverable:** One narrow implementation PR, Go unit tests, and an accurate PR description.  
**This is PR-A only.** PR-B will separately connect caller-owned report buffers to the USB/IP transport and address per-endpoint cache ownership. A later, independent PR will assess per-device NAK-idle policy and Windows idle/sleep behavior.

## 1. Goal

Prepare the three canonical embedded virtual device types to encode a full **fixed-size USB interrupt-IN input report into a caller-supplied byte slice**, without allocating a fresh report *inside the encoder*:

- **Xbox360**: 20-byte input report.
- **Steam Deck**: 64-byte input report, including the existing 64-byte declared length and 60:64 force-sensor tail.
- **DualSense Edge**: 64-byte USB input report (share the implementation with the ordinary DualSense variant already using the same builder).

**The current allocation-returning public/existing helpers and the device `HandleTransfer` behavior must stay byte- and behavior-compatible.** PR-A only extracts a private, in-place encoder from each existing report builder. There is deliberately **no measurable USB/IP hot-path allocation saving from PR-A by itself**: the unchanged `HandleTransfer` path still allocates each returned report and the transport still copies it into `lastInResp`. Actual direct-buffer use and cache reuse belong to PR-B.

Use the smallest existing-package helper methods. Avoid new exported Go interfaces, generic adapters, buffer pools, lifecycle managers, feature flags, timer changes or abstraction layers in PR-A.

## 2. Required source reading and two-consumer constraints

Read the current branch and reconcile actual changes after the reviewed baseline before editing:

1. `FORK_ARCHITECTURE.md` and `docs/libviiper/fork-api.md` in this fork.
2. `docs/work-order/USBIP_PERSISTENT_INTERRUPT_IN_ENDPOINT_WORKERS_WORK_ORDER_2026-10-17.md` (PR #62's nonblocking idle-worker plus concurrent per-URB fallback contract).
3. `docs/work-order/USBIP_RET_SUBMIT_DIRECT_ENCODING_SINGLE_WRITE_OPTIMIZATION_WORK_ORDER_2026-10-16.md` (PR #61 header + body single write).
4. `device/xbox360/inputstate.go`, `device/xbox360/device.go`, `device/xbox360/xbox360_test.go`.
5. `device/steamdeck/inputstate.go`, `device/steamdeck/device.go`, `device/steamdeck/const.go`, `device/steamdeck/steamdeck_test.go`.
6. `device/dualsense/device.go`, `device/dualsense/state.go`, `device/dualsense/output_report_test.go`, `device/dualsense/const.go`.
7. `internal/server/usb/server.go` for **understanding only**; **do not modify** this file in PR-A.
8. `onehoon/SteamAddonforClaw/docs/Full 1902 Implementation/README.md` and the authoritative referenced Full1902 architecture. The Addon is a **standalone** Full1902 app: old references to integration with CTW are out of scope.
9. `onehoon/ClawTweaks-Dev` branch `release/v0.3.98.0` as an independently deployed consumer of the same VIIPER fork; it uses its own typed Xbox360, Steam Deck and DualSense Edge native sessions. Do not edit its code.
10. Optional implementation *technique* reference: WSGM `KillerPixelCrew/VIIPER`, branch `wsgm`, `device/steamdeck/inputstate.go` `writeReport`. **Do not copy its changed gyro/frame/cadence protocol.**

**Artifact-adoption policy:** The user will upgrade the final `libVIIPER.dll` in **both** Addon and CTW-dev only after all fork optimizations are finished. Do not change either application's DLL, SHA pin, lockfile, generated header or packaging in this PR. No real-device verification as an implementation or PR merge prerequisite; the user performs hardware tests after merge.

## 3. Verified baseline and important invariants

### 3.1 Xbox360

File `device/xbox360/inputstate.go`: `(*InputState).BuildReport()` currently:

- allocates `make([]byte, 20)`;
- sets `b[0]=0x00`, `b[1]=0x14`;
- writes lower 16 button bits at 2:4, triggers 4:6 and four little-endian signed stick values 6:14;
- copies the existing reserved slice into **`b[14:19]`** (do not silently alter the existing offset/count or other bytes);
- returns an independently owned 20-byte `[]byte`.

`HandleTransfer` EP1 still consumes `inputCh`, returns `st.BuildReport()` and uses the existing cancellation, tick and rumble contract. Nothing in that flow changes.

### 3.2 Steam Deck

File `device/steamdeck/inputstate.go`: `(*InputState).buildReport(frame uint32)` currently allocates `make([]byte, InputReportLen)` and serializes precisely **64 bytes**. Report header at 0:4 stays `01 00 <InputReportID> 40`; the frame is at 4:8. Existing buttons, hats, pads, sticks, triggers, motion, quaternion fields and byte 15 reserved passthrough retain the same positions and sign/endian handling.

**Critical fork-only contract:** bytes **60:64** are `LStickForce` / `RStickForce` (independent of L3/R3 stick-click buttons). Do **not** import the WSGM alternate payload-length behavior or drop/relabel the four bytes.

`device/steamdeck/device.go` increments its `Frame` **on `UpdateInputState`**, not once per returned input report. `currentInputReport`, controller `HandleTransfer` and EP0 `GET_REPORT` call the allocation-returning report builder today. Keep those calls and the exact frame policy. Preserve existing 4 ms `InterruptINWaitBudget`, raw gyro/accel and zero-quaternion policy; do not adopt WSGM's resampling or report-driven counter.

### 3.3 DualSense and DualSense Edge

File `device/dualsense/device.go`: `(*DualSense).buildUSBInputReport(s, m)` currently allocates `make([]byte, InputReportSize)` (64 bytes), writes USB input ID, axis centering, L2/R2, button/D-pad mapping, sensor values, 3x-microsecond-derived timestamp at 28:32, touch contacts and battery byte.

The encoder **advances `d.seqCounter` once per generated report**; `d.timestampBase` supplies the existing timestamp. `HandleTransfer` EP4 returns the report on new input or the current state on a timeout, while preserving cancellation. Both ordinary DualSense and Edge use this same `DualSense` implementation.

The report's currently zero/unused bytes must remain zero in new or reused destination buffers. Do not rewrite `SetMetaState`, state snapshots, rumble parsing/callbacks, feature/control responses, descriptors or device locks as part of this purely encoder-level PR.

### 3.4 PR #61/#62 transport compatibility

`internal/server/usb/server.go` currently has two real execution paths for simultaneous IN on the same endpoint: a reusable worker when ready and a per-URB fallback otherwise. It owns `pending`, `lastInResp`/ `respMu`, and serializes `writeRet` under `writeMu`. It also retains the timeout-based cached report replay and no-data park behavior. **None of these may change in PR-A.** In particular, do not give `HandleTransfer` a borrowed response buffer yet; its returned bytes must remain independently owned.

## 4. Exact implementation

Only these production files should change:

- `device/xbox360/inputstate.go`
- `device/steamdeck/inputstate.go`
- `device/dualsense/device.go`

Use simple package-private encoder methods and retain the old wrapper API unchanged.

### 4.1 Xbox360: factor out in-place encoder

Illustrative shape:

```go
const xboxInputReportLen = 20 // or reuse an existing established constant

// writeReport encodes exactly 20 bytes into dst; caller owns dst.
func (x *InputState) writeReport(dst []byte) {
    b := dst[:xboxInputReportLen]
    clear(b)
    b[0], b[1] = 0x00, 0x14
    // Move existing button, trigger, signed stick and reserved encoding
    // unchanged (including the current copy(b[14:19], x.Reserved[:])).
}

func (x *InputState) BuildReport() []byte {
    b := make([]byte, xboxInputReportLen)
    x.writeReport(b)
    return b
}
```

If an existing named report length is already present, prefer it to a duplicate. Do not refactor `MarshalBinary` or alter wire-state layout as a side quest.

### 4.2 Steam Deck: factor out in-place encoder

```go
func (s *InputState) writeReport(dst []byte, frame uint32) {
    b := dst[:InputReportLen]
    clear(b)
    // Move existing buildReport encoding byte-for-byte unchanged.
    // In particular b[3] = byte(InputReportLen), and writes continue
    // through both uint16 force fields in b[60:64].
}

func (s *InputState) buildReport(frame uint32) []byte {
    b := make([]byte, InputReportLen)
    s.writeReport(b, frame)
    return b
}
```

Use WSGM only as a model for the *extract/write* technique. Do **not** introduce its `payloadLen` argument; it is incompatible with this fork's declared 64-byte report contract. Keep comments explaining the final force-sensor bytes.

### 4.3 DualSense/Edge: factor out in-place encoder

```go
func (d *DualSense) writeUSBInputReport(dst []byte, s *InputState, m *MetaState) {
    b := dst[:InputReportSize]
    clear(b)
    // Move the existing buildUSBInputReport encoding unchanged.
    // Maintain one seqCounter advance and timestamp sampling
    // per report, including byte offsets and touch/battery fields.
}

func (d *DualSense) buildUSBInputReport(s *InputState, m *MetaState) []byte {
    b := make([]byte, InputReportSize)
    d.writeUSBInputReport(b, s, m)
    return b
}
```

Only change the encoder and its wrapper; leave the current `HandleTransfer` call sites exactly as they are. **Do not introduce a new mutex, a new clock seam, separate sequence authority or different timestamp policy merely for this refactor.** The existing concurrent-URB behavior is a separate transport/lifecycle concern.

### 4.4 Destination-buffer contract

- The caller provides a slice of **at least** the full report length. The writer operates on the first 20 or 64 bytes only, and must not mutate bytes after that range.
- A too-short buffer can follow Go's conventional slice-bounds panic, exactly as an internal, package-private helper with a documented precondition; do not introduce a generalized validation API, extra status return or allocation fallback.
- **Clear the exact report slice on every write**, even when all fields are overwritten, so a buffer previously used for active input never carries stale pressed buttons, sensor data, padding or unused flags after encoding a neutral state.
- In-place writers must not retain the destination slice, return it for later use, save pointers in device state, or store it globally. Lifetime/ownership remains with the caller.
- Avoid `sync.Pool`, heap-owned per-device caches or persistent scratch fields in this PR; those would need concurrency ownership rules before PR-B.
- Keep `BuildReport`, `buildReport` and `buildUSBInputReport` as **fresh independently owned allocations**. Existing callers and tests must never observe overwritten bytes after a later call.
- The private encoder methods should not invoke controller publishers, scheduling or native callbacks.

## 5. Automated verification

**Add focused encoder tests** (prefer small same-package `*_test.go` files for access to unexported helpers; do not expose the helpers just to satisfy external-package tests), in addition to preserving all existing device and USB/IP tests.

### 5.1 Common reuse/ownership cases for all three

For each device type:

1. Encode representative active/button+axis/sensor states and a neutral state into the **same prefilled `0xFF` destination buffer**, confirming no stale bits or padding survive. Verify all bytes using established source-specific golden vectors or fixed expected byte offsets; do not rely only on the legacy wrapper calling the same encoder, which can give a vacuous equality test.
2. Use a destination longer than 20/64 and verify a sentinel suffix is unchanged.
3. Call the original allocation-returning wrapper twice, retain the first returned slice, and verify it is **still unchanged** after the second call, including after changed input/counter where supported.
4. Exercise zero, negative, maximum/edge field encodings and correct fixed report length.

### 5.2 Xbox360 specifics

- Maintain full existing 20-byte golden vectors in `xbox360_test.go`; cover D-pad/buttons, triggers and positive/negative sticks.
- Confirm reserved byte behavior `b[14:19]` exactly matches the baseline (including the final report byte staying zero).
- Preserve EP1 `HandleTransfer` input-channel semantics and cancellation, and no change to output rumble.

### 5.3 Steam Deck specifics

- Assert header reports **64** at byte 3, neutral quaternion fields remain **zero**, and bytes 60:64 map to independent `LStickForce` and `RStickForce`.
- Verify full-bit button and pad/touch mapping, raw gyro/accel and signed/unsigned axis/trigger offsets, and neutral-after-active reuse.
- Existing tests include `TestControllerINCoalescesToLatestInputState`, `TestControllerINDeadlineReturnsLatestReportAndCancellationReturnsNoData`, `TestControllerGETReportRemainsImmediateLatestSnapshot`, `TestMotionFieldWireOrderMatchesSteamConsumers`, `TestSteamDeckForceWireOffsets` and `TestSteamDeckFullReportContract`: all must remain meaningful and pass.
- Frame must continue to increase **once per input-state update**, not once per encoder call, and `GET_REPORT` must remain compatible.

### 5.4 DualSense Edge (and normal DualSense) specifics

- Preserve `TestDualSenseEdgeUSBInputReportContract` and add targeted coverage for the shared in-place writer.
- Check report ID, exact 64-byte length, neutral axis `128`, D-pad diagonal encoding, Edge L4/R4 and PS/Cross mappings, touch inactive flags/coordinates, signed gyro/accel, and battery byte.
- Ensure sequence byte still advances **exactly once per generated report**, including across consecutive wrapper calls; compare with a fixed expected sequence rather than calling writer and wrapper successively on the same device and expecting identical sequence numbers.
- Timestamp bytes 28:32 reflect the existing live elapsed-time formula. Time-dependent bytes should be checked by a robust monotonic/range property or masked when comparing two separately timed encoder calls. **Do not add a production clock abstraction just to freeze a test timestamp.**
- Confirm that a reused buffer is cleared at zero/unused fields, and that normal DualSense is unaffected.

### 5.5 Additional regressions

Run relevant existing USB/IP transport tests to prove no accidental change to `HandleTransfer` report length/contents, concurrent IN/fallback behavior, EP0/OUT, `RET_SUBMIT`, `CMD_UNLINK`, drain/close and batching. PR #61 single-write and PR #62 worker contracts must remain intact. Existing rumble/callback tests for Xbox360 and DualSense Edge must pass.

If an encoder test reveals a pre-existing unrelated bug, record it rather than expanding PR-A into a general report semantics redesign. Do not add timing-sensitive race stress for contrived scheduler interleavings.

## 6. Scope and quality gates

### In scope

- Three private device-input report writers; existing allocation-returning builders become thin wrappers.
- Focused device-level tests for byte-for-byte parity, buffer reuse, clearing, ownership and existing report invariants.
- No behavior change in running consumer configurations.

### Explicitly out of scope

- `usb.Device`, `HandleTransfer` signature, `InterruptInBufferDevice` or any optional fast-path interface (reserved for PR-B).
- `internal/server/usb/server.go`, `lastInResp` cache changes, in-flight URB buffer ownership, transport worker/timeout/context scheduling, `RET_SUBMIT`, batching or idle polling.
- WSGM's `InterruptInSource` / `InputSignal`, hardware pacing, gyro resampler, Steam Deck packet-number-on-report policy and payload-length differences.
- Windows idle/sleep/NAK policy changes. **After PR-A and PR-B**, review the CTW developer's measured Xbox360 idle/sleep report and WSGM's per-device `NaksWhenIdle` behavior as a **separate PR**. Do not delete the common `lastInResp` cache while PR-B still depends on it.
- Go runtime tuning, new allocation benchmarks/analytics framework, new state or lock layers solely to defend against theoretical races.
- Canonical C ABI, generated headers, `libVIIPER.dll` adoption, typed native server/device ownership, `AttachUSBDevice` / `DetachUSBDevice`, HidHide, PID1901/PID1902 restoration, routing and CTW integration.

### Build/tests

```bash
gofmt -w device/xbox360/inputstate.go device/steamdeck/inputstate.go device/dualsense/device.go
go test -count=1 ./device/xbox360 ./device/steamdeck ./device/dualsense
go test -count=1 ./internal/server/usb ./usbip ./lib/viiper
go test -count=1 ./...
go test -race -count=1 ./device/xbox360 ./device/steamdeck ./device/dualsense ./internal/server/usb
go vet ./...
git diff --check
```

Respect CI's existing canonical Windows release build/header/export checks. If the local environment lacks Windows native tooling, report that accurately; do not create a hardware validation gate or alter the artifact contract.

**PR review blockers:** demonstrably different report bytes/lengths or per-device counter/gyro/timestamp semantics, uninitialized/stale destination bytes, overwritten legacy response buffers, actual out-of-bounds crash from supported calls, broken device behavior or failures of relevant tests/builds.

**Not blockers:** unmeasured allocation benefit from PR-A alone, speculative race conditions, changes unrelated to encoder code that are merely stylistic, unavailable handheld hardware validation, and unsupported user-session configurations.

## 7. Acceptance checklist

- [ ] Xbox360 encoder writes the same 20 bytes in place and original `BuildReport()` still allocates and returns an independent report.
- [ ] Steam Deck encoder writes the exact same **64 bytes**, keeps the frame-on-state-update rule, full 64-byte declared header and stick-force tail.
- [ ] DualSense/Edge in-place encoder writes the same 64-byte USB report with current sequence/timestamp/touch/battery semantics; the wrapper still owns a fresh report.
- [ ] Every writer clears and touches **only** its prescribed output length, supporting safe reuse.
- [ ] Golden/invariant and reuse/ownership tests cover active → neutral → active transitions; all current device and USB/IP tests remain green.
- [ ] No change to `usb.Device` API, device `HandleTransfer`, USB/IP server/cache/polling/worker, C ABI, consumer repositories or packaged DLL.
- [ ] PR description states explicitly: **preparatory extraction only; actual report/cache allocation reduction follows in PR-B**.
- [ ] CI passes; physical tests are performed by the user after merge, not required from local Codex.

## 8. Follow-up plan (not part of this PR)

- **PR-B:** introduce the minimal optional, compatible in-place report interface where justified; connect worker-owned and overlapping-fallback-owned 20/64-byte buffers to transport; make the shared per-endpoint report cache safe to reuse under same-endpoint concurrent IN without exposing aliased mutable output. Verify cancellation, cached replay, drain/close and full byte equivalence.
- **NAK-idle PR after PR-B:** consider device-aware Xbox360 idle suppression using CTW's physical Windows idle/sleep evidence and WSGM's opt-in per-device approach. Preserve Steam Deck/DualSense streaming and Addon's already working sleep behavior. Its policy may remove the need for an Xbox360 cache and must not be smuggled into these allocation PRs.
- **Final DLL adoption:** separately update Addon and CTW-dev from **one final proven fork revision** only after the fork optimization sequence is complete.
