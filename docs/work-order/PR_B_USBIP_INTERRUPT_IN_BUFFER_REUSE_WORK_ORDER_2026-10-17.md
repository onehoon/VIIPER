# Work Order — PR-B: USB/IP Interrupt-IN Caller Buffer and Report Cache Reuse

**Date:** 2026-10-17  
**Target repository / branch:** `onehoon/VIIPER` / `main`  
**Reviewed baseline:** `f3b312c2868652923a6b5634edd014efe92a94a1` (PR #63 squash merge; PR-A in-place encoders)  
**Implementer:** Local Codex  
**Change size:** One focused transport/device Go PR, with regression tests.  
**Dependency:** PR #61 (single `RET_SUBMIT` write), PR #62 (persistent idle worker with concurrent fallback), PR #63 (PR-A report encoders) **already merged**.  
**User testing:** Physical-device verification belongs to the user **after merge** and is **not** an implementation requirement, CI gate, or review blocker.

## 1. Decision and scope

Connect the new PR-A in-place encoders for **Xbox360 (20 bytes), Steam Deck (64 bytes), and DualSense/DualSense Edge (64 bytes)** to the actual generic USB/IP non-EP0 interrupt-IN hot path without altering input semantics.

The minimal architecture is:

1. Add **one optional Go device interface** (not a change to the existing `usb.Device` interface or C ABI) that writes a non-EP0 interrupt-IN payload into a caller-owned slice and returns its valid prefix, or nil.
2. Have the three supported device implementations use the PR-A private encoders through this public-interface method while **sharing the existing wait/timeout/state-consumption logic** with their original `HandleTransfer`.
3. Give **each persistent endpoint worker** its own reusable 64-byte report scratch. Each PR #62 **concurrent fallback goroutine** has its own, independently owned 64-byte scratch. Do not share these scratch buffers across concurrent URBs.
4. For fresh input, update the existing per-endpoint `lastInResp` cache **in place under its existing mutex**, rather than allocating a new backing slice for each completed report.
5. For cache replay, copy the cached bytes **under the mutex into that request's scratch buffer**, then release the mutex before encoding/writing `RET_SUBMIT`. Never expose a live mutable cache slice to an unlocked sender.

**Expected effect:** remove the report-allocation hot path for supported controller IN reports after the first worker-buffer/cache allocations, and avoid per-fresh-report cache allocations. There remains at least one copy into `RET_SUBMIT`'s already-reused output buffer, plus original contexts/timers and occasional per-URB fallback goroutine costs. Do not claim an exact allocation count or CPU reduction without measurement.

This PR is **not** NAK-idle. Keep timeout-driven cached-report replay unchanged. A later, separate task will review CTW's measured Xbox360 idle/sleep issue and WSGM's device-specific NAK policy. Final DLL adoption in Addon/CTW-dev happens **only after all VIIPER optimization work is complete**.

## 2. Mandatory review sources and consumer policy

Read before changing code:

- Current `FORK_ARCHITECTURE.md` and `docs/libviiper/fork-api.md`.
- `docs/work-order/PR_A_DEVICE_INPUT_REPORT_BUFFER_ENCODING_WORK_ORDER_2026-10-17.md` and merged PR #63.
- `docs/work-order/USBIP_PERSISTENT_INTERRUPT_IN_ENDPOINT_WORKERS_WORK_ORDER_2026-10-17.md` and merged PR #62.
- `docs/work-order/USBIP_RET_SUBMIT_DIRECT_ENCODING_SINGLE_WRITE_OPTIMIZATION_WORK_ORDER_2026-10-16.md` and merged PR #61.
- `internal/server/usb/server.go`, `internal/server/usb/in_endpoint_worker_test.go`, `transport_async_in_test.go`, `transport_lifecycle_test.go`, `transport_out_test.go`, `interrupt_in_wait_budget_test.go`.
- `usb/device.go` and all three device files: `device/xbox360/device.go`, `device/steamdeck/device.go`, `device/dualsense/device.go`, with the PR-A writers and existing tests.
- `onehoon/SteamAddonforClaw/docs/Full 1902 Implementation/README.md` and its referenced authoritative architecture, in particular persistent one-server/three-logical-device/one-attached-presentation ownership.
- `onehoon/ClawTweaks-Dev` `release/v0.3.98.0` canonical typed Xbox360/Steam Deck/DualSense Edge sessions, as a **separate shared-fork consumer**, not an integration into the standalone Addon.

**Deployment restriction:** No Addon or CTW code/artifact, DLL, native export, canonical generated header, lockfile, SHA pin, packaging or setup changes. Keep both zero and positive `WriteBatchFlushInterval` configurations working (existing consumers and historical pins are not a justification to redesign batching).

Verify the live branch HEAD before implementation and reconcile any intervening changes after the pinned baseline.

## 3. Verified baseline and failure modes

### 3.1 Transport: PR #62 two execution paths

In `internal/server/usb/server.go`:

- `inJob` has `seq`, `ep`, `ctx`, `cancel` and `interval`.
- `startINEndpointWorker` creates **one lazily reused unbuffered receiver** per endpoint/imported stream; `tryDispatch` is nonblocking.
- When a worker is busy or not ready, the reader immediately launches the **legacy per-URB fallback goroutine**. Multiple `HandleTransfer` calls on the **same endpoint** can overlap.
- Both paths call the same `runIN`: `s.processSubmit`, separate attempt timeouts, endpoint replay cache, pending-map completion, `writeRet`.
- `writeRet` takes `writeMu`, encodes one 48-byte header into `retOut`, appends the payload, and performs one `writer.Write`. It **copies the payload synchronously before returning**.
- `streamDone` and `workerWG` release idle and busy workers on EOF/drain/close; `CMD_UNLINK` cancels pending URBs via `pendingMu`.

The current code stores fresh cache payloads with `lastInResp[job.ep] = append([]byte(nil), respData...)`; it reads `cached := lastInResp[job.ep]` under `respMu` and sends that slice **after releasing the lock**. That was safe when every update replaced the backing storage, but **would become an actual data race/corrupt report** if PR-B changed only the cache append to `append(lastInResp[ep][:0], respData...)`. PR-B MUST change the replay read path along with the write path.

**Do not remove `lastInResp` yet.** The later Xbox360 NAK-idle policy may affect one endpoint's need for it, but Steam Deck and other endpoints retain their current streaming/replay policy in this PR.

### 3.2 Xbox360 device

- `HandleTransfer` EP1 increments the existing `tick`, selects between `ctx.Done()` and its one-slot `inputCh`, and returns `InputState.BuildReport()` (20 bytes). Unknown IN endpoint returns nil.
- PR-A's private `(*InputState).writeReport(dst []byte)` emits the exact existing 20-byte report and clears its region.
- State updates currently use `UpdateInputState` with replacement/coalescing semantics; do not change how concurrent host requests consume a state, and do not deduplicate identical frames.

### 3.3 Steam Deck device

- EP3 controller `HandleTransfer` waits for `inputNotify` or deadline; a real cancellation returns nil; a deadline returns `currentInputReport()` (64 bytes).
- `currentInputReport` snapshots the latest state under `mtx`; `Frame` is incremented once on `UpdateInputState` and remains unchanged on reads. The controller's current wait budget is **4 ms** (separate from its descriptor interval).
- EP1 keyboard and EP2 mouse currently return **fresh copied** fixed zero reports (8 and 4 bytes respectively); unknown IN endpoints return nil. When using the optional buffer API, faithfully emit these **same lengths/bytes** into that URB's scratch without unnecessary allocation. Do not change their idle/completion policy.
- EP0 input `GET_REPORT` still uses its existing independently-owned allocated result and must **not** start borrowing worker scratch. PR-A's complete 64-byte Deck report, byte 3 = 64, unchanged quaternion policy and 60:64 stick-force fields all remain.

### 3.4 DualSense and DualSense Edge devices

- EP4 IN `HandleTransfer` reads a state from its one-slot `inputCh` or, on timeout, snapshots its latest input under `mtx` and returns a new 64-byte USB report; real cancel returns nil. Other IN endpoints return nil.
- Both ordinary DualSense and Edge use `(*DualSense).buildUSBInputReport` / PR-A `writeUSBInputReport`, which increment `seqCounter` once per report and derive the sensor timestamp from immutable-after-construction `timestampBase`.
- **Reachable concurrency detail:** PR #62 allows a persistent and a fallback URB on EP4, and EP0 `GET_REPORT` may overlap. Today `seqCounter++` inside `writeUSBInputReport` is outside `d.mtx`. When both execution paths call it concurrently, sequence updates can race, lose an increment or repeat a packet number. Fix this **locally** by protecting only the read/increment of `seqCounter` with the device's **existing `d.mtx`**. Do not hold it across blocking input waits, callback invocations, USB/IP I/O or calls that already hold it; inspect all caller sites to avoid reentrant locking. This is a realistic data-integrity issue, not a request for a new mutex/epoch/manager.
- The original `HandleTransfer` and EP0 `HandleControl` continue returning independently-owned allocated reports. Preserve touch/battery mapping, sensor field order, sequence counter frequency, timestamp units and the reported 64-byte size.

## 4. Required implementation

**Expected files:** `usb/device.go`; `internal/server/usb/server.go`; `device/xbox360/device.go`; `device/steamdeck/device.go`; `device/dualsense/device.go`; focused transport/device tests. Do not modify PR-A encoder implementations in `inputstate.go` except for a demonstrated necessary bug fix. Avoid new production packages or generalized allocation frameworks.

### 4.1 Optional device capability (one interface)

Keep `usb.Device` unchanged. Add a **single optional interface** in `usb/device.go`:

```go
// InterruptInBufferDevice is an optional allocation-saving path for
// non-EP0 IN transfers. dst is caller-owned scratch with enough capacity
// for the device's complete report; the returned slice must be a prefix
// of dst (or nil). No reference to dst may be retained.
// It obeys the same endpoint, input-wait, timeout, cancellation and
// state-consumption semantics as Device.HandleTransfer.
type InterruptInBufferDevice interface {
    HandleInterruptIN(ctx context.Context, ep uint32, dst []byte) []byte
}
```

Notes:

- Go packages other than the device's own package need an **exported method** to satisfy this interface. PR-A's existing lowercase `writeReport` / `writeUSBInputReport` stay private.
- No change to `usb.Device`, `HandleTransfer` signature, EP0 `ControlDevice` or any native C export. Existing third-party/legacy `usb.Device` implementations must still work untouched.
- Only use this optional path on non-EP0 `DirIn`; do not route OUT or EP0 through it.
- The built-in three device types return no more than 64 bytes, but this interface does not require a generalized buffer allocator or arbitrary max-size support in PR-B. The server should fall back safely to the existing `HandleTransfer` path for devices not implementing the optional interface.
- A supported device must **never return a borrowed slice other than the valid prefix of dst** (except nil). This is a method contract rather than a new defensive runtime framework.
- Returning a non-nil report means **fresh or device-defined timeout report exactly as before**; returning nil retains the existing cancellation/no-data meaning. Do not return zero-length non-nil slices to fake a completion.

### 4.2 Device adapters: preserve one canonical input handler

For each of the three device types, have `HandleTransfer` on non-EP0 IN and the new `HandleInterruptIN` method share one small internal handler rather than copying the same blocking `select` loop twice.

An illustrative shared pattern for one device (actual switches and state behavior must follow its existing code):

```go
func (d *Device) HandleInterruptIN(ctx context.Context, ep uint32, dst []byte) []byte {
    return d.handleIN(ctx, ep, dst)
}

// Legacy Device.HandleTransfer:
//   if dir == usbip.DirIn { return d.handleIN(ctx, ep, nil) }
//   // Existing OUT handling unchanged.

func (d *Device) handleIN(ctx context.Context, ep uint32, dst []byte) []byte {
    // Preserve the existing select/cancel/timeout and state consumption.
    // On a real report:
    //   if dst == nil { return state.buildReport(...) }
    //   state.writeReport(dst, ...)
    //   return dst[:fixedReportLen]
}
```

- `nil` dst is a strictly internal signal for the **legacy owned-allocation** path; a transport caller always supplies its own >=64-byte scratch.
- **Xbox360:** preserve `tick` increment location, the exact `inputCh` consume/cancel select, EP1-only output, and rumble callbacks. For a consumed state, use `st.writeReport(dst)` on the new path and `st.BuildReport()` on the old path.
- **Steam Deck:** preserve controller `inputNotify` coalescing, context-deadline vs cancellation distinctions, latest-state snapshots, EP1/EP2 fixed reports, EP3 input, EP0 GET_REPORT and output commands. Use `st.writeReport(dst, st.Frame)` on the new path. Never advance frame on read, force identity quaternion or suppress a repeated report.
- **DualSense/Edge:** preserve EP4 select semantics, state/meta snapshot timing and `seqCounter` increment per completed report. Use `d.writeUSBInputReport(dst, is, &ms)` for new-path reports; retain `d.buildUSBInputReport(...)` and independent return storage for the legacy path. Protect the existing seq increment with `d.mtx` as noted above, but do not serialize an entire IN wait or introduce extra state/counter authority. Do not modify output/rumble and touch/gyro conversions.

**Buffer lifetime:** Neither a device nor the USB/IP server may retain, publish or asynchronously mutate `dst` after the optional method returns. Existing separately allocated legacy return slices remain stable for their current callers.

### 4.3 Worker-local scratch and concurrent fallback scratch

The current `startINEndpointWorker` takes a `run func(inJob)` callback. The minimal change is to have it own its 64-byte scratch within the goroutine and pass that scratch to the common handler. Each fallback goroutine similarly owns its own 64-byte array:

```go
// Representative structure, not a required exact diff:
func startINEndpointWorker(
    streamDone <-chan struct{},
    wg *sync.WaitGroup,
    run func(inJob, []byte),
) *inEndpointWorker {
    worker := &inEndpointWorker{jobs: make(chan inJob)}
    wg.Add(1)
    go func() {
        defer wg.Done()
        var report [64]byte // lives for this endpoint worker only
        for {
            select {
            case job := <-worker.jobs:
                run(job, report[:])
            case <-streamDone:
                return
            }
        }
    }()
    return worker
}

// Reader fallback for an already occupied or not-yet-ready worker:
wg.Add(1)
go func(job inJob) {
    defer wg.Done()
    var report [64]byte // one independent in-flight URB
    runIN(job, report[:])
}(job)
```

- Preserve PR #62's **unbuffered, nonblocking** `tryDispatch` and the reader's immediate fallback on overlap. No endpoint FIFO or shared pooled scratch; no `N × interval` serial delay.
- Keep the existing job-scoped `context.WithCancel` / attempt `context.WithTimeout` lifecycle, cancellation checks, no-data parking and pending-map completion. Redesigning contexts/timers is outside this PR.
- In the common handler's IN call choose `bufferDev.HandleInterruptIN(attemptCtx, job.ep, scratch)` when the device implements the optional capability; otherwise use the unchanged `s.processSubmit(attemptCtx, dev, job.ep, usbip.DirIn, nil, nil)`. Ordinary custom/legacy devices must continue to work, including payload sizes greater than 64 bytes.
- The buffer must not be reused until the current `runIN`'s synchronous `writeRet` returns. `writeRet` currently copies payload into its per-stream `retOut` while holding `writeMu` and writes before returning. Preserve that contract, including positive batching intervals.
- Do not share fallback scratch with the persistent endpoint receiver, or between two simultaneously running fallbacks. Go escape analysis may place these arrays on the heap in some paths; do not claim all per-URB heap allocation is eliminated.

### 4.4 Cache capacity reuse without aliasing (critical)

Retain the existing **single cache map and its existing `respMu`**, per imported stream. Do not replace it with a second device-level report cache, new ownership manager, global pool, per-endpoint state machine or new lock hierarchy.

On **fresh non-nil** report completion:

```go
respMu.Lock()
lastInResp[job.ep] = append(lastInResp[job.ep][:0], respData...)
respMu.Unlock()
// respData is still the current job's independent scratch,
// or an independently owned legacy report.
```

On **timeout with an existing cached report**:

```go
respMu.Lock()
cached, ok := lastInResp[job.ep]
if ok {
    if len(cached) <= len(scratch) {
        copy(scratch, cached)
        respData = scratch[:len(cached)]
    } else {
        // Legacy/custom devices may legitimately return >64 bytes.
        respData = append([]byte(nil), cached...)
    }
}
respMu.Unlock()
if ok {
    break // existing report-replay semantics and status
}
// Otherwise, preserve the existing no-cache wait/retry path.
```

The example is conceptual; Codex may keep the branch inline and use the existing Go style. Important invariants:

- **Copy the cached bytes while holding `respMu`** before unlocking. Do not do `cached := lastInResp[ep]` under lock and `copy(scratch, cached)` afterwards: a concurrent fresh response may overwrite that same memory.
- Never pass `lastInResp[ep]`'s mutable backing slice to `writeRet`, not even when it uses `writeMu`; that write mutex does **not** protect cache mutation.
- Fresh report cache updates copy bytes rather than saving aliases to device/worker scratch, which will be overwritten on its next request.
- Cached bytes are replayed independently for their endpoint and sequence. Do not dedupe equal reports, suppress idle completions or change report counters/timestamps; those are future NAK-idle policy decisions.
- If the cached payload exceeds 64 bytes on a non-optimized custom device, preserve correctness with a rare allocation instead of truncating. Keep the optimization focused on the known 20/64-byte reports.
- No socket/network write or device blocking operation while `respMu` is held. `pendingMu` remains the only request-liveness authority, and `writeMu` remains the only response serialization authority.
- No buffer alias should survive a cancelled/unlinked URB, EOF, drain or server close. Existing cleanup remains the sole teardown path.

### 4.5 Lifecycle and transport behavior are unchanged

Keep all these observable contracts:

- `CMD_UNLINK` cancellation for **active persistent** and **concurrent fallback** request: no late `RET_SUBMIT`, original `RET_UNLINK/-ECONNRESET` status when pending.
- USB/IP reader keeps accepting EP0/OUT/UNLINK even with overlapping IN; no newly queued work.
- EOF with logical device alive, managed device drain, `Server.Close`, native `CloseUSBServer` and `workerWG.Wait()` still cancel/join every busy/idle worker. No new worker registry or shutdown authority.
- `RET_SUBMIT`: 48-byte header + correct `ActualLength` + payload, one `writer.Write`; preserving body for IN, **no response body for OUT**, original OUT consumed-byte `ActualLength`, original `RET_UNLINK` framing and flushing. Both zero and positive batching interval paths work.
- Steam Deck 4 ms software wait budget, other endpoint descriptor polling, pre-read 10 ms timer, frame-on-state-update, live gyro and existing sensor timestamp semantics stay exactly as they were.
- USB/IP built-in EP0 requests and per-device `HandleControl` continue to return independently-owned reports; no worker buffer is exposed through EP0.

## 5. Required regression tests

Reuse existing Go in-process transport harnesses. Add compact device tests for the optional interface and precise tests for **cache alias/concurrent response**. Use bounded signals for meaningful overlap; avoid artificial pathological scheduler interleavings or a new test-only runtime inspection framework.

### 5.1 Optional interface and device compatibility

For Xbox360, Steam Deck, DualSense **and** DualSense Edge:

1. Type-assert the optional `usb.InterruptInBufferDevice` implementation.
2. Supply a >64-byte prefilled scratch, confirm the returned slice aliases only its valid first **20/64** bytes (or 8/4 on Deck keyboard/mouse), has correct report bytes, and leaves the trailing sentinel untouched.
3. Exercise active → neutral → active reuse to ensure PR-A `clear` prevents stale bytes. Cross-check against **existing fixed golden bytes**; for DualSense, mask timestamp bytes and account for one sequence increment per report rather than falsely expecting two independent calls to have identical sequence numbers.
4. Confirm the legacy `HandleTransfer` still returns independent allocated slices; it must not alias an earlier optional buffer and EP0 `GET_REPORT` must remain independent.
5. Confirm original wait/timeout/cancel semantics: Xbox360 EP1 initial and subsequent state; Steam Deck EP3 new notify, expiry latest report vs parent cancel; DualSense Edge EP4 expiry report vs cancellation. Confirm no change to secondary endpoints (Deck keyboard/mouse, Xbox unknown, DualSense unknown) or OUT/rumble callback.
6. For overlapping Edge IN plus EP0 reports, assert `seqCounter` produces distinct, properly advanced sequence numbers and the Go race detector reports no seq-counter race. Do not assert deterministic packet ordering across concurrent requests.

### 5.2 USB/IP cache copy, wire framing and overlap

Extend `internal/server/usb/transport_async_in_test.go` and related existing files:

1. **Buffered-path output and report equivalence:** send actual USB/IP IN `CMD_SUBMIT` requests to the three device types and assert exact lengths, byte fields, neutral/changed/repeated-state behavior and valid `RET_SUBMIT` headers/sequence. Retain existing `TestXbox360AndDualSenseEdgeUSBIPINRegression` and `TestSteamDeckAsyncINReturnsFreshStateForPendingRequest`.
2. **Legacy/custom-device fallback:** existing fake devices only implementing `usb.Device` still run through `HandleTransfer`. Verify their data and `RET_SUBMIT` bytes unchanged, including a fake that returns more than 64 bytes and then triggers cached replay.
3. **Cache reuse while readers overlap (must test):** arrange one fresh response that seeds the cache, another timeout/replay for the **same endpoint**, and a subsequent concurrent fresh response that overwrites the cache. Assert the previously selected response retains its own complete bytes while other URBs progress, with no data race under `go test -race`. This checks the exact real alias hazard introduced by reuse.
4. **Cross-endpoint separation:** two IN endpoints with distinct cached reports never replay each other's bytes or truncate data; existing `TestAsyncINReportCacheIsIndependentPerEndpoint` remains.
5. **Stable reuse:** after several input updates/URBs on a persistent worker, cache/response bytes remain correct and no stale report payload remains. A one-off `testing.AllocsPerRun` or `Benchmark...` comparison may be added only if simple and deterministic; **no benchmark-only PR or numeric optimization gate**.
6. **Same-endpoint concurrency:** preserve PR #62 test verifying two `HandleTransfer` calls can start before one returns; run equivalent optional-device overlap and confirm independent worker/fallback scratch (no two in-flight URBs share mutable report bytes).
7. **Active and fallback UNLINK:** keep them as **distinct** tests and assert no late completion. The reader must not block behind either path or lose an unlink.
8. **EOF/drain/shutdown:** preserve `TestAsyncINWorkersExitOnClientEOFWhileDeviceRemainsAttached`, `TestManagedDrainCancelsPendingSteamDeckINWithoutLateCompletion`, `TestManagedDeviceDrainReleasesParkedNoDataAsyncINWorker` and `TestManagedDeviceDrainWaitsForAsyncINWorker`; ensure scratch buffers and cached reports are not referenced after worker join.
9. **EP0/OUT/batching:** preserve PR #61's single direct `writer.Write` when applicable, OUT payload and length semantics, normal `RET_UNLINK`, positive batching and direct 0ms path, and rumble callback tests.

**Test strategy for cache mutation:** It is acceptable to use a small controllable fake device plus the existing USB/IP harness to force fresh/expired/fallback overlap. Prefer reliable channel signals over sleeps or arbitrary scheduler timing. Verify the old bug hazard would actually be caught; merely checking sequential cache equality is insufficient.

### 5.3 Tests and tools

```bash
gofmt -w usb/device.go internal/server/usb/server.go \
  device/xbox360/device.go device/steamdeck/device.go device/dualsense/device.go
go test -count=1 ./device/xbox360 ./device/steamdeck ./device/dualsense
go test -count=1 ./internal/server/usb ./usbip ./lib/viiper
go test -count=1 ./...
go test -race -count=1 ./device/xbox360 ./device/steamdeck ./device/dualsense ./internal/server/usb ./lib/viiper
go vet ./...
git diff --check
```

Respect existing Windows `libVIIPER` release build/header/ABI/export CI. Report local toolchain limitations accurately. No handheld device, Windows screen-idle, Modern Standby or hardware input/rumble test is required from local Codex or as a PR blocker.

## 6. Explicit non-goals and overengineering policy

- No change to Windows desktop idle classification, `GetLastInputInfo`, XInput packet replay policy, `NaksWhenIdle`, `IdleMode`, keepalive durations, WSGM hardware pacing, 64 ms timeout experiment, Steam Deck gyro resampling or any OS sleep policy.
- No changed device report bytes, packet/frame/timestamp **meaning**, report counters per call, HID descriptors, routing cadence, input snapshots beyond the minimum optional adapter, or publisher logic.
- No modification of `context.WithTimeout`/`WithCancel` frequency, pre-read 10ms delay, USB/IP URB processing order, write batching, socket protocol framing or job dispatch scheduling.
- No shared buffer pool, per-device global scratch, extra synchronization subsystem, manager, wrapper chain, epochs, barriers, task queues or new config switches. **Reuse existing mutexes only**, except for any concrete, minimal protection needed for proven normal-operation data integrity (e.g. `seqCounter` with existing mutex).
- Do not try to support multiple Windows users, RDP, multi-session or unsupported high-volume exotic payload devices; remain correct for already-supported generic `usb.Device` fallback.
- Production blocking findings must have a realistic failure path: broken native ABI, incorrect typed controller reports, corrupted cached replies under same-endpoint overlap, duplicate response/unlink failure, real hang/leak on native drain/EOF/close, wrong frame or timestamp, unexpected input latency or failing CI/tests. No theoretical-race discovery exercise beyond actual lifecycle and overlap.
- The Addon is standalone Full1902 and does not integrate with CTW. Both are **separate consumers** of one library fork.

## 7. Acceptance checklist

- [ ] Optional `usb.InterruptInBufferDevice` preserves the unchanged required `usb.Device` and native C ABI.
- [ ] Xbox360 EP1 (20 bytes), Steam Deck controller EP3 (64), other Deck IN endpoints (8/4), and DualSense/Edge EP4 (64) use PR-A in-place writers without changing old allocated `HandleTransfer` results.
- [ ] Streaming, timeout, context cancellation, state-consumption, tick/frame/sequence, sensor timestamp, feedback and EP0 semantics remain compatible.
- [ ] DualSense sequence increment is protected against real overlapping EP4/EP0 calls using the existing mutex, without long/waiting lock coverage.
- [ ] Each persistent worker and every overlapping fallback owns its own scratch. Buffered report is consumed before scratch reuse.
- [ ] Per-endpoint cache backing storage is reused under `respMu` and replayed through **request-owned copies under that lock**. No sender writes from mutable shared cache backing.
- [ ] Legacy/custom devices, including reports larger than 64 bytes, work correctly and retain the old method fallback.
- [ ] PR #61/#62 header/body, nonblocking dispatch, separate UNLINK cases, EOF/device drain/native teardown and 0ms/positive batching behavior all remain intact.
- [ ] Full relevant Go tests, race checks and CI pass. No required benchmark number and no mandatory physical device validation.
- [ ] PR stays focused on transport buffer reuse, no NAK-idle policy or consumer DLL adoption. Final DLLs are adopted by the user **after** the planned VIIPER optimizations.

## 8. Next PR (not part of PR-B)

After this implementation is merged, design a **separate Xbox360 idle-URB/NAK PR**:

- Inspect the CTW developer's measured Windows 11 Claw idle/sleep report: the patch stopped cached duplicate reports and `GetLastInputInfo` resets in CTW's test.
- Compare WSGM's `NaksWhenIdle() bool` and `IdleMode=auto` strategy; do not globally delete streaming cache/replay for Steam Deck or DualSense.
- Respect that the standalone Addon already sleeps in the user's tests; do not presume that all Windows environments/clients need the same idle policy.
- Investigate state-publisher cadence as evidence where it matters, but never bundle this policy change with PR-B's report allocation optimization.
- Only after VIIPER optimization PRs are complete, prepare **separate consumer adoption** for Addon and CTW-dev from the final fork revision.
