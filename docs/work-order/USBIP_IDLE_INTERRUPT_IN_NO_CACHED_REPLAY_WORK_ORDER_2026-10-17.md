# Work Order — USB/IP Interrupt-IN: Remove Idle Cached-Report Replay (CTW Sleep Fix)

**Date:** 2026-10-17  
**Repository / branch:** `onehoon/VIIPER` / `main`  
**Verified source baseline:** `28a77c122856eba3b166a123b556482ea29decab` (PR #64 squash-merged, after PRs #61–#63)  
**Implementer:** Local Codex  
**Expected PR size:** One focused VIIPER PR; make a single coherent code/test change.  
**Consumer adoption:** **Not in this PR.** The standalone Full1902 Addon and the separate CTW-dev application are both consumers of the fork; neither application integrates the other.  
**Hardware testing:** The user performs real-device validation **after merge**. It is not a local Codex requirement, required CI gate, or PR blocker.

## 1. Decision

Implement the **already hardware-proven CTW idle-sleep fix** in the latest fork USB/IP server:

- An interrupt-IN request that has **no new device-produced report** when its poll/wait deadline expires **must stay pending**, rather than complete with the previous report held by the USB/IP connection.
- Remove the common interrupt-IN replay cache (`lastInResp`), its mutex (`respMu`), and the two cache maintenance helpers introduced/refined in PR #64.
- Preserve all report production rules **inside the three supported device implementations**: Xbox360, Steam Deck, and DualSense Edge (the Edge and ordinary DualSense share `device/dualsense`).
- Do not introduce WSGM's configuration system (`IdleMode`, `NaksWhenIdle`, `IdleKeepaliveInterval`, hardware pacing) or a substitute replay state machine. The common no-data semantics are enough for this change.

**Critical distinction:** The new policy says **no USB/IP server-generated duplicate on timeout**. It does **not** suppress a report returned by the device (even if its *bytes* match the last report). A consumer's new `SetXbox360DeviceState` call is a genuine new state *submission*, even if the supplied value is unchanged. No native setter deduplication is in this VIIPER work order.

This scope is intentionally restricted to the **three controller presentations used by the Addon and the Xbox360 presentation used by CTW-dev**. The USB/IP transport implementation is shared, so its semantics change globally, but do not broaden the PR to redesign unconsumed controller families or invent policies for hypothetical clients.

## 2. Required reference review (read live HEAD before coding)

### Fork

- `FORK_ARCHITECTURE.md`; `docs/libviiper/fork-api.md`.
- `docs/work-order/PR_A_DEVICE_INPUT_REPORT_BUFFER_ENCODING_WORK_ORDER_2026-10-17.md` and PR #63.
- `docs/work-order/PR_B_USBIP_INTERRUPT_IN_BUFFER_REUSE_WORK_ORDER_2026-10-17.md` and merged PR #64. **Its cached replay/storage requirements are superseded *only* where the present work order deliberately removes the cache.** Retain its caller-owned buffers and report encoders.
- `docs/work-order/USBIP_PERSISTENT_INTERRUPT_IN_ENDPOINT_WORKERS_WORK_ORDER_2026-10-17.md` and PR #62.
- `docs/work-order/USBIP_RET_SUBMIT_DIRECT_ENCODING_SINGLE_WRITE_OPTIMIZATION_WORK_ORDER_2026-10-16.md` and PR #61.
- `docs/work-order/USBIP_NO_DATA_IN_URB_CPU_SPIN_BACKPORT_WORK_ORDER_2026-10-01.md` for the separate immediate-nil/no-data anti-spin contract.
- `internal/server/usb/server.go`, `usb/device.go` and the relevant `internal/server/usb/*_test.go` tests.
- `device/xbox360/device.go`, `device/steamdeck/device.go`, `device/dualsense/device.go` and their existing encoder tests.

### Independently maintained consumers (compatibility references only)

- Addon: `onehoon/SteamAddonforClaw/docs/Full 1902 Implementation/README.md`; `FULL_1902_IMPLEMENTATION_ARCHITECTURE.md`, `docs/VIIPER_INTEGRATION.md`, `docs/VIIPER_IMPLEMENTATION_RULES.md`.
- Addon: `src/SteamInputAddonforClaw/VirtualOutput/Viiper/CanonicalXbox360InputPublisher.cs` (4 ms / 250 Hz active, 8 ms / 125 Hz idle, and an unchanged-state refresh at least every 250 ms while idle). Also review the Steam Deck and DualSense Edge typed publishers, the one canonical VIIPER Runtime owner and `MsiClawAddonPresentation`.
- CTW: `onehoon/ClawTweaks-Dev` branch `release/v0.3.98.0`, `Doku/VIIPER_IdleInput_Repeat_Report_2026-10-15.md`, `XboxGamingBarHelper/Native/VIIPER/ctw-no-idle-in-repeat.patch`, `XboxGamingBarHelper/Native/VIIPER/viiper-artifact.json`, and `XboxGamingBarHelper/Labs/ClawButtonMonitor.cs` `SubmitToBackends`.
- WSGM comparison, **not a dependency or a transplant target:** `KillerPixelCrew/VIIPER` branch `wsgm` (device-specific NAK vs continuous gyro/sensor reporting).

Do not mistake CTW's independently pinned/test-built DLL for the current fork baseline. CTW validated the fix against an older fork revision (`8902b53` + local patch), whereas this PR applies **the equivalent behavior to main after PR #64**, which has different buffer/cache helpers and persistent/fallback workers.

## 3. Verified bug and scope-specific semantics

### 3.1 CTW measurement and proven fix

CTW measured its Xbox360 virtual pad on Windows 11 / MSI Claw 8 AI+ / usbip-win2 0.9.8.1. Before its local patch: Windows `GetLastInputInfo` reset **69–108 times per five seconds** and XInput packet numbers increased approximately **95/s** with no state change. After removing **server-side idle replay**, measured idle resets were **0 over 30 seconds** and packet number stayed constant; buttons, sticks, rumble and automatic sleep were subsequently verified functional. These are **CTW hardware results**, not measurements from the Addon or the new fork revision.

CTW separately deduplicated repeated `SetXbox360DeviceState` calls in `SubmitToBackends`. This matters: its **consumer** sends only changed states, so a NAK-idle USB/IP path can genuinely be silent indefinitely while idle.

### 3.2 The current fork's exact code

`internal/server/usb/server.go` at current main:

- `runIN(job, scratch)` serves both PR #62's persistent per-endpoint worker and its independent overlapping fallback goroutines.
- With `job.interval > 0`, each attempt uses `context.WithTimeout(job.ctx, job.interval)` and calls either `usb.InterruptInBufferDevice.HandleInterruptIN` (20/64-byte buffered reports) or legacy `s.processSubmit`.
- If the device returns non-nil `respData`, it calls `storeINResponse(&respMu, lastInResp, job.ep, respData)` and later `writeRet`.
- If the attempt deadline expires with nil `respData`, it calls `copyCachedINResponse` and completes with the last report if one exists. That is the proven source of synthetic Xbox360 idle completions.
- If the device returns nil **without** an expired attempt deadline, the existing code parks on `<-job.ctx.Done()` to prevent a USB/IP no-data request spin.
- `writeRet` uses `writeMu` and a per-stream reusable `retOut`; it encodes a 48-byte `RET_SUBMIT` header plus payload, then one synchronous `writer.Write`. `CMD_UNLINK` removes/cancels the job from `pending` and sends `RET_UNLINK/-ECONNRESET` where appropriate.
- Request-specific `job.ctx` is canceled on UNLINK, managed device drain, stream teardown/EOF, server Close, and parent device removal. `workerWG.Wait()` joins persistent and fallback workers before closing the batching writer.

**Keep the latter contracts entirely unchanged.**

### 3.3 Three-device behavior *after* the cache is removed

| Presented controller | Input IN endpoint | When device has no newly submitted state | Required behavior in this PR |
| --- | --- | --- | --- |
| Xbox360 | EP1 (`0x81`), 20 bytes | `handleIN` blocks on its one-slot `inputCh` until a `SetXbox360DeviceState` call or context expiry | **No `RET_SUBMIT` on timeout.** Leave the URB pending for another attempt. A new submitted report must complete promptly. |
| Steam Deck | controller EP3 (`0x83`), 64 bytes | `handleIN` returns a *new latest-state report* at its existing **4 ms software wait budget** | Continue completing reports. Preserve report bytes, frame-on-`UpdateInputState` rule, IMU fields and gyro continuity. |
| DualSense / DualSense Edge | EP4 (`0x84`), 64 bytes | `handleIN` returns a *new latest-state report* at endpoint timeout | Continue completing reports. Preserve sequence increment for each report, sensor timestamp, metadata, Edge mapping and EP0 behavior. |

**Steam Deck's other IN endpoints:** its keyboard EP1 (8-byte zero report) and mouse EP2 (4-byte zero report) currently return immediate, fresh reports. The shared cache deletion does **not** make them NAK; do not change their behavior or descriptors in this PR. WSGM's different placeholder-endpoint policy is separate work.

The actual program does not use all other VIIPER controller types, and this PR must not spend effort expanding their functional specifications. Keep the preexisting generic `usb.Device` fallback path and protocol sound, including legacy large reports; do not delete other device types.

### 3.4 Addon compatibility boundary

**Keep Addon unchanged in this PR.** Its current Xbox360 publisher still sends an unchanged neutral state approximately once every 250 ms when the Runtime is in the healthy 125-Hz desktop-idle regime, and still submits **once per 4 ms tick** in the active 250-Hz regime. With idle cached replay removed:

- USB/IP should report on **each native setter submission**, even if the submitted bytes equal the preceding value.
- Therefore **Addon XInput packet numbers may still increase around 4/s while idle**, or more in active mode. Neither the CTW 0/s nor Windows idle-timer 0-resets result is a guaranteed outcome for the Addon from this change alone.
- The Addon's 250-ms refresh is an **intentional reconnect/resume safety policy**: a new USB/IP imported stream has no per-stream cache and the logical device's input channel might have been consumed on an earlier import. Do **not** remove this refresh, lower its cadence, or change the Addon publisher in the VIIPER PR.
- A separate future Addon review may evaluate a genuinely changed-only state publisher **together with its legitimate reimport first-report guarantee**. Do not invent a native USB/IP reimport callback, another authority, or changed ABI as a prerequisite to the present fix.

**CTW compatibility boundary:** CTW's changed-state-only path must still allow the Xbox360 virtual pad to stay silent while neutral; there must not be another VIIPER-generated repeated `RET_SUBMIT`. CTW's legacy local patch is therefore no longer required **when it eventually adopts a DLL built from this completed VIIPER revision**. Do not touch CTW pins, packaged DLL, header, SHA, native build process or release version here.

## 4. Implementation (one server source file, focused test updates)

### 4.1 Remove the common cached replay and no-longer-needed helpers

File: `internal/server/usb/server.go`.

Remove:

```go
func storeINResponse(respMu *sync.Mutex, responses map[uint32][]byte, ep uint32, report []byte) { /* ... */ }
func copyCachedINResponse(respMu *sync.Mutex, responses map[uint32][]byte, ep uint32, scratch []byte) ([]byte, bool) { /* ... */ }
```

Also delete the local `var respMu sync.Mutex` and `lastInResp := map[uint32][]byte{}` inside `handleUrbStream`, plus **all only-for-cache uses**. Leave `writeMu`, `pendingMu`, their map/state, and the existing `sync` import as required by unaffected transport synchronization.

Change **only the fresh / expired decision in `runIN`** to:

```go
respData = /* existing optional buffered or legacy device call */
expired := respData == nil && errors.Is(attemptCtx.Err(), context.DeadlineExceeded)
attemptCancel()

if job.ctx.Err() != nil {
    return
}
if respData != nil {
    break // complete the device-produced report; no cache write
}
if expired {
    // No device data at this poll deadline: keep the same URB pending.
    // Only a later real device-produced report can complete it.
    continue
}
// Preserve the preexisting immediate-nil/no-data anti-spin behavior.
// No timeout occurred, so looping immediately would spin.
<-job.ctx.Done()
return
```

**This is illustrative.** Preserve the existing named variables and call order; do not duplicate the device call or create a new function, interface, policy flag or branch for Xbox360.

- Remove the cache-only `time.Sleep(time.Millisecond)` in the **expired** path. Each new timed attempt already waits for the device input or deadline. Keep unrelated timing and `time` uses throughout the server unchanged.
- **Do not collapse the `expired` branch and the non-expired immediate-nil branch.** The latter's parking is the real anti-spin protection for unsupported endpoints/immediately nil fake devices.
- If `job.interval == 0` and a device immediately returns nil, the existing non-expired path still waits until UNLINK/teardown. Do not create a busy loop.
- Do not unconditionally block forever after **any** nil result: Xbox360 needs the per-`bInterval` retry to catch a subsequent input after an attempt's timeout while the original URB stays pending.
- On expired timeout, do **not** issue `RET_SUBMIT` with zero data/status or synthesize a new neutral input report. That would complete the host URB and defeat NAK-idle.
- Preserve `job.ctx` checks after each attempt, and the current pending-map/one-time completion/later writer path so UNLINK wins consistently without late responses.

### 4.2 Preserve PRs #61–#64 transport and device improvements

Must remain untouched unless a demonstrable relevant defect is discovered:

- `usb.InterruptInBufferDevice` and `HandleInterruptIN` for the three devices; the private PR-A `writeReport` encoders and old allocation-owning `HandleTransfer` wrappers.
- PR #62 unbuffered per-endpoint worker, nonblocking dispatch to independent concurrent fallback goroutine, correct per-URB `context.WithCancel` and teardown/join.
- PR #64's **worker-local 64-byte scratch** and independently owned fallback scratch. The device report is consumed by `writeRet` synchronously before scratch reuse.
- `RET_SUBMIT` single header/body write, 48-byte header framing, correct sequence and actual payload length, full 20/64 bytes, accurate OUT completion (`ActualLength` = consumed request bytes, no OUT response body).
- Two live writer configurations: `WriteBatchFlushInterval == 0` (Addon) and `> 0` (CTW uses a 1 ms config). Pending no-data URBs must not prevent flush, EP0 requests, OUT reports, rumble, or `CMD_UNLINK` from being processed.
- Existing per-device control transfers, descriptors, endpoint intervals and report mappings; **Steam Deck 4 ms report wait budget** and **DualSense sequence/timestamp locking** stay unchanged.
- Native ABI, typed handles, 0.9.8.0/0.9.8.1 usbip-win2 support, import/detach/port ownership, managed drain, closed/unsafe fail-close transitions and native logging stay unchanged.

Do not introduce WSGM pacing, shared buffer pools, new per-endpoint ownership tables, caches, mutexes, epochs, state machines, per-consumer toggles, or idle timers. **Removing** `lastInResp` also removes its allocations and synchronization; this should be a net deletion in production code.

## 5. Tests — revise the expected behavior, do not only delete tests

### 5.1 Replace cache-specific unit tests

`internal/server/usb/in_response_cache_test.go` currently tests three helpers which will no longer exist:

- `TestCachedINResponseSnapshotSurvivesOverwriteBeforeConsumption`
- `TestCachedINResponseLargerThanScratchIsCopiedWithoutTruncation`
- `TestStoreINResponseReusesCacheCapacity`

**Delete this entire test file** along with the removed helpers. Its correctness expectations are *obsolete* after cache deletion. The USB/IP transport tests below must establish the replacement semantics at the real request/response boundary instead; do not maintain a duplicate no-op helper just to keep old tests green.

### 5.2 Replace the transport tests which *require* replay

`internal/server/usb/transport_async_in_test.go` currently has these tests/fakes that **must be updated**, not skipped:

1. `TestAsyncINReportCacheIsIndependentPerEndpoint` / `endpointCacheINDevice`:
   - Keep real two-endpoint coverage. Deliver an initial distinct report from both endpoints, then queue fresh requests for each with **no new input**.
   - Verify **no cached completion** on either endpoint, even though each endpoint previously sent a report. UNLINK both requests (or submit independent fresh data to each after the no-replay assertion) and verify correct sequence, no late `RET_SUBMIT`, and no cross-endpoint confusion.
   - Rename test and device fixture to convey **no idle replay**, not cache independence. Channel synchronization for fresh state and cancellation is preferable to long sleeps.
2. `TestLegacyCustomDeviceLargeINPayloadAndCachedReplay` / `largeCachedReplayINDevice`:
   - Preserve the important generic `usb.Device` fallback test that sends a **100-byte report without implementing `InterruptInBufferDevice`**, proving the new 64-byte scratch optimization never truncates legacy data.
   - **Remove the expectation that a second request replays those 100 bytes.** Instead, confirm the second request remains pending without new data and can be UNLINKed or completed by a second **explicitly newly produced** 100-byte report.
   - Rename to `TestLegacyCustomDeviceLargeINPayloadWithoutIdleReplay` or similar.
3. `TestXbox360AndDualSenseEdgeUSBIPINRegression`:
   - Preserve initial neutral and changed-report assertions for both types.
   - Currently `read(3)` expects a report without an update **for both devices**. That is no longer correct for Xbox360; factor the test's third step by device:
     * Xbox360: after consuming its submitted report, issue another EP1 IN URB, assert no reply across at least one polling deadline, and then (a) call `UpdateInputState` with a deliberately equal state to confirm a *new native submission* still completes the URB, **and/or** (b) issue UNLINK and assert `RET_UNLINK/-ECONNRESET` and no late `RET_SUBMIT`. Cover both behaviors across tests.
     * DualSense Edge: unchanged new-report-on-timeout behavior remains; the third read must still return a 64-byte report with correct sensor fields and advancing sequence/timestamp semantics.
   - Do not attempt an exact XInput packet number or `GetLastInputInfo` assertion in the pure-Go USB/IP unit tests: these are Windows driver/kernel observations not reproduced by this fake TCP host harness.
4. Add a focused **Xbox360 NAK-idle wire test**, preferably reusing the existing imported-session helper:
   - Fresh neutral -> one `RET_SUBMIT`.
   - No new setter call -> a subsequent same-endpoint IN URB **stays pending** over multiple 4-ms attempt deadlines; no stale replay.
   - New button press -> one timely `RET_SUBMIT` with the right bit; unchanged value submitted again is still a *new* report (not suppressed by VIIPER).
   - Button release -> next newly submitted state produces an immediate release report.
   - Another unsubmitted idle -> no completion; UNLINK -> correct `RET_UNLINK` and **no late `RET_SUBMIT`.
   - Repeat the pending+UNLINK scenario for the **concurrent fallback** in addition to the active worker when naturally supported by existing harness.
5. Keep Steam Deck EP3 continuous behavior and IMU/frame fields covered by `TestSteamDeckAsyncINReturnsFreshStateForPendingRequest`. Add a cheap repeated-no-new-state USB/IP test **only if current coverage does not already prove** its fresh timeout report survives removal of the *transport* cache. Same principle for DualSense Edge EP4 and EP0 sequence tests.
6. Steam Deck keyboard/mouse endpoint behavior is intentionally unchanged: do **not** convert their existing 8/4-byte neutral reports to NAK or alter descriptors in this PR.

### 5.3 Protect teardown, protocol and buffer behavior

Run the existing tests (fix only tests whose old cache-replay contract was removed):

- `TestNoDataAsyncINWaitsForUnlink`: a device returning immediate nil must **park**, without busy-spinning or unsolicited empty `RET_SUBMIT`.
- `TestAsyncINUnlinkCancelsActiveRequest`; `TestOverlappingAsyncINRequestsUnlinkIndependently`: active and fallback URBs are independently cancellable, no late completions.
- `TestOverlappingBufferedINRequestsUseIndependentWorkerAndFallbackScratch` and `in_endpoint_worker_test.go`: each concurrent request retains independent 64-byte scratch.
- `TestAsyncINWorkersExitOnClientEOFWhileDeviceRemainsAttached`: close the stream with pending IN; worker(s) exit/join.
- `TestManagedDrainCancelsPendingSteamDeckINWithoutLateCompletion`, `TestManagedDeviceDrainReleasesParkedNoDataAsyncINWorker`, `TestManagedDeviceDrainWaitsForAsyncINWorker`: real drain/Close waits for workers and no late response.
- `transport_out_test.go`, `transport_import_test.go`, `transport_cleanup_test.go`, `transport_lifecycle_test.go` and batching-related tests: EP0, OUT/rumble, attachment, device loss/reimport, positive/zero flush behavior and teardown unchanged.
- All existing `device/{xbox360,steamdeck,dualsense}` encoder and concurrency tests: byte-for-byte report correctness, independent old API output, Steam Deck frame semantics and DualSense/Edge sequence safety.

**Pay attention to tests with read deadlines:** restore `conn.SetReadDeadline(time.Time{})` after an intentional no-response assertion so subsequent legitimate replies or UNLINK can be read. Do not depend on a fixed response order when worker and fallback requests are concurrent. Use bounded waits and existing channels, not scheduler-interleaving stunts.

### 5.4 Minimal example: no replay is *not* no future input

Conceptual test structure (adjust to the existing imported USB/IP harness and helper names):

```go
// 1. Seed and consume a report.
sendIN(100, 1)
expectReport(100, neutral20)

// 2. Do not submit another native state. The next URB stays pending.
sendIN(101, 1)
assertNoRETSubmitWithinBoundedPollWindow(conn) // reset read deadline afterward

// 3. A fresh setter call (even with unchanged bytes) must wake the pending URB.
xbox.UpdateInputState(neutralState)
expectReport(101, neutral20)

// 4. Following no-data URBs still wait and are cancellable.
sendIN(102, 1)
assertNoRETSubmitWithinBoundedPollWindow(conn)
sendUNLINK(200, 102)
expectRETUnlink(200, -104) // -ECONNRESET
assertNoLateRETSubmit(102)
```

The tests above establish **behavior**, not an exact 4ms scheduler performance target. Avoid assertions tied to pathological timing boundaries.

## 6. Validation by local Codex / CI

```bash
gofmt -w internal/server/usb/server.go internal/server/usb/transport_async_in_test.go
go test -count=1 ./internal/server/usb ./device/xbox360 ./device/steamdeck ./device/dualsense ./lib/viiper
go test -count=1 ./...
go test -race -count=1 ./internal/server/usb ./device/xbox360 ./device/steamdeck ./device/dualsense ./lib/viiper
go vet ./...
git diff --check
```

Add any existing repository-specific static checks/CI. Confirm any now-unused imports or obsolete cache-only fixtures are gone, and all CI jobs pass. If there are local toolchain limitations, report them accurately. No Windows device, `usbip-win2` driver, user sleep study, controller rumble, XInput packet number probe or on-handheld timing proof is required to merge this **code-only** PR.

## 7. Explicit exclusions / guardrails

- No change to `onehoon/SteamAddonforClaw` source, publisher cadence/idle 250-ms refresh, UI/overlay, HidHide, PID1901/PID1902, Game Bar/WING policy, Windows scheduled tasks, routing ownership, native pins or DLLs.
- No change to `onehoon/ClawTweaks-Dev` source, state dedup, its pinned local patched DLL, manifest or release packaging.
- No attempt to make the Addon's idle `GetLastInputInfo` resets **zero** while it still sends equal Xbox360 states periodically; distinguish measured CTW behavior from a possible future Addon consumer work order.
- No change to Xbox360 `UpdateInputState` channel semantics or native setter behavior, and do not change anything related to output rumble, feedback, Xbox buttons, Guide, or auxiliary mappings.
- No changes to Steam Deck frame/gyro/IMU, report interval, EP1/EP2 zero-report behavior, Steam input quirk handling or DualSense/Edge timestamp/sequence semantics.
- No write-batching or USB/IP timing optimization beyond removing the **cache-only** 1ms sleep. The controller can still receive a fresh report on every driver-defined request once the device produces one.
- No broad tests for unrelated controllers or unsupported environments, and no theoretical timing race defense that requires new states/locks/managers.
- Do not delete `usb.InterruptInBufferDevice`, PR #64 worker-owned scratch or `writeRet` in the name of cache cleanup.
- Do not remove the **immediate-nil request parking** behavior: this protects against a real no-data CPU spin, separately from timeout handling.

## 8. Acceptance checklist

- [ ] Common USB/IP idle replay is gone: no `lastInResp`, `respMu` cache mutex, `storeINResponse`, `copyCachedINResponse` or cache-only 1ms sleep.
- [ ] A timed-out IN attempt with no data leaves **the original URB pending**, and later new device data produces exactly one correct `RET_SUBMIT`.
- [ ] An immediate-nil result with no deadline expiry continues to park safely and does not spin/emit empty replies.
- [ ] Xbox360 idle after its last setter call is silent; a new setter call, **including identical content**, can complete its pending URB. Correct neutral, press, release, rumble and guide mapping are preserved.
- [ ] Steam Deck EP3 and DualSense Edge EP4 still emit their own existing timeout reports; 64-byte payloads, sensors, counters and timestamps preserve current behavior. Steam Deck EP1/EP2 are unchanged.
- [ ] Positive/zero batching configurations, 20/64-byte buffered reports, independent worker/fallback scratch and >64-byte legacy device reports remain correct.
- [ ] UNLINK active/fallback, EOF, device loss/reimport, managed drain/Close and native teardown remain safe; no late `RET_SUBMIT` or blocked shutdown.
- [ ] The obsolete replay-cache tests are **rewritten or removed and replaced** with no-replay protocol tests, never silently skipped. Other functional regressions must keep meaningful assertions.
- [ ] Fork ABI, generated header, native exports, typed handle ownership, exact attachment APIs and consumer binary pins unchanged.
- [ ] Full relevant Go unit tests, race tests, static checks and CI pass. Hardware validation is performed later by the user, not required for PR approval/merge.

## 9. Deliberately separate follow-ups

**Addon (later; different repository/PR):** Re-evaluate its current **250-ms idle identical-state refresh** and 250-Hz active submissions, alongside a concrete plan for first report after a legitimate USB/IP reimport/reconnect. Do not simply copy CTW's dedup without this safety proof. The user has authorized a future Addon adjustment if needed, but **not in this VIIPER PR**.

**CTW (later; different consumer update):** After VIIPER library changes pass CI, CTW may replace its locally patched `8902b53`-based DLL with the verified fork CI artifact at the chosen revision, preserving its own manifest/export ABI and ownership checks. No CTW consumer work is part of this implementation.

**Future idle efficiency (only if warranted):** In this PR an Xbox360 NAK-idle URB still performs a timed attempt every endpoint `bInterval`. WSGM avoids those idle timer wakeups with a data-driven input source and different worker design, but that is **not required to fix the reported Windows idle/sleep bug** and should not be silently bundled into this focused PR. Performance optimization can follow measured evidence.
