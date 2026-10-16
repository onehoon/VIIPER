# Work Order — USB/IP Persistent Interrupt-IN Endpoint Workers (Transport-Only PR 1)

**Date:** 2026-10-17  
**Repository:** `onehoon/VIIPER`  
**Target branch:** `main`  
**Reviewed baseline:** `532c4e4ae3097a94f1fe22e0d5904ca0d05dca10` (squash merge of VIIPER PR #61)  
**Implementer:** Local Codex  
**PR scope:** One focused transport implementation PR, with in-process Go regression tests; no consumer-repo changes.  
**Reference, not a cherry-pick:** `KillerPixelCrew/VIIPER` `wsgm` at `8210b0a9b54c098dbefb8a39a752c59b239a71f5`, especially the **generic** persistent endpoint worker in `internal/server/usb/server.go`.

## 1. Goal

Replace the current **one goroutine per USB/IP non-EP0 IN URB** with **one persistent, on-demand worker per IN endpoint for each imported USB/IP stream**. The worker processes requests on that endpoint, using the existing device `HandleTransfer` contract and current timeout, replay, cancellation and completion behavior.

This PR is **only** a scheduling/worker-lifetime optimization. **Do not include PR 2's device input-report allocation/encoding work.** In particular, do not attempt to claim that this change itself eliminates per-URB contexts or report allocations: the existing generic `HandleTransfer` path still needs cancellable per-request contexts and its report builders still allocate. The actual performance gain is not yet measured.

### Mandatory baseline and sources

Review before coding:

- `FORK_ARCHITECTURE.md`, `docs/libviiper/fork-api.md`.
- `internal/server/usb/server.go` (entire `handleUrbStream`, `Server.Close`, `BeginDeviceDrain`, `managedConnectionFor`, `batchingWriter` and its cleanup).
- `internal/server/usb/transport_async_in_test.go`, `transport_lifecycle_test.go`, `transport_cleanup_test.go`, `transport_out_test.go`, `interrupt_in_wait_budget_test.go`.
- `docs/work-order/USBIP_NO_DATA_IN_URB_CPU_SPIN_BACKPORT_WORK_ORDER_2026-10-01.md`.
- `docs/work-order/USBIP_OUT_RET_SUBMIT_PAYLOAD_PROTOCOL_FIX_WORK_ORDER_2026-09-28.md`.
- `docs/work-order/USBIP_RET_SUBMIT_DIRECT_ENCODING_SINGLE_WRITE_OPTIMIZATION_WORK_ORDER_2026-10-16.md` and the **merged** PR #61 implementation.
- For consumer product contract, `onehoon/SteamAddonforClaw/docs/Full 1902 Implementation/README.md` and its referenced current Full1902 architecture; `docs/VIIPER_INTEGRATION.md`.
- Inspect the **actual private fork** `onehoon/ClawTweaks-Dev` branch `release/v0.3.98.0` when checking the second consumer. Do not substitute the public CTW source.

Verify branch HEAD at implementation time, and review any commits after the pinned baseline.

### Products served by this fork

| Consumer | Current native design | Relevant device types |
| --- | --- | --- |
| Standalone Full1902 SteamAddonforClaw | One caller-owned USB server/bus, three logical typed devices pre-created; at most one Windows-attached virtual presentation | Xbox360, Steam Deck, DualSense Edge |
| CTW-dev (`onehoon/ClawTweaks-Dev`) | A separate canonical USB server/session is created for each **selected** device type, not three simultaneously running servers | Xbox360, Steam Deck, DualSense Edge |

Both applications now set their respective `WriteBatchFlushIntervalMs` to **0 ms** in current source; the VIIPER transport must nevertheless preserve existing positive-interval batching support. CTW-dev still has an **older pinned DLL** (`17b9334645b4ede72de81a8d09d967189fb3fdc4`) in `XboxGamingBarHelper/Native/VIIPER/viiper-artifact.json`. Neither consumer's DLL/package/hash should be modified in this PR.

This is a common transport change, **not** CTW integration into the standalone Addon.

## 2. Verified current code

At the reviewed baseline, `internal/server/usb/server.go` around the non-EP0 IN branch (approximately lines 1099–1156) does this for **each** received USB/IP `CMD_SUBMIT` with `DirIn` and `ep != 0`:

```go
urbCtx, urbCancel := context.WithCancel(ctx)
pendingMu.Lock()
pending[seq] = urbCancel
pendingMu.Unlock()
interval := interruptINWaitDuration(dev, ep)

workerWG.Add(1)
go func(seq, ep, dir uint32) {
    defer workerWG.Done()
    defer urbCancel()
    // Repeat current HandleTransfer/timeout/latest-report fallback logic.
    // On successful completion, remove from pending and call writeRet.
}(seq, ep, dir)
```

The existing worker:

1. Calls `s.processSubmit(attemptCtx, dev, ep, DirIn, nil, nil)`.
2. Uses `context.WithTimeout(urbCtx, interval)` when the endpoint has an interval; distinguishes `DeadlineExceeded` from parent/URB cancellation.
3. When fresh non-nil input arrives, updates the per-endpoint `lastInResp` cache and completes the requested sequence.
4. On a timed-out attempt with a cached report, repeats the cache; without a cache, sleeps 1 ms and retries.
5. When `HandleTransfer` returns an immediate `nil` without timeout, **parks until `urbCtx.Done()`**, sends no empty response and exits on UNLINK/teardown (the October 1 CPU-spin fix).
6. Removes pending status before writing the response, and calls the PR #61 `writeRet` with the original length and `flush=true`.

`CMD_UNLINK` removes the requested sequence from the current `pending map`, calls its cancellation function if found and replies with `RET_UNLINK`, status `-ECONNRESET` on a live removal, or success if already completed. The stream cleanup cancels outstanding URBs and calls `workerWG.Wait()`.

The existing `writeMu` serializes all asynchronous IN and synchronous EP0/OUT/UNLINK writes; PR #61 already directly encodes a 48-byte `RET_SUBMIT` header and appends payload to a per-stream buffer for a **single `writer.Write`**. Do not replace or undo that.

The URB receive loop currently retains a `select` with a **10 ms wait** before `ReadExactly`. Its removal or adjustment, and any input-pacing effect, is **out of scope** for this PR.

## 3. Why WSGM must be adapted, not copied wholesale

WSGM creates an endpoint-owned worker with a request channel and reuses it across URBs. That is the relevant idea to port.

Its other changes **must not** come along:

- `usb.InterruptInSource`, `InputSignal` and `WriteInputReport` fast path.
- `HardwarePacedCompletions` and endpoint-grid timing policy.
- `IdleMode`/`NaksWhenIdle` and `IdleKeepaliveInterval`.
- Changes to Steam Deck IMU gyro resampling, report numbers, HID descriptor intervals, USB/IP OUT/EP0 status, or Xbox360/DualSense device state.
- Changes to process-level `GOMAXPROCS`, socket read delay, attach/detach, logging or public API.

WSGM's generic path still creates per-URB request contexts and per-attempt deadline contexts; this PR can therefore be useful **without** attempting a zero-allocation redesign.

**Important fork-specific integration issue:** WSGM uses a buffered `jobs` channel (capacity 8) and sends requests to it from the USB/IP reader. Naively doing `endpoint.jobs <- job` can block the reader when that endpoint is backed up. A reader blocked trying to enqueue requests cannot receive a subsequent `CMD_UNLINK` from the same host stream. That is a plausible real-world issue for pipelined IN URBs, not an instruction to build a general scheduler.

**Required policy:** enqueueing a normally accepted IN URB must not leave the sole USB/IP request-reader blocked indefinitely behind a waiting endpoint worker. Use the **smallest** nonblocking-per-reader queue mechanism integrated with the existing `pendingMu`, rather than copying a bounded channel send that can stall protocol handling. Do **not** drop a valid pending URB merely because a small queue is full. No new per-URB goroutine to do enqueueing, as that defeats this optimization. A simple per-endpoint FIFO slice plus a coalesced wake channel is sufficient; do not build a general queue library or a separate scheduler manager.

## 4. Required implementation boundaries

**Primary production file:** `internal/server/usb/server.go`. Avoid changing any other production file unless needed for compilation or a demonstrated transport correctness issue.

### 4.1 One on-demand worker per stream/endpoint

Inside `handleUrbStream`, keep the current stream-owned `writeMu`, `pendingMu`, `pending` and `workerWG`. Add only the minimum per-stream bookkeeping to route accepted non-EP0 IN jobs to their endpoint worker.

A reasonable compact shape (names are illustrative):

```go
type inJob struct {
    seq    uint32
    ctx    context.Context
    cancel context.CancelFunc
}
type inEndpoint struct {
    queue []inJob
    wake  chan struct{} // capacity 1; coalesced queue notification
}
// Managed entirely inside one handleUrbStream invocation:
endpoints := map[uint32]*inEndpoint{}
```

- Create an endpoint worker **lazily** on its first IN submission. One goroutine for that endpoint runs until the imported stream ends, regardless of how many URBs it processes.
- Do **not** create a process-global endpoint registry, alter the caller-owned bus/device model, or share workers across import sessions.
- Preserve the endpoint's `interruptINWaitDuration(dev, ep)` including the **Steam Deck's existing explicit 4 ms wait budget**; other endpoints keep their descriptor-derived intervals. Do not silently substitute WSGM's Deck 6 ms descriptor/polling configuration.
- Keep `pending` as the single source for UNLINK liveness. Reuse `pendingMu` to protect enqueue, queue removal, and pending-map transitions where appropriate. If using a FIFO slice, clear popped job references and avoid retaining stale cancelled jobs unnecessarily; keep the queue simple.
- The reader should register the request in `pending`, enqueue the job without waiting for the endpoint to process it, and nonblocking-signal `wake`. It must stay able to read EP0, OUT and `CMD_UNLINK`.
- The worker consumes queued jobs **in endpoint FIFO order**; if a job was cancelled/unlinked while waiting, discard it without calling `HandleTransfer` or emitting `RET_SUBMIT`. No guarantee is required for ordering **across different endpoints** beyond the existing wire-level serialization.
- For the active job, invoke the existing `HandleTransfer`/`context.WithTimeout` loop. On success, complete only if the sequence is still present in `pending`. Remove it **once** under the existing mutex before writing, and cancel/release its request context when finished. Never hold `pendingMu` while blocking on device input, writing to the socket or performing a timed wait.
- Keep the existing `writeRet(seq, actualLen, respData, true)`; async error classification/logging (disconnect vs other) must remain.
- Multiple endpoints may have independent workers, so the existing `writeMu` remains the only serialization gate for RET_SUBMIT/RET_UNLINK and synchronous EP0/OUT.

### 4.2 Preserve exact generic IN behavior

Use `HandleTransfer` unchanged. The worker must retain all four results:

| Condition | Required behavior |
| --- | --- |
| Fresh, non-nil input | Return the current report, update the endpoint's cache and complete the matching URB |
| Attempt deadline expired and prior report exists | Replay the last cached report **for the same endpoint** at the current behavior/timing |
| Deadline expired and no cached response | Keep waiting/retrying as currently implemented; no new zero-length completion |
| Immediate nil without deadline | Keep this URB pending until its cancellation; do not busy-loop or send an empty response |

For cancelled/unlinked jobs, skip response emission, including when `HandleTransfer` has just returned. Keep parent cancellation and `context.DeadlineExceeded` distinct as in the existing implementation.

The current `lastInResp[ep]` plus `respMu` can be replaced with a **worker-local last-report slice** because this PR makes each endpoint's IN processing sequential. Prefer this simplification if the same per-endpoint cache semantics are preserved, including initial state, copy ownership, and replay. Do not change report bytes/counters or dedupe policies.

Do not change `device/xbox360/`, `device/steamdeck/` or `device/dualsense/` input encoders in this PR.

### 4.3 Queue/UNLINK integration

The current `pending map[seq]cancelFunc` is sufficient authority for cancellation. Its meaning must remain:

- live accepted IN request => present in `pending`, cancel function usable;
- unlinked request => removed, cancelled, never later completed;
- successful completion => removed before response write;
- cancelled queued request => may remain as a transient FIFO entry, but must be skipped and eventually released.

On receipt of `CMD_UNLINK`, retain the exact `RET_UNLINK` response framing, success vs `-ECONNRESET` status, existing `writeMu` and Flush behavior.

A cancelled active job must unblock via its existing context. A queued cancelled job must not permanently hold the endpoint worker or block subsequent valid jobs. Coalescing wake notifications must not lose work: worker checks the queue before sleeping, and queue append plus wake happens without dropping a pending request.

Do not add epochs, generation counters or redundant maps solely to defend against instruction-level timing interleavings. Protect the realistically reachable cancellation and teardown paths with the current pending-map/connection-owner invariants.

### 4.4 Stream exit, native Close and managed device drain

This is the **most important integration constraint**.

- The current stream exits on device cancellation, imported connection EOF, socket failure, explicit device drain, or server close. **Every endpoint worker created for that stream must terminate and be joined before `handleUrbStream` returns**.
- A connection EOF can happen while the device remains attached and its device context is still alive. Therefore **do not wait only on the device context** to stop idle endpoint workers; the stream's own termination must wake them. A single stream-local `streamDone` channel (or equivalent already-existing cancellation) is appropriate here, **not** a second global owner/state machine.
- Stream cleanup should close/notify worker stop, cancel remaining pending request contexts, wake blocked/idle workers and join them via the **existing `workerWG`**. Check defer ordering against `bw.Close()` so a worker cannot write to a closed writer.
- Preserve the existing `BeginDeviceDrain` closing bound managed connections, `TransportDrain.Wait` waiting for detached transports, `Server.Close` joining accepted connections, and canonical `CloseUSBServer` wait-for-transport/callback contract.
- Neither Addon's persistent server/bus nor CTW's session-scoped `TryCloseServer` may return with a leaked active endpoint worker referencing a prior imported connection.
- Keep the current `DisableAutoBusCleanup` and per-owner cleanup behavior. This PR does not own bus/device teardown, USB/IP native attachment or PID1901/PID1902 restoration.

If a worker exits on an existing I/O failure, maintain the existing error classification; do not introduce a retry/reconnect policy or silently leave pending work unjoined.

### 4.5 Preserve PR #61, EP0 and OUT

No modifications to `RetSubmit.Encode`, the stream-local `retOut` buffer or `writeRet` single-Write implementation other than directly necessary to call it safely from persistent workers.

- Sync EP0 and OUT stay on the reader; they may complete independently of queued IN URBs.
- OUT `RET_SUBMIT.ActualLength` stays the number of consumed OUT bytes, and **OUT response payload remains empty even if the device returns nonempty bytes**.
- Leave `RET_UNLINK` wire format and write/flush behavior unchanged.
- Leave `WriteBatchFlushInterval = 0` direct path and positive-interval batching intact.

## 5. Required automated tests (no additional benchmarking PR)

Reuse in-process USB/IP test harnesses; add only small focused fake devices when necessary.

**Target test files:** `internal/server/usb/transport_async_in_test.go`, `transport_lifecycle_test.go` and possibly `transport_out_test.go`. Follow the existing actual `OP_REQ_IMPORT` / `CMD_SUBMIT` / `RET_SUBMIT` / `CMD_UNLINK` test pattern.

1. **One worker for sequential IN URBs:** submit several requests on the same endpoint in sequence and prove that the worker is created once per endpoint/stream, not once per request. Prefer a small test-visible worker hook/count if there is an existing seam; otherwise use a narrow test-only strategy. Do not add a production-wide runtime inspector or permanent metric solely for this test.
2. **Independent endpoints:** on one imported stream, exercise IN on two endpoints and confirm neither endpoint's report cache/pending state is reused for the other; both requests complete correctly without corrupting frames.
3. **Queued cancellation and reader progress:** hold one IN handler pending, enqueue several more IN requests for the same endpoint (enough to cover a normal pipelined batch), then send `CMD_UNLINK` for a queued and/or active sequence on **the same connection**. Confirm prompt `RET_UNLINK` with `-ECONNRESET` and no stale `RET_SUBMIT`; after releasing/cancelling the held work, later valid jobs progress. The test must demonstrate that enqueue does not block the only reader.
4. **No-data CPU-spin regression:** a handler returning immediate `nil` without deadline keeps its URB pending until UNLINK. There is no empty response or busy loop; a later request can progress after cancellation. Preserve `TestNoDataAsyncINWaitsForUnlink`.
5. **Report equivalence:** preserve existing `TestXbox360AndDualSenseEdgeUSBIPINRegression` and `TestSteamDeckAsyncINReturnsFreshStateForPendingRequest`; assert neutral/changed/repeated-state bytes, lengths, sequence and report/frame counter remain compatible with this fork. Preserve Steam Deck's current wait-budget tests.
6. **EP0/OUT interleaving:** while an IN request is pending, EP0 and OUT must still be received and answered, retaining the PR #61 48-byte header + body framing and OUT no-payload contract. Existing positive-interval batching tests must keep passing.
7. **EOF without device removal:** start at least one pending IN endpoint worker, close the imported client socket while the device context remains alive and prove the stream exits and worker joins, without waiting for device detach.
8. **Managed drain/close:** preserve and extend `TestManagedDrainCancelsPendingSteamDeckINWithoutLateCompletion`, `TestManagedDeviceDrainReleasesParkedNoDataAsyncINWorker`, `TestManagedDeviceDrainWaitsForAsyncINWorker`, and managed connection Close tests as needed. Queued and active workers must not leak across remove/recovery, and native-style session teardown remains joinable.
9. **Existing ordinary transport failures:** confirm write failure, `RET_UNLINK` and mixed-completion tests still pass without a deadlock or malformed response.

Tests should have bounded waits and deterministic entry/release signals where useful. Do **not** add extensive artificial concurrency stress machinery or tests of unsupported pathological scheduler interleavings. The goal is ordinary host pipelining and real lifecycle recovery.

## 6. Review and implementation notes

- Keep scope to the shared USB/IP transport and its tests. One PR; do not modify application repositories, native artifacts or device types.
- **No producer/consumer scheduling benchmarks required before implementation.** An optional short before/after benchmark in the same PR is fine but is not a release gate; do not claim a percentage gain without measurement.
- This first PR reduces **goroutine creation per URB**. It intentionally may retain per-URB `context.WithCancel`, per-attempt `context.WithTimeout` and per-device report allocation. That is the future **PR 2** decision; keep this PR independently reviewable.
- Do not add new global managers, epoch/barrier synchronization, additional transport ownership layers, or an extensibility framework for unsupported consumers.
- Preserve current priority of real lifecycle safety: Sleep/Hibernate/Resume; crash/shutdown/restart; physical PnP loss; rollback/fail-close; HidHide; VIIPER native ownership/teardown; PID1901 ↔ PID1902 restoration. VIIPER's internal worker must not interfere with the existing owners for those operations.
- Supported consumer model is one interactive Windows user/session, with no RDP/Fast User Switching/multi-session requirement.
- The Addon and CTW-dev use separate deployment artifacts, although they share this fork. Do **not** modify or replace DLLs in either repository in this PR.
- The user, not local Codex, performs physical-device verification **after code merge**. **No hardware verification requirement or PR blocker** for this task.

### Build/test checks

Run with the repository's Go toolchain (currently Go 1.26.2):

```bash
gofmt -w internal/server/usb/server.go
go test -count=1 ./internal/server/usb ./usbip
go test -count=1 ./lib/viiper
go test -count=1 ./...
go test -race -count=1 ./internal/server/usb ./lib/viiper
go vet ./...
```

Run the existing canonical Windows `libVIIPER` build/ABI/export checks as configured in CI, e.g. `just build-libVIIPER Release` where available. If an environment-specific check cannot run locally, note that accurately in the PR description and rely on CI; do not require real handheld tests.

### Merge-blocking regressions

Only realistically reachable material defects: ordinary queued IN submissions preventing UNLINK from being processed, lost/double completions, malformed protocol frames, broken Xbox360/Steam Deck/DualSense Edge report behavior, incorrect OUT framing, failure to join workers on EOF/drain/close, real deadlock, broken canonical native ABI, build failure or failing relevant tests.

Do **not** block on unmeasured CPU benefit, optional benchmark output, stylistic architecture proposals, purely theoretical races, or a lack of physical test.

## 7. Acceptance checklist

- [ ] A single persistent worker is created lazily per endpoint **per imported stream**; no per-URB goroutine remains in the generic non-EP0 IN hot path.
- [ ] The USB/IP reader remains able to process `CMD_UNLINK`, EP0 and OUT even when IN work is queued.
- [ ] Existing pending-map authority, cancellation status, error classification and completed-URB wire framing are preserved.
- [ ] The generic `HandleTransfer` timeout/replay/no-data behavior is unchanged for Xbox360, Steam Deck and DualSense Edge; polling frequency/gyro/frame/timestamp contracts are not redesigned.
- [ ] Worker starts/stops have an explicit **stream lifetime**, preserving managed drain and server close join behavior without a new global owner.
- [ ] No late completion after an ordinary cancellation or drain, no leaked persistent worker on EOF.
- [ ] PR #61 direct header encoding and single `writer.Write` are preserved; OUT/EP0/`RET_UNLINK` remain correct.
- [ ] Both 0 ms and positive-interval batching paths remain supported.
- [ ] Focused tests plus full Go tests and CI are green.
- [ ] No public C ABI, consumer code, vendored DLL, native attach/detach, controller device/report code or separate PR 2 optimizations added.
- [ ] PR description states exactly what was optimized and what allocations remain, without unsupported quantitative CPU claims.

## 8. Source inspection reference

- VIIPER `main` `532c4e4ae3097a94f1fe22e0d5904ca0d05dca10`: `internal/server/usb/server.go`, `usbip/usbip.go`, `transport_async_in_test.go`, `transport_out_test.go`, `transport_lifecycle_test.go`, `FORK_ARCHITECTURE.md` and `docs/libviiper/fork-api.md`.
- WSGM `KillerPixelCrew/VIIPER` `wsgm` `8210b0a9b54c098dbefb8a39a752c59b239a71f5`: `internal/server/usb/server.go` persistent worker and generic path (not its data-driven/NAK/keepalive/pacer paths).
- WSGM performance notes: `KillerPixelCrew/WSGM/docs/perf/README.md`. Its 23-to-1 heap allocations result is from **combined data-driven changes**, not proof of this isolated PR's effect.
- Addon Full1902 architecture and its current shared canonical server configuration.
- CTW-dev `release/v0.3.98.0` actual three canonical typed session implementations and currently pinned artifact manifest.
