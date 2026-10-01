# Work Order — Backport upstream no-data IN URB CPU-spin fix

Date: 2026-10-01

Repository: `onehoon/VIIPER`  
Target base: latest `main`  
Upstream reference: [Alia5/VIIPER `3111299d67bacbaa7f6a31d56ea9ae06678f5865`](https://github.com/Alia5/VIIPER/commit/3111299d67bacbaa7f6a31d56ea9ae06678f5865) — **Fix CPU spin on no-data IN URBs**

**Scope: one small USB/IP transport fix plus focused regression coverage.**

## 1. Goal and concrete fault

In `internal/server/usb/server.go`, `Server.handleUrbStream` starts an asynchronous worker for a non-control IN `CMD_SUBMIT` (`dir == usbip.DirIn && ep != 0`).

The worker calls `s.processSubmit`. When the emulated device returns `nil` *without an attempt-context timeout*, the current code breaks out of its loop and sends an empty `RET_SUBMIT`. Repeated host resubmissions can cause unnecessary USB/IP activity and CPU consumption.

The upstream fix keeps that no-data URB outstanding until its existing `urbCtx` is cancelled (e.g. by `CMD_UNLINK`, device drain, or server teardown), rather than completing it with an empty `RET_SUBMIT`.

**Do not conflate this with the existing timed-out-attempt branch.** The `expired` branch, including `lastInResp` reuse and its current retry behavior, is a different path and is **not** part of this work order.

Upstream release context: the change was released in v0.8.2 following an upstream user report about high CPU usage. That report is evidence of an upstream problem, not proof that the fork reproduces the same CPU percentage.

## 2. Required repository and product contracts

Read before editing:

- `FORK_ARCHITECTURE.md`;
- `docs/libviiper/fork-api.md`;
- `internal/server/usb/server.go`;
- `internal/server/usb/transport_async_in_test.go`;
- relevant `internal/server/usb/transport_lifecycle_test.go` tests.

The fork provides the canonical embedded typed `lib/viiper` API for the standalone Full 1902 MSI Claw Addon. Preserve its existing lifecycle authority:

- server-owned device/bus lifecycle and caller-owned long-lived bus;
- `ManagedTransportLifecycle`, `BeginDeviceDrain`, `TransportDrain.Wait`, and `workerWG` joining;
- existing `CMD_UNLINK` cancellation and `RET_UNLINK` semantics;
- exact Windows usbip-win2 attachment/detachment ownership, classified results, and fail-closed behavior;
- the canonical C ABI, exported symbols, generated headers, device report formats, and callbacks.

There is **no** CTW integration work and **no** Addon code change in this PR.

## 3. Exact production edit

File: `internal/server/usb/server.go`  
Function: `(*Server).handleUrbStream`  
Location: asynchronous non-EP0 IN worker, after the `respData != nil` and `expired` branches.

Current fork:

```go
if respData != nil {
    respMu.Lock()
    lastInResp[ep] = append([]byte(nil), respData...)
    respMu.Unlock()
    break
}
if expired {
    respMu.Lock()
    cached, ok := lastInResp[ep]
    respMu.Unlock()
    if ok {
        respData = cached
        break
    }
    time.Sleep(time.Millisecond)
    continue
}
// Device answered "no data" without blocking.
break
```

Make the narrow upstream-equivalent replacement **only** for the final no-data branch:

```go
// Device answered "no data" without blocking.
<-urbCtx.Done()
return
```

Expected behavior:

- **Non-nil IN data:** unchanged; cache the report, complete `RET_SUBMIT`, and remove the pending entry normally.
- **Timed-out IN attempt:** unchanged; retain the existing cached-report/retry policy.
- **Immediate `nil` with no deadline expiry:** no empty `RET_SUBMIT`; park on `urbCtx.Done()` without polling or a new ticker.
- **`CMD_UNLINK`:** the existing pending cancellation ends the worker; the host receives the existing `RET_UNLINK` with the existing status semantics, not a late empty `RET_SUBMIT`.
- **Managed device drain/close:** existing cancellation ends the worker, allowing the existing `workerWG` and managed transport teardown to complete.

The existing `defer urbCancel()` and `defer workerWG.Done()` already cover worker exit. Do not create a second state/cancellation/cleanup mechanism solely for this path.

## 4. Focused tests

Prefer extending `internal/server/usb/transport_async_in_test.go` using its real `OP_REQ_IMPORT` + `CMD_SUBMIT` transport pattern rather than introducing a new framework.

**Test A — no-data IN remains pending, then UNLINK completes cleanly (required):**

1. Register a small fake device with a valid interrupt IN endpoint and `HandleTransfer` returning `nil` immediately for that endpoint (not because `ctx` hit its deadline). Signal when the handler is entered so the test does not rely solely on sleeps.
2. Import the fake device over the existing in-process USB/IP server harness.
3. Submit an IN URB, observe that no empty `RET_SUBMIT` is sent while no data is available (use a **bounded** connection read deadline; do not hang tests).
4. Submit `CMD_UNLINK` targeting that sequence number. Confirm the next protocol response is `RET_UNLINK` with the pre-existing cancellation status. Confirm no stray `RET_SUBMIT` is emitted for the cancelled IN request.
5. Close/clean up all connections and the server through existing test cleanup.

This test must fail on the old `break` behavior and pass with the change. Avoid adding arbitrary sleeps, scheduling hooks, or production instrumentation to force rare interleavings.

**Test B — managed drain does not retain a parked worker (required if existing coverage does not cover this exact case):**

- Reuse the existing `TestManagedDeviceDrainWaitsForAsyncINWorker` pattern, adapting only as needed to cover a device that promptly returns `nil`.
- Once a no-data IN worker is outstanding, initiate `BeginDeviceDrain(dev)` and verify `TransportDrain.Wait()`/server close finishes within a bounded test deadline.
- Do **not** alter managed connection ownership or drain implementation merely to pass this test.

**Positive control (small, within existing tests if possible):** a non-nil IN report still completes with the same sequence, expected actual length, and identical response bytes. Retain existing async IN, OUT, and EP0 tests.

Test machinery should remain proportionate to a two-line production change. Prefer one small test helper over a general-purpose test abstraction.

## 5. Explicit exclusions

Do **not** include:

- upstream `d5a672fa02dc34538a14ae21646be1d5b2837020` (orphaned-device automatic cleanup / read-deadline change);
- automatic bus cleanup or caller-owned bus policy changes;
- new `conn.SetReadDeadline` cancellation hooks in production;
- scheduling/refactoring of the 10 ms main-loop wait or 1 ms expired-attempt retry;
- changes to `pending`, `workerWG`, managed connection tracking, `CMD_UNLINK`, or `RET_SUBMIT` framing beyond what the exact fix demonstrably requires;
- changes to Xbox360/Steam Deck device implementation, physical input, IMU, haptics/rumble, HidHide, or native USB/IP attachment;
- upstream v0.8.x merge/rebase, DS4 `UpdateFlags`, dependency bumps, new public exports, or Addon dependency adoption.

Do not add new locks, managers, retry machinery, epochs, states, or compatibility fallbacks for theoretical races. If tests expose a reproducible real lifecycle regression, document it separately rather than silently expanding this work order.

## 6. Validation

Run the normal repository checks, including:

```sh
go test ./internal/server/usb/...
go test -race ./internal/server/usb/...
go test ./...
go vet ./...
```

Build and validate the canonical Windows shared library using the repository's current `just build-libVIIPER Release`/CI workflow. Verify existing generated-header/export/ABI checks still pass; no ABI change is expected. Do not commit generated DLL/header artifacts.

The PR description should identify the upstream commit and report the focused test results and canonical build/CI status. Any Addon pin/update is a **separate reviewed change after this fork PR**.

## 7. Acceptance criteria

- The old immediate-`nil` branch no longer writes an empty IN `RET_SUBMIT`.
- That worker waits without polling until its existing URB context is cancelled.
- Existing valid IN response, cached response, and timeout paths retain their behavior.
- `CMD_UNLINK` and managed drain release the parked worker without hanging.
- No change to Full 1902 typed ownership, bus lifetime, native attachment, or public ABI.
- Only the minimum production code and focused regression tests are changed.

**Expected production diff:** replace the one `break` with `<-urbCtx.Done(); return` in the no-data IN branch. Nothing else needs to be redesigned.
