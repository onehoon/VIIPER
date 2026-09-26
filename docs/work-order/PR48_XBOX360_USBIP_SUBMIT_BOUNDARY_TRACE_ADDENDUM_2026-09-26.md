# Work Order — PR #48 Xbox360 USB/IP CMD_SUBMIT OUT Boundary Trace Addendum

## Target

Repository:

```text
onehoon/VIIPER
```

Existing PR:

```text
PR #48
branch: refactor/x360-rumble-nightly-diagnostic
reviewed head: e82be9c95aaa57a7469b50d66dba0cef9cd854fb
```

This work extends the existing temporary PR #48 diagnostic build. Do **not** open a separate feature PR unless technically necessary.

Read before implementation:

- `docs/work-order/XBOX360_RUMBLE_NATIVE_BOUNDARY_DIAGNOSTIC_WORK_ORDER_2026-09-24.md`
- `internal/server/usb/server.go`
- `device/xbox360/device.go`
- `device/xbox360/rumble_trace.go`
- `lib/viiper/embeddedlog.go`

PR #48 already provides forced-on, file-only Xbox360 rumble tracing and the full session identity:

```text
ProcessID / BusID / DeviceID / TraceSessionID
```

Preserve that contract.

---

# 1. Why this additional boundary is required

Current CTW field evidence has already narrowed a reproduced stuck-rumble event to:

```text
host / usbip-win2
        ↓
VIIPER TCP USB/IP receive
        ↓
Xbox360.HandleTransfer
        ↓
parser
        ↓
callback
        ↓
CTW
```

During a latch reproduction, the existing PR #48 trace did **not** show the expected terminal Xbox360 `0 / 0` packet at `Xbox360.HandleTransfer`.

Treat that absence as evidence only if that capture passes the existing PR #48 trace-completeness gate in section 10. If completeness was not established for the prior capture, its absence result is **inconclusive**. The additional boundary trace is intended to distinguish, in a complete future capture:

```text
A. the USB/IP CMD_SUBMIT OUT never reached VIIPER's TCP receive path

vs.

B. VIIPER read the complete CMD_SUBMIT OUT payload but it did not reach
   Xbox360.HandleTransfer
```

Static source currently shows the normal OUT path is direct and ordered:

```text
read USB/IP header
-> ReadExactly(conn, outPayload)
-> processSubmit(...)
-> dev.HandleTransfer(...)
-> RET_SUBMIT
```

and `internal/server/usb/server.go` explicitly keeps EP0/OUT transfers ordered.

This addendum measures that one remaining boundary rather than inferring it.

---

# 2. Goal

Add one additional **pre-device-dispatch** trace point for Xbox360 EP1 OUT traffic.

Required measurement point:

```text
USB/IP CMD_SUBMIT header accepted
-> complete OUT payload successfully read by ReadExactly
-> NEW trace event here
-> processSubmit(...)
-> Xbox360.HandleTransfer(...)
-> existing X360RumbleRaw
```

The new event must prove:

> VIIPER's USB/IP server received the complete EP1 OUT payload from the connected usbip client before invoking the device handler.

It must **not** claim which upstream component generated, omitted, delayed, or lost the request.

---

# 3. Exact implementation boundary

In `internal/server/usb/server.go`, the relevant current code is conceptually:

```go
var outPayload []byte
if dir == usbip.DirOut && xferLen > 0 {
    ...
    if err := usbip.ReadExactly(conn, outPayload); err != nil {
        return fmt.Errorf("read OUT payload: %w", err)
    }
}

// NEW diagnostic hook must run here for traced Xbox360 EP1 OUT.

...
respData := s.processSubmit(ctx, dev, ep, dir, setup, outPayload)
```

The trace must occur:

- only after the full declared OUT payload was successfully read;
- before `processSubmit`;
- before `dev.HandleTransfer`;
- before the RET_SUBMIT is written.

For an EP1 OUT with `xferLen == 0`, the declared payload is the successfully received empty payload; emit the boundary event with `Length=0` and an empty `Payload` at the same point. No `ReadExactly` call is needed for a zero-length payload.

Do not log partial payloads after a failed `ReadExactly` as a successful submit.

Do not change `ReadExactly`, `processSubmit`, RET_SUBMIT construction, batching, flush behavior, connection handling, pending-IN workers, or OUT ordering.

---

# 4. Keep the generic USB server generic

Do **not** import `device/xbox360` into `internal/server/usb`.

Do **not** add Xbox360 report parsing to the generic USB/IP server.

Do **not** extend the required `usb.Device` interface and force every device implementation to add a diagnostic method.

Use the smallest optional internal seam local to the USB server.

Preferred shape:

```go
type usbipOutBoundaryTracer interface {
    TraceUSBIPOutBoundary(usbipSeq uint32, ep uint32, payload []byte)
}
```

Then, only for OUT transfers:

```go
if tracer, ok := dev.(usbipOutBoundaryTracer); ok {
    tracer.TraceUSBIPOutBoundary(seq, ep, outPayload)
}
```

The interface itself may stay unexported in `internal/server/usb`; the method on Xbox360 must be exported only because Go requires cross-package method identity for the type assertion.

This is a **temporary diagnostic seam**, not a new public product contract.

Do not add the method to:

- `usb.Device`;
- libVIIPER C exports;
- generated `libVIIPER.h`;
- any application-facing interface.

If an equally small implementation avoids the exported Go method without creating new registry/state/manager complexity, that is acceptable. Prefer the simpler implementation.

---

# 5. Xbox360-side trace behavior

Implement the optional hook on the canonical Xbox360 device.

The method must:

1. return immediately when no PR #48 `RumbleTrace` is installed;
2. ignore non-EP1 traffic;
3. write through the **same existing PR #48 file-only async trace logger**;
4. use the same trace identity:
   ```text
   ProcessID
   BusID
   DeviceID
   TraceSessionID
   ```
5. never invoke the application's synchronous `VIIPERLogCallback`;
6. never change rumble parser or callback behavior.

Suggested event name:

```text
Event=X360RumbleUSBIPSubmitOut
```

Suggested fields:

```text
ProcessID=<pid>
BusID=<bus>
DeviceID=<device>
TraceSessionID=<session>
USBIPSeq=<USB/IP CMD_SUBMIT seqnum>
Endpoint=1
Length=<declared/read payload length>
Payload=<first 32 bytes, hex>
```

Use the same bounded payload policy as the existing `X360RumbleRaw` event:

```text
maximum first 32 bytes
```

`handleUrbStream` reuses its OUT-payload scratch buffer. The hook must not mutate or retain the supplied slice; it must synchronously encode/copy the bounded prefix into an owned hex string before returning, so later transfers cannot change an enqueued record.

Do not reinterpret or parse the payload at this boundary.

In particular, the USB server trace must record a valid terminal packet exactly as bytes:

```text
00 08 00 00 00 00 00 00
```

without assigning motor semantics there.

---

# 6. Do not change existing TraceSeq semantics

The existing PR #48 `TraceSeq` belongs to the Xbox360 device-handler trace:

```text
X360RumbleRaw
X360RumbleParsed
X360RumbleCallbackDispatch
```

Do **not** increment, reserve, or otherwise change `TraceSeq` from the new USB/IP boundary hook.

Use the USB/IP protocol's existing:

```text
USBIPSeq
```

on `X360RumbleUSBIPSubmitOut`.

Do not introduce a pending-correlation queue or state machine merely to copy `USBIPSeq` into `X360RumbleRaw`.

USB/IP `seqnum` is incremented **per connection**, not globally across a device trace session ([USB/IP protocol specification](https://www.kernel.org/doc/html/v6.15/usb/usbip_protocol.html)). Treat `USBIPSeq` as connection-local diagnostic context, not as a globally unique event ID or a sole cross-event join key.

For this field diagnostic, ordered timestamps and exact payload bytes are the primary evidence; `USBIPSeq` adds context within its connection. If concurrent imports or a reconnect/sequence reset makes boundary-to-raw packet matching ambiguous, classify that per-submit comparison as **inconclusive**. Do not add a connection registry, pending-correlation queue, or state machine to force a match. Session-wide absence conclusions in Case C still require the completeness gate in section 10.

---

# 7. Logging and timing constraints

This remains a diagnostic-only measuring build.

The new trace must not deliberately perturb the transport.

Do not:

- write files synchronously from `handleUrbStream`;
- route the packet trace through `s.logger` when that would fan out through `VIIPERLogCallback`;
- sleep;
- debounce;
- retry;
- coalesce;
- add a worker dedicated to OUT tracing;
- add another queue;
- add another file;
- change `WriteBatchFlushInterval`;
- change low-latency / WSK attach mode;
- change zero-copy mode;
- change RET_SUBMIT timing;
- change connection deadlines;
- change device attachment behavior.

Reuse the exact file-only bounded async sink already owned by PR #48.

A trace enqueue failure remains diagnostic-only and must never change controller behavior or USB/IP completion behavior.

---

# 8. Required tests

Add focused tests only. Do not build scheduler/race torture tests.

## 8.1 Boundary ordering

Using the USB transport test seam, prove for one EP1 OUT submit:

```text
complete OUT payload read
-> optional boundary hook invoked
-> device HandleTransfer invoked
```

The recorded order must be:

```text
USBIP boundary trace
before
HandleTransfer
```

Do not require wall-clock timing assertions.

## 8.2 Exact payload preservation

For:

```text
00 08 00 00 00 00 00 00
```

prove the boundary hook receives and records exactly those bytes and the actual USB/IP sequence number.

Also test one non-zero packet.

For a zero-length EP1 OUT, prove the hook records `Length=0` and an empty `Payload`, and the device receives the original empty payload unchanged.

## 8.3 Bounded payload

Provide an OUT payload longer than 32 bytes.

Prove:

- device behavior receives the original full payload unchanged;
- diagnostic payload text is bounded to the first 32 bytes.
- the hook does not mutate or retain the scratch-backed payload; after a later transfer reuses that buffer, the first recorded hex payload remains unchanged.

## 8.4 Non-Xbox360 devices unchanged

A device that does not implement the optional diagnostic interface must behave exactly as before.

No new required interface method may be introduced.

## 8.5 Trace absent / unavailable

When the Xbox360 device has no active trace context, the optional hook must be a no-op.

No panic, no behavior change, no logging dependency.

## 8.6 File-only logging

Prove the new `X360RumbleUSBIPSubmitOut` event reaches the PR #48 file-only trace sink and does **not** reach the synchronous `VIIPERLogCallback` observer.

## 8.7 Existing Xbox360 trace behavior preserved

Existing tests for:

```text
X360RumbleTraceStart
X360RumbleRaw
X360RumbleParsed
X360RumbleCallbackDispatch
X360RumbleTraceEnd
X360RumbleTraceAbort
```

must continue passing with the existing identity/completeness rules unchanged except where this addendum explicitly extends field interpretation.

---

# 9. Field interpretation

For one target session identified by:

```text
ProcessID / BusID / DeviceID / TraceSessionID
```

compare the new boundary event to the existing raw event.

## Case A — terminal 0/0 exists at USB/IP boundary and at X360 raw

```text
X360RumbleUSBIPSubmitOut Payload=0008000000000000
X360RumbleRaw            Payload=0008000000000000
```

At least one terminal request was observed at both VIIPER transport and Xbox360 device handling. Claim an exact per-submit pairing only when the correspondence is unambiguous under section 6.

Continue with existing parser/dispatch/managed-callback decision rules.

## Case B — terminal 0/0 exists at USB/IP boundary but no corresponding X360 raw

```text
X360RumbleUSBIPSubmitOut Payload=0008000000000000
(no corresponding X360RumbleRaw)
```

After applying the normal PR #48 trace-completeness checks and only when the boundary event can be matched to the device-handler trace unambiguously under section 6:

```text
the complete OUT payload was read by VIIPER's USB/IP server
but was not observed at Xbox360.HandleTransfer
```

This is a VIIPER internal transport-to-device-dispatch defect candidate.

Do not immediately redesign transport; inspect the exact direct code path first.

If concurrent imports or a reconnect/sequence reset makes the packet correspondence ambiguous, this case is **inconclusive**, not a dispatch-defect finding.

## Case C — terminal 0/0 absent from both

```text
(no USBIP boundary 0/0)
(no X360 raw 0/0)
```

When the trace session is otherwise complete and has no async-log-drop evidence:

```text
VIIPER did not observe a terminal 0/0 CMD_SUBMIT OUT at its TCP receive boundary
```

Investigation then moves upstream of VIIPER's receive boundary:

```text
game / XInput-XUSB / Windows USB stack / usbip-win2 request submission
```

This does **not** by itself prove a usbip-win2 0.9.8 bug.

A separate evidence-backed A/B test of usbip-win2 low-latency vs zero-copy receive mode may follow only after this boundary is measured.

## Case D — boundary sees a stop-like but non-canonical payload

Record the exact bytes.

Do not change the parser in this PR.

---

# 10. Trace completeness / absence claims

The existing PR #48 completeness contract remains mandatory.

A missing `X360RumbleUSBIPSubmitOut` event is usable for an absence-based conclusion only when:

- the target trace session has valid Start and End;
- no Abort applies;
- there is no `droppedLogRecords` evidence in the relevant interval;
- the log is not truncated/malformed;
- the normal transport drain completed before TraceEnd;
- the file-only sink had its normal bounded flush opportunity.

Do not convert diagnostics into lifecycle success criteria.

If the trace is incomplete:

```text
result = inconclusive
```

---

# 11. Explicit non-goals

Do **not** include any of the following in this PR #48 extension:

```text
usbip-win2 code changes
low-latency -> zero-copy mode switch
WSK receive redesign
RET_SUBMIT redesign
Xbox360 parser changes
synthetic terminal 0/0
rumble dead-man behavior
callback retry/buffering
new transport manager
new lifecycle authority/state machine
additional locking for theoretical interleavings
CTW changes
SteamInputAddon changes
```

This change is measurement only.

---

# 12. Validation

Run at minimum:

```text
go test ./device/xbox360/...
go test ./internal/server/usb/...
go test ./lib/viiper/...
go test ./...
go vet ./...
git diff --check
just build-libVIIPER Release
go run ./lib/viiper/exportverify -header dist/libVIIPER/libVIIPER.h -def dist/libVIIPER/libVIIPER.def
```

Confirm:

- canonical export count/ABI is unchanged from the current PR #48 head unless another already-approved PR #48 change requires otherwise;
- generated header has no new boundary-trace API;
- Release DLL builds successfully;
- existing PR #48 forced trace still works without an environment variable;
- `SetDiagnosticLogDirectory` behavior remains unchanged;
- the new trace appears in the same `libVIIPER.log`.

---

# 13. PR #48 handoff update

After implementation, update the existing PR #48 description with a short additional diagnostic note:

```text
- Added X360RumbleUSBIPSubmitOut at the USB/IP server receive boundary,
  after the full EP1 OUT payload is read and before device dispatch.
- Zero-length EP1 OUT is recorded as an empty, successfully received payload;
  USBIPSeq is connection-local, and ambiguous per-submit matches are inconclusive.
- The event is file-only and behavior-neutral.
- It carries the existing ProcessID/BusID/DeviceID/TraceSessionID identity,
  USBIPSeq, endpoint, length, and bounded raw payload.
- This distinguishes "not received by VIIPER TCP USB/IP" from
  "received by VIIPER but not observed by Xbox360.HandleTransfer" only when
  the existing completeness gate passes and packet correspondence is unambiguous.
```

Keep PR #48 Draft and unmerged for CTW nightly field measurement.

---

# 14. Acceptance criteria

The work is complete when all of the following are true:

1. One new trace exists after successful full EP1 OUT payload receive and before device dispatch.
2. The new trace uses the same PR #48 file-only async sink.
3. It never fans out through `VIIPERLogCallback`.
4. It carries `ProcessID / BusID / DeviceID / TraceSessionID / USBIPSeq`.
5. Payload logging is bounded to 32 bytes.
6. Existing `TraceSeq` semantics are unchanged.
7. Normal USB/IP OUT behavior, ordering, RET_SUBMIT behavior, batching, and attachment behavior are unchanged.
8. Non-Xbox360 devices require no code changes.
9. Tests prove boundary-before-HandleTransfer ordering and exact STOP payload preservation.
10. Full tests/build/export verification pass.
11. PR #48 remains a temporary diagnostic revision, not a product behavior fix.
12. Zero-length EP1 OUT is recorded as an empty payload, and the hook does not mutate or retain the reusable payload buffer.
13. `USBIPSeq` is treated as connection-local; ambiguous boundary-to-raw packet correspondence is inconclusive.
