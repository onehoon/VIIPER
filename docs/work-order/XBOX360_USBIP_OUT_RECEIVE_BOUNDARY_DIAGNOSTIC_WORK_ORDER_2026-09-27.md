# Work Order — Xbox360 USB/IP OUT Receive-Boundary Diagnostic Trace

## Target

Repository: onehoon/VIIPER

Implementation branch: create a fresh implementation branch from the latest main.

Reviewed behavioral baseline:

    main @ 6296af4cb3f488791c9fae55a6ebe58610c0c9b4
    PR #50 already merged: Windows localhost attach restored to zero-copy

Implement from the latest main at execution time. Do not undo later main changes.

This is a diagnostic-only PR. It must not change controller behavior, USB/IP scheduling, attachment policy, callback behavior, or the public ABI.

---

## 1. Context

The current Xbox360 rumble investigation has already established several useful facts.

The existing native rumble trace records the device-side path:

    Xbox360.HandleTransfer
      -> X360RumbleRaw
      -> parser
      -> X360RumbleParsed
      -> X360RumbleCallbackDispatch

A real MSI Claw / Lies of P reproduction after restoring Windows localhost attach to zero-copy still showed the same class of stuck-rumble event.

During reproduced latch intervals:

- a non-zero rumble sequence reached the Xbox360 device handler;
- the expected terminal zero/zero STOP was absent from the existing Xbox360 device-level trace;
- packets that did reach Xbox360.HandleTransfer were parsed and dispatched correctly;
- zero/zero is not filtered by the parser;
- no parser/callback fix is currently justified;
- the existing CTW five-second physical STOP safety is older safety behavior and is not the root-cause change under investigation.

Current source also shows:

    USB/IP receive loop
      -> read CMD_SUBMIT header
      -> read complete OUT payload
      -> processSubmit
      -> Xbox360.HandleTransfer
      -> write RET_SUBMIT

Non-EP0 IN traffic is asynchronous, but OUT remains on the receive loop and is dispatched inline.

Therefore the next missing evidence is one boundary earlier than the current rumble trace:

    Did VIIPER's USB/IP server receive the complete terminal Xbox360 EP1 OUT payload?

This PR exists only to measure that boundary.

---

## 2. Read before implementation

Read the current versions of:

- FORK_ARCHITECTURE.md
- docs/libviiper/fork-api.md
- docs/work-order/XBOX360_RUMBLE_NATIVE_BOUNDARY_DIAGNOSTIC_WORK_ORDER_2026-09-24.md
- docs/work-order/PR48_XBOX360_USBIP_SUBMIT_BOUNDARY_TRACE_ADDENDUM_2026-09-26.md
- device/xbox360/device.go
- device/xbox360/rumble_trace.go
- internal/server/usb/server.go
- lib/viiper/xbox360.go
- lib/viiper/server.go
- lib/viiper/embeddedlog.go

The older PR48 boundary-trace addendum was design work only. Current main does not contain X360RumbleUSBIPSubmitOut / TraceUSBIPOutBoundary implementation. This work order turns that missing diagnostic boundary into a standalone implementation PR against current main.

---

## 3. Goal

Add structured, file-only diagnostic events around Xbox360 EP1 USB/IP OUT processing so one physical reproduction can distinguish:

    Case A
    terminal STOP never appears at VIIPER receive boundary

from:

    Case B
    terminal STOP is fully received by VIIPER
    but does not reach Xbox360.HandleTransfer

and also show whether VIIPER successfully hands the corresponding RET_SUBMIT to its current writer path.

This completion-side evidence is intentionally weaker than "sent to the USB/IP client". With WriteBatchFlushInterval > 0, EP1 OUT calls writeRet with flush=false, so writeRet may return successfully while the bytes remain buffered in batchingWriter until a later timer/threshold flush. The diagnostic must not claim socket flush or peer receipt.

Required server-side boundaries:

    CMD_SUBMIT header accepted
      -> full OUT payload successfully read
      -> X360 USB/IP OUT ingress trace
      -> processSubmit
      -> existing Xbox360.HandleTransfer / rumble trace
      -> writeRet returns success / current writer accepts RET_SUBMIT
      -> X360 USB/IP OUT writer-accepted trace

The primary diagnostic is the ingress event.

The completion event is supporting evidence for server-side response/backpressure analysis. It proves only that writeRet returned successfully and the current writer accepted the RET_SUBMIT bytes. When batching is enabled it does not prove that batchingWriter flushed those bytes to the socket, and it never proves peer receipt. It must not become a new lifecycle contract.

---

## 4. Exact behavior to instrument

Relevant current code is in internal/server/usb/server.go inside handleUrbStream.

Conceptually:

    read USB/IP header

    if OUT and transfer length > 0:
        ReadExactly(conn, outPayload)

    if non-EP0 IN:
        async worker path
        continue

    respData = processSubmit(...)
    actualLen = ...
    writeRet(...)

For Xbox360 EP1 OUT only, add:

### 4.1 Ingress event

Emit immediately after the complete declared OUT payload has been read successfully and before processSubmit is called.

For a zero-length EP1 OUT, emit at the equivalent point after the header has been accepted.

Suggested event:

    Event=X360USBIPOutIngress

Required fields:

    ProcessID
    BusID
    DeviceID
    TraceSessionID
    USBIPSeq
    Endpoint
    DeclaredLength
    Payload

Payload requirements:

- preserve exact bytes;
- encode at most the first 32 bytes as hex;
- do not parse motor values at this boundary;
- do not mutate the source slice;
- do not retain the scratch-backed slice after the hook returns.

For a normal terminal Xbox360 STOP, ingress must preserve:

    00 08 00 00 00 00 00 00

exactly.

### 4.2 Completion event

After processSubmit has returned and writeRet returns successfully for the corresponding RET_SUBMIT, emit:

    Event=X360USBIPOutWriterAccepted

Required fields:

    ProcessID
    BusID
    DeviceID
    TraceSessionID
    USBIPSeq
    Endpoint
    ActualLength

Do not emit X360USBIPOutWriterAccepted if writeRet failed.

Interpretation is deliberately limited:

    X360USBIPOutWriterAccepted
    = writeRet returned success and the current writer accepted the response bytes

It does NOT prove:

    batchingWriter flushed those bytes to the socket
    the kernel sent them
    usbip-win2 received them
    the peer processed the RET_SUBMIT

For EP1 OUT, current code calls writeRet(..., flush=false). If 1 ms batching is enabled, the response may still be buffered when this event is emitted.

Do not force a flush to make the diagnostic stronger. That would change transport timing.

Do not alter RET_SUBMIT status, payload, flush policy, or ordering for the sake of diagnostics.

The existing logger timestamps are sufficient for initial correlation. Do not add a timer/state registry solely to calculate latency.

---

## 5. Keep the generic USB server generic

Do not import device/xbox360 into internal/server/usb.

Do not parse Xbox360 packet contents in internal/server/usb.

Do not add Xbox360-specific fields to usb.Device.

Use a tiny optional internal interface implemented by Xbox360.

A suitable shape is conceptually:

    type usbipOutBoundaryTracer interface {
        TraceUSBIPOutIngress(seq uint32, ep uint32, declaredLength uint32, payload []byte)
        TraceUSBIPOutWriterAccepted(seq uint32, ep uint32, actualLength uint32)
    }

The exact naming may differ if a simpler implementation fits the existing code better.

Constraints:

- the interface remains internal to the Go implementation;
- it must not be added to the required usb.Device interface;
- no public C export;
- no generated libVIIPER.h change;
- no ABI change;
- no generic diagnostics manager;
- no registry keyed by device identity;
- no correlation state machine.

Because the optional interface is asserted across packages, exported Go methods on Xbox360 are acceptable if required by Go method identity. They are internal implementation methods, not public libVIIPER ABI.

---

## 6. Reuse the existing rumble trace session

Do not create a second logging subsystem.

The new events must reuse the existing Xbox360 RumbleTrace session and the same file-only asynchronous trace sink already used by:

    X360RumbleTraceStart
    X360RumbleRaw
    X360RumbleParsed
    X360RumbleCallbackDispatch
    X360RumbleTraceEnd
    X360RumbleTraceAbort

Use the same identity:

    ProcessID / BusID / DeviceID / TraceSessionID

The USB/IP protocol sequence number is additional connection-local context:

    USBIPSeq

Do not reuse or increment the existing device trace TraceSeq for ingress/writer-accepted events.

TraceSeq remains owned by the device-handler packet trace.

Do not create a mapping table to force USBIPSeq into X360RumbleRaw.

Ordered timestamps, exact payload bytes, session identity, and USBIPSeq are sufficient for this field diagnostic.

If reconnects or concurrent imports make a particular packet pairing ambiguous, classify that comparison as inconclusive instead of adding synchronization/state machinery.

---

## 7. Logging safety

This instrumentation must not become part of the bug.

The EP1 OUT receive path is latency-sensitive.

Required rules:

- no synchronous file I/O from handleUrbStream;
- no new worker dedicated to OUT tracing;
- no new queue;
- no sleeps;
- no debounce;
- no retry;
- no callback into the embedding application;
- no log fan-out through the synchronous VIIPERLogCallback;
- no unbounded payload logging;
- no packet cloning beyond the bounded data needed to create an owned trace record;
- logging failure must never fail a USB/IP request.

Reuse the existing file-only asynchronous trace logger.

The new trace hook must copy/hex-encode the bounded payload before returning because handleUrbStream reuses outPayloadScratch.

A later USB/IP request must not mutate a previously queued diagnostic record.

---

## 8. Explicitly preserve current transport behavior

Do not change any of the following in this PR:

- Windows receive mode;
- zero-copy policy;
- WskEvents;
- usbip-win2 0.9.8.0 pin;
- WriteBatchFlushInterval;
- batching writer behavior;
- async non-EP0 IN implementation;
- inputCh;
- lastInResp cache;
- endpoint deadlines;
- pending-IN map;
- UNLINK semantics;
- writer locking;
- RET_SUBMIT flush behavior;
- connection deadlines;
- server lifecycle;
- managed transport drain;
- bus ownership;
- attachment ownership;
- attach/detach classification;
- Xbox360 parser;
- Xbox360 callback synchronization;
- callback ABI;
- Steam Deck behavior;
- legacy clib behavior.

Do not add a runtime mode switch.

This PR must answer a diagnostic question, not attempt to fix the suspected regression.

---

## 9. Required tests

Add focused deterministic tests only.

Do not add scheduler torture tests or artificial race tests.

### 9.1 Ingress ordering

For one Xbox360 EP1 OUT CMD_SUBMIT, prove:

    complete payload read
      -> ingress hook
      -> device HandleTransfer

The ingress event must happen before the device handler sees the payload.

No wall-clock timing assertion is required.

### 9.2 Exact terminal STOP payload

Use:

    00 08 00 00 00 00 00 00

Verify ingress records:

- the actual USBIP sequence number;
- endpoint 1;
- declared length 8;
- exact payload bytes.

Verify the device still receives the same full payload unchanged.

### 9.3 Non-zero rumble payload

Use a representative non-zero Xbox360 output packet and prove identical preservation.

### 9.4 Completion ordering

For an EP1 OUT submit where writeRet returns success, prove:

    ingress
      -> HandleTransfer
      -> writeRet returns success / current writer accepts RET_SUBMIT
      -> X360USBIPOutWriterAccepted

Do not require a specific elapsed time.

Do not assert that this event means a socket flush occurred. In the 1 ms batching configuration, flush may happen later.

### 9.5 Failed RET_SUBMIT

If the test seam makes writeRet fail, prove no X360USBIPOutWriterAccepted event is emitted.

The existing failure behavior must be unchanged.

### 9.6 Scratch-buffer reuse

Issue two OUT requests that reuse the server scratch buffer.

Prove the first recorded ingress payload remains the first packet after the second request arrives.

This protects diagnostic integrity without introducing a new production buffer owner.

### 9.7 Bounded payload

For an OUT payload longer than 32 bytes:

- device receives the complete original payload;
- diagnostic payload contains only the first 32 bytes;
- DeclaredLength still records the full declared length.

### 9.8 Zero-length EP1 OUT

Verify:

    DeclaredLength=0
    Payload=""

and unchanged device handling.

### 9.9 Non-Xbox360 device

A device without the optional tracer behaves exactly as before.

No new required method may be added to usb.Device.

### 9.10 No active trace

If the Xbox360 logical device has no active RumbleTrace, both optional hooks are no-ops.

No panic and no behavior change.

### 9.11 File-only sink

Verify ingress/writer-accepted events use the existing file-only rumble diagnostic sink and do not invoke the embedding application's synchronous VIIPERLogCallback.

### 9.12 Existing trace tests

All existing tests for:

    X360RumbleTraceStart
    X360RumbleRaw
    X360RumbleParsed
    X360RumbleCallbackDispatch
    X360RumbleTraceEnd
    X360RumbleTraceAbort

must continue to pass.

---

## 10. Field interpretation

For one complete trace session, correlate:

    X360USBIPOutIngress
    X360RumbleRaw
    X360RumbleParsed
    X360RumbleCallbackDispatch
    X360USBIPOutWriterAccepted

### Case A — STOP received, recognized, and dispatched by VIIPER

Require the complete native evidence chain:

    X360USBIPOutIngress Payload=0008000000000000
    X360RumbleRaw        Payload=0008000000000000
    X360RumbleParsed     Recognized=true Left=0 Right=0 CallbackPresent=true
    X360RumbleCallbackDispatch Left=0 Right=0

Only with all of those records can the trace conclude:

    VIIPER received the complete terminal STOP payload,
    the Xbox360 parser recognized it as 0/0,
    and VIIPER reached the registered callback-dispatch point.

This still does NOT prove that CTW received or successfully processed the callback. Correlate the managed CTW/application logs for that later boundary.

If only X360RumbleRaw exists, conclude only that Xbox360.HandleTransfer observed the raw payload. Do not call that callback dispatch.

Do not modify the parser.

### Case B — STOP visible at ingress but absent at device trace

    X360USBIPOutIngress Payload=0008000000000000
    no corresponding X360RumbleRaw

If the session is complete and pairing is unambiguous, this is a concrete VIIPER transport-to-device-dispatch defect candidate.

Inspect the direct code path before proposing any architecture change.

### Case C — STOP absent from ingress and device trace

    no terminal X360USBIPOutIngress
    no terminal X360RumbleRaw

If the trace session is complete, first inspect the existing connection/read-error evidence around the interval.

If there is a payload ReadExactly failure, disconnect, truncation, or stream termination before the full declared EP1 OUT payload was read, the correct conclusion is:

    VIIPER did not obtain a complete terminal STOP payload at the ingress trace point.

Do not infer whether the USB/IP client attempted to send it.

If there is no such read/disconnect evidence and the complete session still contains no terminal ingress event, the strongest supported statement remains:

    VIIPER did not observe a complete terminal STOP payload at this server ingress boundary.

That moves the next investigation upstream of this successfully-read-payload boundary, but still does not prove the client never attempted or partially transmitted the request.

Possible later targets include:

    Windows XUSB / USB stack
    usbip-win2 client submission
    game / Steam input-output source

This case does not prove that the client failed to send the request, nor which upstream component omitted, truncated, delayed, or lost it.

A later usbip-win2 client-side capture may be required to prove whether the client emitted the STOP.

### Case D — ingress exists, handler exists, writer-accepted event missing

If ingress and device trace are present but X360USBIPOutWriterAccepted is absent:

- inspect existing writeRet failure/disconnect evidence;
- inspect writer contention/backpressure only if supported by timestamps/logs;
- do not infer socket flush or peer receipt either way;
- do not redesign async IN from this fact alone.

If X360USBIPOutWriterAccepted is present, conclude only that writeRet returned successfully and the current writer accepted the response bytes. With batching enabled, actual socket flush may still occur later.

### Case E — stop-like non-canonical bytes

Record the exact bytes.

Do not change parser behavior in this PR.

---

## 11. Trace completeness

Absence claims are valid only for a complete trace session.

Preserve the current completeness rules:

- valid X360RumbleTraceStart;
- valid X360RumbleTraceEnd for a normally retired device;
- no applicable Abort;
- no relevant droppedLogRecords evidence;
- no truncated/malformed file;
- normal managed transport drain completed before End;
- normal async log flush opportunity occurred.

If completeness is not established:

    result = inconclusive

Do not turn diagnostic-log completeness into a controller lifecycle success condition.

---

## 12. Relationship to the fork differential research

Recent source comparison found:

- onehoon inherited Alia5's data-driven/per-URB async IN architecture before the onehoon/Valkirie fork split;
- Corando/Valkirie later changed X360 state/scheduling to value snapshot + InputGate + persistent endpoint workers/hardware pacing/idle policy;
- those changes are behavior-changing scheduling experiments, not source-proven fixes for the missing X360 STOP;
- onehoon's X360 parser accepts zero/zero and dispatches it;
- current OUT dispatch remains inline on the receive loop;
- async IN can at most remain an indirect timing/backpressure hypothesis until the missing packet boundary is located.

Therefore this PR must NOT:

- port Corando persistent workers;
- port InputGate;
- enable hardware-paced completions;
- add X360 NAK-idle;
- revert to historical sequential snapshot;
- change inputCh;
- introduce an async/snapshot policy switch.

Those are separate investigations only if boundary evidence justifies them.

---

## 13. Validation

Run the repository's normal formatting, lint, unit tests, and libVIIPER build.

At minimum:

- Go formatting/lint used by current CI;
- Xbox360 tests;
- USB server tests affected by the optional boundary hook;
- libVIIPER build;
- generated-header/export consistency verification.

Expected ABI result:

    no public export change
    no generated C ABI change

If generated libVIIPER.h or export lists change, stop and investigate because this PR should not require an ABI change.

---

## 14. Hardware test after merge/adoption

After the new diagnostic DLL is adopted by the CTW test build:

1. reboot normally;
2. verify controller/VIIPER setup is otherwise unchanged;
3. use zero-copy;
4. retain current 1 ms CTW server batching configuration;
5. reproduce Lies of P rumble normally;
6. if a latch occurs, capture the complete libVIIPER log;
7. identify the last non-zero sequence;
8. search for terminal zero/zero at:
   - X360USBIPOutIngress;
   - X360RumbleRaw;
   - X360RumbleParsed;
   - X360RumbleCallbackDispatch;
   - X360USBIPOutWriterAccepted;
9. preserve exact timestamps and USBIPSeq values;
10. do not combine the test with another VIIPER behavior change.

The purpose is a clean boundary measurement.

---

## 15. Non-goals

Do not include:

- rumble guard changes;
- CTW five-second STOP changes;
- synthetic STOP generation;
- callback retries;
- packet buffering;
- low-latency mode;
- write-batching policy changes;
- async-IN redesign;
- Corando/HHC scheduling ports;
- Addon changes;
- CTW application code changes;
- Steam Deck changes;
- public ABI changes;
- new lifecycle authority;
- new manager/strategy abstraction;
- theoretical race hardening.

---

## 16. Acceptance criteria

The PR is complete when:

1. current behavior is unchanged with tracing inactive;
2. complete Xbox360 EP1 OUT payload reception is observable before device dispatch;
3. successful writeRet / current-writer acceptance of the EP1 OUT RET_SUBMIT is observable without claiming socket flush or peer receipt;
4. both ingress and writer-accepted events correlate with the existing RumbleTrace session and USBIPSeq;
5. the canonical zero/zero STOP bytes are preserved exactly;
6. diagnostic records cannot be corrupted by scratch-buffer reuse;
7. no synchronous application log callback is added to the OUT hot path;
8. no public ABI/header/export changes occur;
9. all relevant tests/build checks pass;
10. the resulting hardware log can distinguish:
    - STOP absent before VIIPER device dispatch,
    - STOP present at VIIPER ingress but lost before handler,
    - STOP handled but RET_SUBMIT writer acceptance fails or later transport behavior remains unresolved.

Keep this PR strictly diagnostic. Do not fix the suspected transport regression until the trace identifies the first missing boundary.