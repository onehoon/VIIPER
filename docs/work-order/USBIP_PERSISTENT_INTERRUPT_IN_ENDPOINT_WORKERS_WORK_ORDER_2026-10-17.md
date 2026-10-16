# Work Order — Reusable USB/IP Interrupt-IN Endpoint Worker with Concurrent-URB Fallback (PR 1)

**Date:** 2026-10-17  
**Repository / branch:** `onehoon/VIIPER` / `main`  
**Reviewed code baseline:** `532c4e4ae3097a94f1fe22e0d5904ca0d05dca10` (VIIPER PR #61 squash merge)  
**Implementation owner:** Local Codex  
**Change type:** One bounded, transport-only Go implementation PR with automated tests  
**Revision:** Corrects the original 2026-10-17 work order after independent P1/P2 review. **The prior endpoint FIFO design is withdrawn.** This replacement document is authoritative in its entirety.

## 1. Goal and binding design decision

Reduce per-request goroutine creation for ordinary sequential USB/IP non-EP0 Interrupt-IN requests **without changing existing same-endpoint concurrent request semantics or input latency**.

**Use a persistent worker when idle, with a per-URB goroutine fallback when busy:**

1. Lazily start **one reusable worker per endpoint per imported USB/IP stream**.
2. When that worker is already ready to receive a job, hand off the job directly.
3. When it is busy, not yet ready or otherwise cannot receive immediately, launch the **existing-style per-URB processing goroutine** for that job. Do not wait for the persistent worker.
4. The reader never blocks dispatching IN jobs, rejects an accepted URB, or creates a backlog behind a pending `HandleTransfer`.
5. Both paths execute the **same existing generic transfer logic**, with the original request-scoped context, timeout, report replay, UNLINK and completion behavior.

This is a **conservative partial optimization**. It removes short-lived goroutines in the common sequential case; when USB/IP submits overlapping requests to the same endpoint, it retains the original concurrent processing cost and behavior. **Do not claim that the PR removes all per-URB goroutines, contexts or report allocations.** We are not requiring a separate performance measurement PR or numeric CPU reduction target.

### Why the original FIFO plan was rejected (P1)

Current `handleUrbStream` processes each non-EP0 IN URB independently in a goroutine. With a descriptor-derived timeout (Xbox360 main EP1: 4 ms) and cached response replay, overlapping requests can reach their own timeout concurrently. A serial FIFO worker would instead make later requests wait for earlier `HandleTransfer` calls, potentially increasing completion delay toward `N × interval`. Worse, some no-data requests can park until UNLINK, leaving later queued jobs unable to begin.

**Such changed completion latency is not accepted as a hidden consequence of an optimization.** Preserving existing independent processing under overlap takes priority over maximizing goroutine reuse. This does **not** require new schedulers, arbitrary concurrency limits, global authorities, epochs, retry policies or complex defensive state machines.

### Source and architecture contract

Inspect the currently checked-out code and any commits after the reviewed baseline before implementing:

- `FORK_ARCHITECTURE.md` and `docs/libviiper/fork-api.md`.
- `internal/server/usb/server.go`: entire `handleUrbStream`, `Server.Close`, `BeginDeviceDrain`, `managedConnectionFor`, `WriteBatchFlushInterval` and cleanup.
- `internal/server/usb/transport_async_in_test.go`, `transport_lifecycle_test.go`, `transport_cleanup_test.go`, `transport_out_test.go` and `interrupt_in_wait_budget_test.go`.
- `docs/work-order/USBIP_NO_DATA_IN_URB_CPU_SPIN_BACKPORT_WORK_ORDER_2026-10-01.md`.
- `docs/work-order/USBIP_OUT_RET_SUBMIT_PAYLOAD_PROTOCOL_FIX_WORK_ORDER_2026-09-28.md`.
- `docs/work-order/USBIP_RET_SUBMIT_DIRECT_ENCODING_SINGLE_WRITE_OPTIMIZATION_WORK_ORDER_2026-10-16.md` and merged PR #61.
- `onehoon/SteamAddonforClaw/docs/Full 1902 Implementation/README.md`, including authority precedence and `FULL_1902_IMPLEMENTATION_ARCHITECTURE.md`.
- `onehoon/ClawTweaks-Dev` branch `release/v0.3.98.0` (the actual shared-fork consumer, **not** public CTW).
- WSGM reference: `KillerPixelCrew/VIIPER` branch `wsgm` commit `8210b0a9b54c098dbefb8a39a752c59b239a71f5`. Its serial endpoint worker and data-driven device path are **not** copied unchanged.

## 2. Actual consumers and safety constraints

| Consumer | USB/IP server ownership | Presentations |
| --- | --- | --- |
| Standalone Full1902 SteamAddonforClaw | **One** caller-owned server/bus with three logical typed devices, one attached presentation | Xbox360, Steam Deck, DualSense Edge |
| `onehoon/ClawTweaks-Dev` | Separate selected-device session creates its own server; normally only one selected server is running | Xbox360, Steam Deck, DualSense Edge |

Both currently set `WriteBatchFlushIntervalMs = 0` in application source. VIIPER must still work with positive flush intervals. The CTW-dev embedded DLL manifest currently pins `17b9334645b4ede72de81a8d09d967189fb3fdc4`, which is older than the Addon DLL. **Do not update, copy or replace either consumer DLL/lock/manifest in this PR.** Consumer adoption will be separate after a successful VIIPER artifact is produced.

The Addon is a **standalone Full1902 app**: it does **not** integrate with CTW. They merely consume the same fork. Do not touch physical PID1902/PID1901 restoration, Center M, HidHide, routing authority, native attachment, canonical typed C ABI or application-side publishers.

## 3. Reviewed transport behavior that must be preserved

At the baseline, `internal/server/usb/server.go` around lines 1099–1156:

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
    // Independently perform current HandleTransfer/timeout/replay/response.
}(seq, ep, dir)
```

The existing per-URB path:

- Executes `s.processSubmit(attemptCtx, dev, ep, usbip.DirIn, nil, nil)`.
- Uses a `context.WithTimeout` attempt where `interruptINWaitDuration` returns a positive interval. **Steam Deck controller EP3 currently has an explicit 4 ms wait budget**; retain it exactly. Other endpoints use their existing descriptor-derived intervals.
- On **non-nil fresh input**, stores a copied report in shared `lastInResp[ep]` and replies with that report.
- On deadline with an existing endpoint cache, sends that cached report, not a zero-length response.
- On deadline without a cached report, continues its existing wait/retry behavior (including current 1 ms retry sleep); do not introduce a new idle policy.
- On immediate nil without the attempt deadline, parks until **that URB's cancellation**. This is the existing CPU-spin correction and must remain.
- Checks cancellation, removes the pending entry, and sends the original `writeRet(seq, actualLen, respData, true)` completion.
- Protects `lastInResp` using `respMu`, `pending` using `pendingMu` and all response writes using `writeMu`.

`CMD_UNLINK` removes the target from `pending`, cancels its context, and emits `RET_UNLINK` with current status semantics. The stream cleanup cancels outstanding URBs, then joins their goroutines via `workerWG`.

PR #61 has already optimized `RetSubmit.Encode` and `writeRet` to send the 48-byte header and response body with a **single `writer.Write`**. Preserve the existing `retOut`, `writeMu`, error behavior and explicit EP0/OUT handling. Preserve the current **10 ms pre-read wait** before each `ReadExactly`; changing that latency/polling behavior is another task.

### Why WSGM's serial worker is not copied

WSGM's generic endpoint worker uses a buffered `jobs` channel and one consumer. It processes same-endpoint `HandleTransfer` calls sequentially and can change their start times. It also has unrelated changes for `usb.InterruptInSource`, hardware pacing, NAK-idle, per-device gyro resampling, output report encoding and `GOMAXPROCS`. **None of those changes belongs in this PR.**

## 4. Implementation specification

**Primary production file:** `internal/server/usb/server.go`. Modify other production files only if a demonstrated compilation or existing transport correctness dependency requires it. Keep the implementation inside the current stream/transport owner; do not introduce a new global component.

### 4.1 Dispatch to an idle worker, fall back immediately on overlap

Use only the minimal per-stream endpoint map and unbuffered channel. Suggested local structure (names illustrative):

```go
type inJob struct {
    seq    uint32
    ep     uint32
    ctx    context.Context
    cancel context.CancelFunc
}
type inEndpoint struct {
    jobs chan inJob // no buffer; only a ready receiver accepts a job
}
endpoints := map[uint32]*inEndpoint{}
streamDone := make(chan struct{})
```

A lazy worker should run approximately as follows:

```go
workerWG.Add(1)
go func() {
    defer workerWG.Done()
    for {
        select {
        case job := <-endpoint.jobs:
            runIN(job)
        case <-streamDone:
            return
        }
    }
}()
```

Request registration and dispatch should follow the current request lifetime:

```go
urbCtx, urbCancel := context.WithCancel(ctx)
pendingMu.Lock()
pending[seq] = urbCancel
pendingMu.Unlock()
job := inJob{seq: seq, ep: ep, ctx: urbCtx, cancel: urbCancel}

select {
case endpoint.jobs <- job:
    // Ready endpoint worker handles this URB without a new goroutine.
default:
    // A prior URB is still running (or receiver is not ready).
    // Do not delay this URB behind that earlier request.
    workerWG.Add(1)
    go func() {
        defer workerWG.Done()
        runIN(job)
    }()
}
```

These are **illustrations, not a line-for-line patch**. Codex should extract the current per-URB body into one small stream-local `runIN(job)` helper used by both paths, retaining its original `defer job.cancel()`. Include any necessary context/stream-exit guards using the existing owner and mutexes.

**Mandatory properties:**

1. A **zero-capacity channel** plus nonblocking `select/default` ensures the sole reader never waits for an occupied worker. **Do not use an endpoint FIFO, buffered channel, blocked send, extra enqueue goroutine or dropped URB.**
2. The persistent worker can process sequential jobs; any overlapping same-endpoint URB receives **a transient goroutine**, as before. Context timeout **starts when its own `HandleTransfer` is invoked**, never after another request returns.
3. Keep the original shared `lastInResp[ep]` cache and `respMu`. A fallback and persistent worker can execute **concurrently on the same endpoint**. Do **not** move the cache into worker-local storage.
4. Keep `pending map[seq]cancelFunc` and `pendingMu` as the one liveness/cancellation authority. Before emitting a completion, confirm that its sequence is still live under that mutex, remove it once, and avoid completing normally unlinked work.
5. The existing `writeMu` remains the sole response-serialization gate. No new mutex beyond what the chosen minimal implementation provably needs.
6. Reuse current `interruptINWaitDuration`, report builder, freshness rules, attempt timeout, cache replay, no-data park, failure classification, zero/positive batching and single-Write path.
7. No new per-device report allocation optimization in this PR. It is acceptable (and expected) for the fallback to allocate the legacy request goroutine.

### 4.2 Cancellation/UNLINK — P2 independent tests

`CMD_UNLINK` must work for the **two actual processing paths**:

- **Persistent-worker active URB:** cancel while `runIN` is executing in the reusable worker. The device handler receives cancellation and no late `RET_SUBMIT` is emitted.
- **Overlapping fallback URB:** occupy the persistent worker with a first request; start a second request on the **same endpoint** via fallback; cancel the fallback's sequence independently. Its own context must be cancelled, without blocking or cancelling the first request.

Both cases retain the original `RET_UNLINK` framing and `-ECONNRESET` status when found in `pending`. A completed/absent target retains the existing response semantics.

**There is no queued URB** in this revised design, so the original review's suggested 'queued UNLINK' test is superseded by a **fallback UNLINK** test. Do **not** use a combined 'active and/or queued' test; verify active and fallback separately.

A handler returning immediate nil without timeout parks only **its own** URB; it must not stop the reader or prevent a second URB being dispatched to a fallback goroutine. Do not introduce a new generalized cancellation state machine to guard only theoretical instruction-level races.

### 4.3 Stream lifetime, EOF, drain and close

The added reusable workers must not outlive their imported stream:

- On any `handleUrbStream` return (peer EOF/read error, normal close, device removed, `BeginDeviceDrain` or server shutdown), signal one **stream-local `streamDone`** to wake all idle persistent workers. This is not a second global authority.
- Cancel all remaining `pending` contexts, including persistent and fallback URBs; join **both classes** using the existing `workerWG`. An EOF can occur while the underlying logical device is still attached, so **device context cancellation alone is insufficient** to release an idle persistent receiver.
- Preserve defer ordering so `workerWG.Wait()` runs before any deferred `batchingWriter.Close()` that would close a writer still in use.
- Keep `Server.Close` joined managed connections, `BeginDeviceDrain` closing bound connections, `TransportDrain.Wait` and canonical `CloseUSBServer` teardown semantics intact.
- Preserve `DisableAutoBusCleanup` and the existing bus ownership/fail-close rules. Do not create a new native attachment/detachment owner.
- Ordinary connection/write failure must still be classified and cleaned up. No new transport retry or reconnect state.

### 4.4 EP0/OUT and protocol compatibility

- Synchronous EP0 and OUT remain reader-owned and may interleave with both classes of in-flight IN work.
- OUT `RET_SUBMIT.ActualLength` remains the consumed request payload length; its **response payload is always empty**, including when `HandleTransfer` produced output bytes.
- Preserve `RET_UNLINK` wire protocol, status and Flush, PR #61's `RetSubmit.Encode` and one `writer.Write` per reply, and 0 ms direct / positive flush-interval paths.
- Device descriptors, Xbox360/Steam Deck/DualSense Edge state, report counters and timing are untouched.

## 5. Focused automated regression tests

Use existing `internal/server/usb` in-process harnesses. Prefer deterministic controlled device entry/release signals to fragile exact millisecond scheduling assertions. Focus on the realistic host pipeline, worker cancellation and session teardown.

1. **Sequential reusable worker:** verify repeated sequential IN replies for one endpoint are correct and that an endpoint worker does not remain after connection close. Confirm worker reuse structurally in code review; do not add a new production-visible worker counter solely for testing.
2. **Same-endpoint concurrency — P1:** keep the first `HandleTransfer` pending, send another IN URB to the **same endpoint**, and assert that **both handlers were entered before releasing the first**. This test must fail if an unconditional FIFO worker serializes them. Verify separate sequences, payloads and no forced response order.
3. **Active-worker UNLINK — P2a:** ensure the persistent worker is actually processing an IN URB, cancel that target, assert prompt `RET_UNLINK/-ECONNRESET` and no late `RET_SUBMIT`.
4. **Overlapping fallback UNLINK — P2b:** keep one active worker busy, confirm another same-endpoint URB entered its fallback handler, UNLINK **only that fallback** request, and assert no late reply. Keep the first request independently cancellable or completable. Separate test from P2a.
5. **Immediate-nil park:** when an endpoint returns immediate nil without an attempt timeout, that one request stays pending without CPU-spin/empty completion. A second request on the same endpoint must not be trapped by it. Preserve `TestNoDataAsyncINWaitsForUnlink`.
6. **Independent endpoints:** separate report cache and response correctness for two endpoints of one USB/IP stream; valid responses may arrive in either order.
7. **Three controller regression paths:** retain `TestXbox360AndDualSenseEdgeUSBIPINRegression` and `TestSteamDeckAsyncINReturnsFreshStateForPendingRequest`; verify Xbox360 20-byte reports, Steam Deck and DualSense Edge 64-byte reports, neutral/changed/cached state, Steam Deck 4ms wait budget, frame/gyro/timestamp report content and no unwanted interval change.
8. **EP0/OUT while IN pending:** existing wire/framing and OUT no-payload tests remain correct; include both direct 0ms and positive batching paths as applicable.
9. **EOF with logical device alive:** after starting a reusable endpoint worker and an in-flight fallback, close the imported socket with the device context still active; all worker goroutines must be joined without requiring physical detach.
10. **Managed drain/close:** preserve `TestManagedDrainCancelsPendingSteamDeckINWithoutLateCompletion`, `TestManagedDeviceDrainReleasesParkedNoDataAsyncINWorker`, `TestManagedDeviceDrainWaitsForAsyncINWorker` and managed connection Close tests. Normal switch/drain/close must not leave active or idle workers.
11. **Transport errors:** no malformed or duplicate response, lost cancel, deadlock, stale completion, broken `RET_UNLINK` or failure to close on actual I/O faults.

Do not build an elaborate concurrency test suite solely for arbitrary instruction-by-instruction races. Code/CI validation is required; **real-device validation belongs to the user after merge** and must not block local Codex implementation or PR review.

## 6. Scope boundaries and quality gates

**In scope:** `internal/server/usb/server.go` plus focused transport regression tests. Keep one PR. It is fine to add compact local helper types/functions where needed; avoid new package-level manager, pool, state authority, epoch, inspector or generic scheduler abstraction.

**Out of scope:** WSGM `InterruptInSource` fast path, report encoder/allocation changes (future PR 2), Steam Deck gyro resampler, Xbox360 idle NAK, DualSense report cadence, new retry machinery, USB/IP receive loop 10ms pre-read change, C ABI, DLL packaging, CTW or Addon code.

Preserve real lifecycle and failure behavior for restart/shutdown/crash, Sleep/Hibernate/Resume, physical PnP loss, routing rollback/fail-close, HidHide/VIIPER native teardown, and PID1901/PID1902 restoration by leaving consumer authority unchanged and ensuring the internal worker joins correctly.

**Static/automated checks** (normal repository toolchain, CI owns platform-specific native builds):

```bash
gofmt -w internal/server/usb/server.go
go test -count=1 ./internal/server/usb ./usbip
go test -count=1 ./lib/viiper
go test -count=1 ./...
go test -race -count=1 ./internal/server/usb ./lib/viiper
go vet ./...
```

Also preserve CI's Windows canonical `libVIIPER` release build/export/header/ABI verification. Document any platform-unavailable local check without changing source contracts or making hardware testing a blocker.

**Blocking findings only when realistically harmful:** serialized same-endpoint IN behavior or new latency, reader blocked behind a busy worker, active/fallback UNLINK broken, lost/double completion, malformed USB/IP wire output, broken report/gyro/frame behavior, crashed/hung worker, real resource leak on EOF/drain/close, native ABI incompatibility or failing build/tests.

**Not blockers:** absence of new microbenchmark or measured CPU reduction, stylistic refactors, theoretical scheduler combinations, multi-session/RDP/Fast User Switching (unsupported), hardware validation not yet performed by the user.

## 7. Acceptance checklist

- [ ] One persistent reusable idle worker per IN endpoint per imported stream; fallback to legacy per-URB goroutine **only when it cannot receive immediately**.
- [ ] Unbuffered, nonblocking dispatch. **No FIFO/queued backlog** and no new wait behind another `HandleTransfer`.
- [ ] Same-endpoint overlapping IN URBs enter independently; request/attempt timeout and report replay unchanged.
- [ ] Active-worker **and** overlapping fallback UNLINK have distinct passing tests.
- [ ] Shared `lastInResp[ep]`, `respMu`, `pendingMu` and `writeMu` preserve the existing concurrency/response contracts.
- [ ] Stream-end signals and `workerWG` join both persistent workers and fallbacks on EOF, managed drain, Close and normal controller switch.
- [ ] 0ms and positive Flush support, PR #61 header encoder/single Write, EP0/OUT/UNLINK framing and three device reports remain correct.
- [ ] No device-specific timing/gyro/frame/idle behavior, typed C ABI, consumer artifacts or consumer code changed.
- [ ] Full Go regression checks and CI pass; report limitations honestly.
- [ ] PR description states **concurrency-preserving partial goroutine-reuse optimization**, not all-URB allocation removal or a proven percentage performance gain.
- [ ] Physical-device verification is **post-merge user responsibility**, not a merge gate.

## 8. Source references

- Our fork `onehoon/VIIPER` at `532c4e4ae3097a94f1fe22e0d5904ca0d05dca10`, particularly `internal/server/usb/server.go` and transport tests.
- WSGM `KillerPixelCrew/VIIPER` at `8210b0a9b54c098dbefb8a39a752c59b239a71f5`, generic persistent worker reference — **not** the serialized backlog contract.
- `onehoon/SteamAddonforClaw` Full1902 authority documents and canonical VIIPER integration rules.
- `onehoon/ClawTweaks-Dev` `release/v0.3.98.0` canonical typed Xbox360/Steam Deck/DualSense Edge sessions and artifact manifest.
