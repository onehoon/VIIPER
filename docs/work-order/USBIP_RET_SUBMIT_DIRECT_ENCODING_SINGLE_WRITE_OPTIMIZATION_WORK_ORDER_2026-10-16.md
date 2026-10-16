# Work Order — USB/IP RET_SUBMIT Direct Header Encoding and Single-Write Completion

**Date:** 2026-10-16  
**Target repository:** `onehoon/VIIPER`  
**Target branch:** `main`  
**Reviewed baseline:** `e3275bc156fc5dd1cf8c1f2798c5770f3257dd58`  
**Implementation owner:** Local Codex  
**Scope:** One narrow VIIPER Go PR, with focused automated protocol/transport regression coverage.  
**Reference implementation:** WSGM-pinned `KillerPixelCrew/VIIPER` commit `8210b0a9b54c098dbefb8a39a752c59b239a71f5`, primarily `usbip/usbip.go` and `internal/server/usb/server.go`.

## 1. Goal and product context

Reduce redundant serialization work and socket write calls in the common USB/IP `RET_SUBMIT` completion path. This is a transport-level, wire-compatible optimization, not a change to the emulated controller reports or URB completion policy.

The canonical `onehoon/VIIPER` library is consumed by **two independently deployed products**:

1. **Standalone Full1902 SteamAddonforClaw** — production `WriteBatchFlushIntervalMs = 0`, adopted in Addon commit `cb4e44990a896a88c83318e616b5c2f78e5a3970`. Its persistent runtime owns one USB server and bus with Xbox360, Steam Deck and DualSense Edge logical devices; only the selected presentation is attached.
2. **`onehoon/ClawTweaks-Dev`, `release/v0.3.98.0`** — a separate consumer of the **same canonical typed C API**. It has dedicated `CanonicalViiperXbox360Session`, `CanonicalViiperSteamDeckSession` and `CanonicalViiperDualSenseEdgeSession` implementations. Its currently bundled artifact is commit `17b9334645b4ede72de81a8d09d967189fb3fdc4` and its three session classes still specify 1 ms; migration to 0 ms is a **separate CTW change**.

The 1 ms values in CTW are not a separate tuned compatibility requirement. CTW commit `4519f7fc28b1bab9e7a8e776cc42bf2934be8e43` explicitly changed its canonical Xbox360 session from an unspecified flush interval to 1 ms to match the Addon **at that time**. The later Steam Deck and DualSense Edge sessions copied 1 ms at creation. The user intends CTW to use 0 ms as well. **This VIIPER PR must work with both 0 ms and positive batching intervals**, without modifying either consumer.

Read before implementing:

- `FORK_ARCHITECTURE.md` and `docs/libviiper/fork-api.md` in this repository.
- `internal/server/usb/server.go` and `usbip/usbip.go`.
- `docs/work-order/USBIP_OUT_RET_SUBMIT_PAYLOAD_PROTOCOL_FIX_WORK_ORDER_2026-09-28.md` — especially the OUT response body invariant.
- `docs/work-order/USBIP_NO_DATA_IN_URB_CPU_SPIN_BACKPORT_WORK_ORDER_2026-10-01.md` — preserve pending no-data IN / UNLINK behavior.
- Active Full1902 authority, if consumer integration questions arise: `onehoon/SteamAddonforClaw/docs/Full 1902 Implementation/README.md`, which points to `FULL_1902_IMPLEMENTATION_ARCHITECTURE.md` and newer precedence documents. **Do not reintroduce CTW integration into the standalone Addon.** CTW is a separate binary consumer of the shared VIIPER fork.

### Explicit non-goals

- Do not port WSGM's persistent endpoint workers, `InterruptInSource`, gyro resampler, NAK-idle policy, altered report cadence or 64 ms idle keepalive.
- Do not modify Xbox360/Steam Deck/DualSense Edge device code, descriptors, report contents, feedback/rumble callbacks, or input publishers.
- Do not change `CMD_SUBMIT`, `RET_UNLINK`, `CMD_UNLINK` or management packet encoding as part of this PR. WSGM optimizes more headers, but this PR concerns the `RET_SUBMIT` hot path only.
- Do not change C ABI exports, generated `libVIIPER.h`/`.def`, Go typed handles, attach/detach ownership, usbip-win2 driver ABI selection, or application-side runtime code.
- Do not add a new lock, queue, goroutine, pool, manager, epoch or performance configuration switch.
- Do not use a numerical CPU reduction target as a merge gate; the benefit from this isolated change is not yet measured. Do not require a physical-device test for PR merge.

## 2. Verified current implementation and hot path

The repository was reviewed at the exact baseline above. Verify the current HEAD before editing and reconcile only genuine intervening changes.

### `usbip/usbip.go` — `RetSubmit.Write`

The `RetSubmit` struct starts near line 232, and its `Write(w io.Writer)` implementation near line 242 independently serializes:

- five `HeaderBasic` `uint32` fields;
- `Status` (`int32`);
- `ActualLength`, `StartFrame`, `NumberOfPackets`, `ErrorCount` (`uint32`);
- the final 8 bytes of `Padding`.

The result is exactly **48 bytes (0x30), big-endian**. It currently issues multiple `binary.Write` calls followed by a padding write, even when the caller is an in-memory buffer.

### `internal/server/usb/server.go` — `Server.handleUrbStream`

Near lines 925–938, `WriteBatchFlushInterval > 0` selects `batchingWriter`. With zero interval, `writer = conn` (the direct-write path).

Near lines 961–995, the local `writeRet(seq, actualLen, respData, flush)`:

1. Acquires the **existing `writeMu`**.
2. Creates an all-success `usbip.RetSubmit` for the given sequence and length.
3. Resets a per-stream `bytes.Buffer` and calls `ret.Write(&retOut)`.
4. Calls `writer.Write(retOut.Bytes())` for the header.
5. If payload is non-empty, calls **a second** `writer.Write(respData)`.
6. Flushes only when `flush && bw != nil`.

Thus a normal 0 ms controller IN completion with a body typically calls the underlying connection's `Write` twice. The existing `writeMu` already protects the complete header+payload operation against interleaving between asynchronous IN completions and synchronous EP0/OUT completions.

Near lines 1107–1165, non-EP0 IN uses `pending`, `urbCtx`, `workerWG`, `lastInResp` and a later `writeRet(..., true)`. Near lines 1168–1176, EP0/OUT uses `writeRet(..., ep == 0)`. The OUT branch deliberately sets **`actualLen = len(outPayload)` and `respData = nil`**. This correction **must survive unchanged**.

The `logConn` wrapper writes raw-traffic logs by observing each actual `Write` chunk. Chunk boundaries may change from two to one, but the concatenated byte stream must be identical. Do not expand this PR into logging changes.

## 3. Required implementation — direct `RetSubmit` encoder

**Production file:** `usbip/usbip.go`.

Add a small method that writes the existing header bytes directly into a caller-owned 48-byte slice:

```go
// Encode writes the 48-byte RET_SUBMIT header into dst.
// dst must have at least 48 bytes.
func (r *RetSubmit) Encode(dst []byte) {
    binary.BigEndian.PutUint32(dst[0:4], r.Basic.Command)
    binary.BigEndian.PutUint32(dst[4:8], r.Basic.Seqnum)
    binary.BigEndian.PutUint32(dst[8:12], r.Basic.Devid)
    binary.BigEndian.PutUint32(dst[12:16], r.Basic.Dir)
    binary.BigEndian.PutUint32(dst[16:20], r.Basic.Ep)
    binary.BigEndian.PutUint32(dst[20:24], uint32(r.Status))
    binary.BigEndian.PutUint32(dst[24:28], r.ActualLength)
    binary.BigEndian.PutUint32(dst[28:32], r.StartFrame)
    binary.BigEndian.PutUint32(dst[32:36], r.NumberOfPackets)
    binary.BigEndian.PutUint32(dst[36:40], r.ErrorCount)
    copy(dst[40:48], r.Padding[:])
}

func (r *RetSubmit) Write(w io.Writer) error {
    var header [48]byte
    r.Encode(header[:])
    _, err := w.Write(header[:])
    return err
}
```

Use one clearly defined header-size constant if convenient; reuse the existing server-side `retSubmitHeaderSize` / `0x30` agreement without adding abstraction. Keep the `RetSubmit` fields and public calling convention unchanged. The `uint32(r.Status)` conversion intentionally preserves signed 32-bit two's-complement wire bits.

**Do not** add a second independently maintained header encoder just for `handleUrbStream`. `RetSubmit.Write` and the hot path must share `Encode`.

The example deliberately retains the existing `io.Writer` error contract; do not create generalized short-write retry infrastructure for this optimization unless an actual production contract defect is demonstrated.

## 4. Required implementation — one write per `RET_SUBMIT`

**Production file:** `internal/server/usb/server.go`, local `writeRet` in `(*Server).handleUrbStream`.

Replace only the `bytes.Buffer` used by `writeRet` with a **connection-local reusable byte slice** (typically initial capacity `48+64`):

```go
var writeMu sync.Mutex
retOut := make([]byte, retSubmitHeaderSize, retSubmitHeaderSize+64)

writeRet := func(seq, actualLen uint32, respData []byte, flush bool) error {
    writeMu.Lock()
    defer writeMu.Unlock()

    ret := usbip.RetSubmit{
        Basic: usbip.HeaderBasic{
            Command: usbip.RetSubmitCode,
            Seqnum:  seq,
            Devid:   0,
            Dir:     0,
            Ep:      0,
        },
        Status:          0,
        ActualLength:    actualLen,
        StartFrame:      0,
        NumberOfPackets: 0,
        ErrorCount:      0,
    }

    ret.Encode(retOut[:retSubmitHeaderSize])
    retOut = append(retOut[:retSubmitHeaderSize], respData...)

    if _, err := writer.Write(retOut); err != nil {
        return fmt.Errorf("write RET_SUBMIT: %w", err)
    }
    if flush && bw != nil {
        if err := bw.Flush(); err != nil {
            return fmt.Errorf("flush response: %w", err)
        }
    }
    return nil
}
```

This is an illustrative replacement, not an instruction to rewrite unrelated surrounding logic. Keep the existing `writeMu` boundary and all existing `writeRet` call sites, `flush` argument meanings, and error propagation.

Required invariants:

1. There is **one call to `writer.Write` for each successful `RET_SUBMIT`**, whether the response has a body or is header-only.
2. The write covers **48 + len(respData)** bytes, without stale bytes from a preceding completion.
3. The original 48-byte header fields, success status, seq number, `ActualLength`, zero fields and padding remain byte-for-byte identical.
4. The buffer is owned by the individual stream and is only mutated under the pre-existing `writeMu`. Do not use a package-global/shared buffer or add another mutex.
5. The initial 64-byte payload headroom is an **allocation optimization, not a payload limit**. EP0 descriptors and other legitimate responses larger than 64 bytes must be fully transmitted; `append` may grow capacity.
6. In the 0 ms direct path, one call to `writer.Write` means one application-level connection write attempt. **It does not promise one TCP packet or one physical USB/IP transaction**.
7. In the positive-interval path, maintain the existing `batchingWriter` and exactly the previous `flush && bw != nil` semantics.
8. OUT completions still contain **no response payload**, even when their `ActualLength` is nonzero. Do not modify the current `dir == usbip.DirOut` handling.
9. Do not alter `RET_UNLINK` serialization, its existing flush operation or UNLINK cancellation logic.
10. Preserve the current server error handling, managed draining and close behavior. Do not add a new lifecycle owner/worker.

Because `writer` can be the current `logConn` and/or `batchingWriter`, do not let `retOut` escape to a new asynchronous sender or retain it beyond the synchronous `Write` call. The existing synchronous `io.Writer` contract plus `writeMu` is sufficient.

## 5. Focused automated regression coverage

Prefer extending existing test infrastructure rather than introducing a test framework or a new production seam.

### Test A — independent golden 48-byte header

**Test file:** preferably `usbip/usbip_test.go` (new file is acceptable).

- Construct `RetSubmit` with distinct nonzero `HeaderBasic` fields, `Status = -104`, distinct values for all remaining fields and nonzero padding.
- Assert exactly **48 bytes**, byte-for-byte against a **literal independently specified golden frame**. Do **not** validate `Encode` solely against the refactored `Write`; they would share the same potential encoding bug.
- Also assert `RetSubmit.Write` produces that golden frame through a normal `bytes.Buffer`, and handles a writer error.
- Include an all-zero/default-success frame to validate the canonical server case.

### Test B — one direct write for body and header-only responses

**Test file:** extend `internal/server/usb/transport_out_test.go` and/or `transport_async_in_test.go`.

- Wrap the server-side in-process `net.Conn` with a narrowly scoped **test-only counting Write wrapper**, or reuse an equivalent existing harness.
- Exercise a real `CMD_SUBMIT -> handleUrbStream -> RET_SUBMIT` path with **`WriteBatchFlushInterval = 0`**.
- Verify **exactly one underlying server-side `Write` per `RET_SUBMIT`**, both for a nonempty IN response and for a header-only OUT response.
- Exclude/import-handshake traffic from the count; do not conflate `RET_UNLINK` traffic with `RET_SUBMIT`. If another connection wrapper or harness detail makes the count ambiguous, adjust the test at the existing URB-stream boundary rather than adding instrumentation to product code.
- Assert response bytes/sequence/length, not only the number of calls.

### Test C — production controller IN regressions

**Existing file:** `internal/server/usb/transport_async_in_test.go`.

Retain and run the existing cases:

- Xbox360 main IN `0x81`, 20-byte report, button/state change.
- DualSense Edge main IN `0x84`, 64-byte HID report, neutral sticks and gyro.
- Steam Deck main IN, 64-byte HID report, neutral and updated gyro/stick state.
- Relevant pending no-data IN, `CMD_UNLINK` and managed drain tests.

Extend minimally only where needed to establish that a new completion's bytes do not contain stale tail bytes from an earlier completion. Do not change report builders, polling budgets or idle behavior to make tests pass.

### Test D — >64-byte EP0 response and variable-length buffer reuse

Add a focused in-process EP0/transport test using the existing USB/IP harness pattern, a valid descriptor, or a small `usb.ControlDevice` fake:

- Return a legal control response longer than 64 bytes (for example, a 128-byte HID report descriptor).
- Verify every byte is returned with correct `ActualLength` and no truncation; this checks `append` capacity growth.
- Follow with a short or zero-body response **on the same connection**; verify it contains no bytes left over from the previous larger frame, and the next response begins at the correct header boundary.
- Avoid changing actual descriptor or EP0 behavior to satisfy a test; use the existing `processSubmit` contract.

### Test E — OUT wire contract, failure, batching

**Existing file:** `internal/server/usb/transport_out_test.go`.

Preserve and run:

- `TestUSBIPOutProcessesOnlyAfterCompletePayloadAndPreservesResponse`
- `TestUSBIPOutZeroLengthPayloadCompletesSuccessfully`
- `TestUSBIPOutResponseWriteFailureReturnsError`
- `TestUSBIPOutRetSubmitOmitsDeviceResponseAndPreservesStreamBoundary`

The last test deliberately has a device return nonempty data for an OUT transfer. Assert `RET_SUBMIT.ActualLength == len(outPayload)` but **no response body**, and that a second OUT response is still parseable.

A small explicit positive-interval batching test is appropriate if existing coverage does not already establish that the new combined frame is flushed at the same call sites. The **0 ms and 1 ms** code paths must both work. Do not demand a timing-sensitive millisecond-exact scheduling test.

### Test F — mixed completions and lifecycle

Keep the existing asynchronous IN + synchronous EP0/OUT mutual exclusion. Test a realistic stream with back-to-back completions and assert each 48-byte header plus body is parseable and not interleaved; do **not** require a particular cross-URB scheduling order that the current design does not promise.

Run the existing `CMD_UNLINK`, pending no-data IN, managed drain/close and `workerWG` tests. There must be no late completed response after a cancelled/purged URB beyond existing behavior.

## 6. Consumer compatibility and release boundaries

### Standalone Addon

- Existing Addon is already 0 ms; **do not** modify its code in this PR.
- Its pinned DLL/commit/hash and package dependency will be updated in a **separate consumer adoption PR after this VIIPER PR merges and a successful native artifact exists**.
- No change to persistent triple-device ownership, Steam/BPM presentation policy, PID1901/PID1902 restoration, HidHide, native callback lifetime, fail-close, or teardown.

### CTW-dev

- CTW-dev's **actual private fork** is `onehoon/ClawTweaks-Dev`, `release/v0.3.98.0`; do not use the public upstream CTW implementation as a substitute.
- Its three canonical typed sessions use `NewUSBServer` and `SetXbox360DeviceState` / `SetSteamDeckDeviceState` / `SetDualSenseDeviceState`, with typed attach/remove and fail-close recovery. All share this same USB/IP implementation when a new DLL is adopted.
- CTW's three current `WriteBatchFlushIntervalMs = 1` settings originated from the old Addon choice. The requested 0 ms alignment is a **separate CTW-side change**, not part of this VIIPER PR. Preserve batching compatibility meanwhile.
- CTW currently packages `XboxGamingBarHelper/libVIIPER.dll` with `XboxGamingBarHelper/Native/VIIPER/` header/DEF, `viiper-artifact.json` and `Doku/VIIPER_CANONICAL_ARTIFACT.md`. Update these in a **later CTW artifact adoption change**, not this PR.
- There is no change to the canonical exported ABI and no required CTW P/Invoke rewrite.

### Common real-world safety

Review that the transport-only edit does not weaken normal shutdown, restart, suspend/resume, device loss/re-enumeration, native detach/remove, `CloseUSBServer`, `ManagedTransportLifecycle` or `TransportDrain` behavior. These are **existing ownership/lifecycle contracts**, not a request to invent new synchronization for theoretical interleavings.

## 7. Build, review and merge criteria

Required local/CI static and automated checks (appropriate platform/toolchain):

```bash
gofmt -w usbip/usbip.go internal/server/usb/server.go
go test ./usbip ./internal/server/usb
go test ./lib/viiper
go test ./...
```

Use the existing Windows native snapshot/build workflow as appropriate for the shared library. Keep generated C exports/header stable. Report test outcomes and any platform-excluded checks in the PR description; do not suppress test failures or reduce test coverage to satisfy this change.

**Merge-blocking findings:** actual malformed/misaligned USB/IP frames, extra/missing OUT payload, incorrect `ActualLength`, broken IN/EP0/OUT response, corrupt or interleaved stream, lost cancellation/teardown correctness, compile/test failure, real consumer ABI incompatibility, or ordinary reachable reliability regressions.

**Not merge blockers:** not-yet-measured CPU percentage gain, speculative nanosecond-level races, speculative performance of unsupported environments, and **physical device validation**. The user performs hardware tests after code merge, separately from the local Codex implementation/PR gate.

## 8. PR scope and acceptance checklist

One PR, preferably under roughly 300 changed lines (do not compromise correctness to hit an LOC number):

- [ ] `RetSubmit.Encode` writes the original fixed 48-byte header with unchanged endianness and all fields.
- [ ] `RetSubmit.Write` reuses `Encode` and performs one header write.
- [ ] `handleUrbStream.writeRet` reuses one per-stream byte buffer under existing `writeMu`.
- [ ] One `writer.Write` per `RET_SUBMIT`, including zero-body completions.
- [ ] 0 ms direct and positive-interval buffered output both preserve existing flush/error policy.
- [ ] Xbox360, Steam Deck and DualSense Edge input bytes are unchanged.
- [ ] Large EP0 payloads, zero payloads, successive frames and OUT framing remain correct.
- [ ] Existing asynchronous IN, UNLINK, drain and native lifecycle tests pass.
- [ ] No device implementation, C ABI, consumer repository, pinned DLL or generated artifact changed in this PR.
- [ ] No worker/NAK-idle/polling policy changes; no unnecessary state, lock or abstraction added.
- [ ] PR description describes the exact before/after `Write` behavior and test evidence, **without claiming a measured CPU gain**.

### Sources reviewed

- Our `usbip/usbip.go`, `internal/server/usb/server.go`, `internal/server/usb/transport_async_in_test.go`, `internal/server/usb/transport_out_test.go`, `internal/server/usb/transport_lifecycle_test.go` and `internal/server/usb/config.go` at `e3275bc`.
- WSGM fork `usbip/usbip.go` and `internal/server/usb/server.go` at its WSGM-pinned `8210b0a`.
- `onehoon/SteamAddonforClaw` current `main` and Full1902 documents.
- `onehoon/ClawTweaks-Dev` current `release/v0.3.98.0` canonical sessions and pinned artifact manifest.
