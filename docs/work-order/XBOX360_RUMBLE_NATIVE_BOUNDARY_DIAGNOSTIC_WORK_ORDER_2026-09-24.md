# Work Order — Xbox360 Rumble Native-Boundary Diagnostic Instrumentation

## Baseline

Repository:

```text
onehoon/VIIPER
```

Implementation baseline reviewed for this work order:

```text
branch: main
commit: 61b6fc236bf71ff4f723223373eabd39c8676ca2
```

Read before implementation:

- `FORK_ARCHITECTURE.md`
- `docs/libviiper/fork-api.md`
- `device/xbox360/device.go`
- `lib/viiper/xbox360.go`
- `lib/viiper/server.go`
- `internal/server/usb/server.go`
- `lib/viiper/embeddedlog.go`

This is a **diagnostic-only PR**. It must not change controller behavior.

---

# 1. Context

A CTW measuring build reproduced a stuck-rumble event and proved the following on the CTW side:

```text
every 0/0 that reached CTW OnRumbleReceived
-> was not deduped
-> was not dropped
-> was written successfully
-> physical HID write completed 32/32 bytes
```

During the reproduced stuck event:

```text
last observed physical rumble = non-zero
terminal 0/0 never reached CTW OnRumbleReceived
```

This moves the next diagnostic boundary upstream.

The exact unresolved question is:

```text
Did the Xbox360 host-output 0/0 packet reach VIIPER?

If yes:
    did Xbox360.HandleTransfer recognize it?
    did VIIPER invoke the registered typed rumble callback?

If no:
    the loss is upstream of VIIPER
    (game / host / Windows XInput-XUSB / USB-IP transport)
```

Do not infer a VIIPER bug before this boundary is measured.

---

# 2. Important non-regression fact

The old legacy CTW feedback path and the current canonical typed Xbox360 path use the same relevant Xbox360 rumble interpretation:

```text
8-byte Xbox360 OUT report
[0] = 0x00
[1] = 0x08
[3] = left / large motor
[4] = right / small motor
```

A valid terminal STOP is:

```text
00 08 00 00 00 00 00 00
```

The canonical implementation already has a rumble-off unit test for `0/0`.

This PR must **not** alter that parser, reinterpret STOP, synthesize STOP, add a timeout, retry callbacks, or change callback ordering.

---

# 3. Goal

Add enough diagnostic evidence to distinguish these boundaries:

```text
Windows / USB-IP OUT
        ↓
Xbox360.HandleTransfer raw packet arrival
        ↓
Xbox360 rumble parser acceptance
        ↓
registered rumble callback dispatch
        ↓
typed C callback bridge
        ↓
managed application callback
```

VIIPER should provide evidence for the first three native boundaries only.

The application/CTW/Addon's own logs remain authoritative after the managed callback boundary.

---

# 4. Scope

Implement Xbox360-rumble trace instrumentation only.

Required properties:

- disabled by default;
- no public C ABI change;
- no generated-header change;
- no parser behavior change;
- no attach/detach behavior change;
- no callback lifetime change;
- no new worker thread;
- no timer;
- no retry;
- no queue dedicated to rumble;
- no synchronization added solely for theoretical races;
- use the existing libVIIPER owned **file-only asynchronous** logging sink;
- rumble trace records must never fan out through the embedding application's synchronous `VIIPERLogCallback`;
- logging failure must remain diagnostic-only;
- a missing trace record is never treated as packet-loss proof unless trace completeness is independently established for that measurement session.

Do not modify usbip-win2 integration.

Do not modify the legacy flat compatibility API behavior.

Do not add an MSI-Claw-specific policy to VIIPER.

---

# 5. Trace activation

Use one narrow opt-in diagnostic switch:

```text
VIIPER_X360_RUMBLE_TRACE=1
```

Exact value `1` enables the trace.

Unset, empty, or any other value leaves the trace disabled.

Read the switch once when the canonical Xbox360 typed device is created; do not poll environment state per packet.

The trace lifetime is the **Xbox360 logical-device lifetime**, not the callback-registration lifetime.

This is intentional: the trace must remain able to observe a recognized host packet while the application callback is absent and record `CallbackPresent=false`.

This is deliberately an environment-gated diagnostic feature rather than a new exported runtime-control API.

Do not add:

- a settings file;
- registry configuration;
- a new C export;
- a generic diagnostics manager.

---

# 6. Logging architecture

Do not make `internal/server/usb` understand Xbox360 report semantics.

The generic USB server must remain generic.

## 6.1 File-only trace sink — never the composite server logger

The existing `NewUSBServer` logger is a composite:

```text
libVIIPER.log file handler
+
optional synchronous VIIPERLogCallback observer
```

The rumble trace must **not** use that composite logger.

Per-packet trace logging through the composite logger would synchronously invoke the embedding application's `VIIPERLogCallback` from the USB OUT path and could perturb timing or allow re-entrancy into application code.

Instead, add the smallest internal helper that returns a logger backed only by:

```text
openRealEmbeddedLogFileHandler()
-> existing bounded asyncLogWriter
-> libVIIPER.log
```

with **no callback handler attached**.

Conceptually:

```text
buildEmbeddedRumbleTraceLogger()
    -> file handler only
    -> slog.DiscardHandler when file sink is unavailable
```

Reuse the same cached file handler / async writer already owned by `embeddedlog.go`.

Do not:

- open a second libVIIPER.log file;
- create a rumble-specific file writer;
- create another logging goroutine or queue;
- route trace events through `hw.logger`;
- invoke `VIIPERLogCallback` for rumble trace records.

Ordinary existing VIIPER diagnostics continue using the current composite server logger unchanged.

## 6.2 Device-owned trace lifetime

The preferred shape is a small **internal Go-only trace context owned by the Xbox360 device**.

Install it during canonical Xbox360 creation when:

```text
VIIPER_X360_RUMBLE_TRACE == 1
```

was observed at creation.

### Installation ordering is part of the measurement contract

The trace must be active **before the device can be auto-attached or accept its first host OUT transfer**.

The current generic creation path obtains `exportMeta`, registers the device, and may then run the `autoAttach` branch before returning to `createXbox360Device`. Therefore, do **not** attach the trace only after `createDeviceLockedPublic(...)` returns.

Required ordering:

```text
xbox360.New(...)
-> bus.Add(...)
-> obtain exportMeta / BusID / DeviceID
-> install Xbox360 trace context + emit X360RumbleTraceStart
-> register/complete canonical logical-device setup
-> only then allow autoAttachLocalhost, if requested
```

A narrow internal creation hook is acceptable if needed to install the trace after `exportMeta` exists but before the existing auto-attach branch. Keep it internal and behavior-neutral for every non-Xbox360 device.

Do not restructure attachment ownership or duplicate the auto-attach implementation merely for tracing.

The trace context contains only diagnostic state needed for this investigation:

```text
file-only logger
BusID
DeviceID
TraceSessionID
per-session TraceSeq
enabled / sealed state
```

## 6.2.1 TraceSessionID distinguishes logical-device incarnations

`BusID/DeviceID` is not a sufficient session key.

`VirtualBus` returns a removed `DevID` to its allocation pool, so a later Xbox360 creation on the same bus may reuse the same `BusID/DeviceID`. The previous transport may also still be draining after its public handle has already been finalized.

Assign each enabled Xbox360 trace context a process-local monotonically increasing:

```text
TraceSessionID=<uint64>
```

when that trace context is created.

Requirements:

- allocate from one process-wide atomic counter;
- zero is reserved for "no trace session" and must never be emitted for an active session;
- do not persist the counter across processes;
- do not derive it from handles, pointers, BusID, DeviceID, wall-clock time, or random values;
- every Start/raw/parsed/dispatch/End/Abort record for that context must carry the same `TraceSessionID`;
- `TraceSeq` starts from zero independently for each `TraceSessionID`.

This is diagnostic identity only. It must not participate in controller ownership, attachment, callback, or teardown decisions.

Use the full trace identity:

```text
BusID
DeviceID
TraceSessionID
```

in every trace event. Do not use only `BusID/DeviceID` or only `TraceSeq` as identity.

The hook is internal Go implementation detail only.

Do not expose it through `libVIIPER.h`.

## 6.3 Callback lifetime stays independent

`SetXbox360RumbleCallback(NULL)` clears only the application callback.

It must **not** disable the device trace.

This allows a recognized packet after callback clear to produce:

```text
CallbackPresent=false
```

without invoking any application callback.

Existing teardown paths such as `clearDeviceCallbackLocked` call `Xbox360.SetRumbleCallback(nil)` directly. They must keep doing so; do not route teardown through a new wrapper or alter callback ownership merely for tracing.

The trace context is not tied to the exported handle lifetime. A successful logical-handle finalization may occur before the existing managed transport drain has completed.

Therefore the trace context must remain alive until the corresponding existing transport drain has completed and the trace has been sealed as described in section 6.4.

Keep all callback teardown behavior unchanged.

## 6.4 Trace session start/end and transport-drain fence

When trace is enabled for a device, emit:

```text
Event=X360RumbleTraceStart
BusID=<bus>
DeviceID=<device>
TraceSessionID=<session>
TraceSeq=0
```

after `exportMeta` establishes the canonical identity and **before any auto-attach can expose the device to a host**, as required by section 6.2.

### TraceEnd must be after the existing transport drain

Do **not** emit `X360RumbleTraceEnd` from:

- `finalizeDeviceLocked`;
- `finalizeBusLocked`;
- immediately before either finalizer;
- any point before the corresponding `TransportDrain.Wait()` has completed.

The current teardown architecture intentionally permits this ordering:

```text
under lifecycleMu:
    clear callback
    detach
    BeginDeviceDrain
    logical device/bus removal
    handle finalization
    ForgetDeviceTransport
unlock lifecycleMu

outside lifecycleMu:
    waitTransportDrains(...)
```

A `HandleTransfer` already in flight may still finish during that drain window. Its rumble trace event is valid evidence and must be included before `LastTraceSeq` is frozen.

For every **successfully retired** traced Xbox360 device, retain the minimal trace context/identity independently of the finalized public handle until its existing transport drain completes.

A newly created Xbox360 device is allowed to reuse the same `BusID/DeviceID` while the prior incarnation is still draining. The two streams remain unambiguous because they have different `TraceSessionID` values.

Required order:

```text
BeginDeviceDrain
-> existing logical removal/finalization path
-> unlock lifecycleMu
-> existing TransportDrain.Wait()
-> no more HandleTransfer work for that retired transport
-> seal the Xbox360 trace context
-> atomically snapshot final LastTraceSeq
-> enqueue X360RumbleTraceEnd
-> release retained trace context
```

The trace seal must ensure no later raw/parsed/dispatch event can be emitted after `X360RumbleTraceEnd`.

Use the smallest synchronization already appropriate for the trace context (for example an atomic/sealed flag plus the sequence state). Do not introduce a new lifecycle manager, worker, epoch, or generalized teardown abstraction.

### All canonical removal entry points must obey the same fence

The rule applies whether the Xbox360 device is retired through:

- `RemoveXbox360Device` / `RemoveXbox360DeviceEx`;
- `RemoveUSBBus`;
- `CloseUSBServer`.

Those paths already aggregate and wait transport drains outside `lifecycleMu`. Preserve that architecture.

If implementation needs to carry a small diagnostic-only "trace to finalize after drain" item alongside the existing drain result, keep it internal and narrowly scoped. Do not move `waitTransportDrains` under `lifecycleMu`, and do not delay controller teardown for logging I/O.

### Failed removal

If removal fails and the logical Xbox360 device remains authoritative/alive, do **not** emit `X360RumbleTraceEnd` merely because a drain was started or attempted.

Only a successfully retired logical device gets an end marker.

A later successful retry owns the final drain fence and trace end.

### End marker

After the successful drain fence:

```text
Event=X360RumbleTraceEnd
BusID=<bus>
DeviceID=<device>
TraceSessionID=<session>
LastTraceSeq=<n>
```

through the same file-only async sink.

The existing async writer can report queue saturation as:

```text
libVIIPER logging backlog droppedLogRecords=<n>
```

The trace-end marker is part of the evidence contract: if it is absent, the trace session is incomplete and absence-based conclusions are not allowed.

Do not make logging completeness a controller-lifecycle success criterion. A missing trace-end record remains diagnostic-only and must never block device removal or server close.

## 6.5 Creation failure / rollback session closure

Because trace activation must occur before auto-attach, a trace session may start even though `CreateXbox360Device` ultimately fails.

Handle those paths explicitly.

### Known creation rollback

If creation fails with a known/safe failure and the just-created Xbox360 device is rolled back/finalized before the create call returns:

```text
X360RumbleTraceStart
-> creation/auto-attach attempt
-> known failure
-> rollback succeeds
-> X360RumbleTraceAbort
```

Emit:

```text
Event=X360RumbleTraceAbort
BusID=<bus>
DeviceID=<device>
TraceSessionID=<session>
LastTraceSeq=<n>
Reason=<stable diagnostic reason>
```

after any transport drain required by that rollback has completed. If no transport was ever exposed and no drain exists, emit Abort after rollback/finalization is complete.

`TraceAbort` is a terminal marker for a **non-committed creation session**. It is never interchangeable with `TraceEnd`, and an aborted session is never eligible for absence-based packet-loss reasoning.

### Unsafe/unknown creation outcome

If auto-attach/create enters the existing unsafe/unknown ownership state and the logical device remains retained under the fail-closed server contract, do **not** emit Abort merely because the public create operation returned failure.

The trace remains attached to that retained logical device until the existing fail-closed lifecycle eventually retires it successfully. That later successful retirement gets the normal drain-fenced `TraceEnd`.

If the process terminates before a safe retirement is observed, the session is simply incomplete and cannot support absence-based conclusions.

### Session identity on retry

A later retry/new creation always gets a new `TraceSessionID`, even when it reuses the same `BusID/DeviceID`.

No Start/End/Abort matching rule may group records solely by `BusID/DeviceID`.

---

# 7. Required trace points

Use a per-Xbox360-device monotonic diagnostic sequence number so lines from one host OUT transfer can be correlated.

The counter is diagnostic-only.

Every rumble trace event must also include:

```text
BusID=<canonical bus id>
DeviceID=<canonical logical device id>
```

because multiple Xbox360 typed devices — including a new incarnation reusing the same `BusID/DeviceID` while an older one drains — may write to the same `libVIIPER.log`. `TraceSessionID + TraceSeq` is the session-local correlation key.

## 7.1 Raw EP1 OUT arrival

Inside `Xbox360.HandleTransfer`, for every:

```text
dir == usbip.DirOut
ep == 1
```

when tracing is enabled, log before validating the rumble packet.

Suggested event:

```text
Event=X360RumbleRaw
BusID=<bus>
DeviceID=<device>
TraceSessionID=<session>
TraceSeq=<n>
Length=<len>
Payload=<bounded hex>
```

The payload is small for this device. Log at most the first 32 bytes.

This line is essential because a terminal command may arrive in a form the current parser does not recognize.

Do not log input reports.

## 7.2 Parser classification

For the same `TraceSeq`, classify the OUT packet.

For a recognized current rumble packet:

```text
Event=X360RumbleParsed
BusID=<bus>
DeviceID=<device>
TraceSessionID=<session>
TraceSeq=<n>
Recognized=true
Left=<0..255>
Right=<0..255>
CallbackPresent=<true|false>
```

For an EP1 OUT packet that is not recognized by the current rumble parser:

```text
Event=X360RumbleParsed
BusID=<bus>
DeviceID=<device>
TraceSessionID=<session>
TraceSeq=<n>
Recognized=false
Length=<len>
ReportId=<if available>
DeclaredLength=<if available>
```

Do **not** change parser acceptance to make the diagnostic pass.

## 7.3 Callback dispatch

Immediately before invoking the currently registered Xbox360 rumble callback:

```text
Event=X360RumbleCallbackDispatch
BusID=<bus>
DeviceID=<device>
TraceSessionID=<session>
TraceSeq=<n>
Left=<0..255>
Right=<0..255>
```

This must be emitted before the callback call.

The ordering for one recognized packet should therefore be:

```text
X360RumbleRaw
X360RumbleParsed Recognized=true
X360RumbleCallbackDispatch
<application managed callback log>
```

If no callback is registered, the parsed line must record `CallbackPresent=false` and there is no dispatch line.

---

# 8. Timing / behavior rules

The trace must not deliberately alter packet scheduling.

Do not:

- sleep;
- debounce;
- coalesce;
- batch rumble events separately;
- dispatch logging on a second rumble-specific queue;
- invoke the application callback asynchronously;
- reorder callback and trace events.

The existing libVIIPER owned file handler already uses bounded asynchronous file output. Use the **file-only handler** required by section 6.1 rather than synchronous direct file I/O from `HandleTransfer` and rather than the composite server logger.

Formatting a small diagnostic record on the OUT-transfer thread is acceptable for this measurement build. Do not add a larger logging subsystem.

Because the async file queue is intentionally lossy under saturation, absence of an individual trace line is not by itself proof that the corresponding USB packet was absent. Section 10/11 defines the completeness gate required before making an absence-based conclusion.

---

# 9. Required tests

Add narrow tests proving behavior, not implementation style.

At minimum:

### Trace disabled

```text
VIIPER_X360_RUMBLE_TRACE unset
valid non-zero packet
-> callback exactly once
-> values unchanged
-> no rumble-trace records
```

### Explicit STOP

```text
00 08 00 00 00 00 00 00
-> raw trace
-> recognized trace Left=0 Right=0
-> dispatch trace Left=0 Right=0
-> callback exactly once with 0/0
```

### Non-zero

```text
00 08 00 03 06 00 00 00
-> same TraceSeq across raw/parsed/dispatch
-> callback exactly once with 3/6
```

### Non-rumble EP1 OUT

Provide at least one EP1 OUT packet that does not satisfy the current `0x00 / 0x08 / len>=8` parser.

Expected:

```text
raw trace exists
parsed Recognized=false exists
callback not invoked
```

### Callback cleared while trace remains active

After `SetXbox360RumbleCallback(NULL)` while the Xbox360 device still exists:

- normal application callback behavior remains cleared;
- the device trace remains active;
- a subsequent valid rumble packet is still logged as raw + parsed;
- the parsed event records `CallbackPresent=false`;
- no dispatch event is emitted;
- no stale managed/native callback is retained.

### Device removal ends trace only after drain

On successful typed Xbox360 removal:

- callback clearing keeps its existing behavior;
- logical handle finalization may occur before the drain completes, matching current architecture;
- the trace context remains retained after handle finalization while the existing transport drain is outstanding;
- a test-controlled in-flight `HandleTransfer` that completes during the drain window can still emit its raw/parsed/dispatch trace;
- `X360RumbleTraceEnd` is emitted only after that drain has completed;
- `LastTraceSeq` includes the final transfer completed during drain;
- no rumble trace event appears after `X360RumbleTraceEnd`;
- direct teardown callback clearing through `clearDeviceCallbackLocked` does not clear the trace prematurely.

Add equivalent coverage for successful removal through `RemoveUSBBus` and `CloseUSBServer`, because both paths can finalize device handles before waiting their accumulated transport drains.

Also prove that a failed removal which leaves the logical device authoritative does not emit a premature trace-end marker.

### Trace is active before first possible auto-attached OUT

Exercise creation with `autoAttachLocalhost=true` using a test seam that can inject/observe the first host OUT as soon as attachment becomes possible.

Prove:

- trace context and `X360RumbleTraceStart` are established after canonical `BusID/DeviceID` are known;
- they are established before the auto-attach operation can expose the device to host traffic;
- the first observed EP1 OUT cannot precede trace activation.

Do not require real usbip-win2 hardware in the unit test; use the existing creation/attachment seams.

### Multiple Xbox360 devices

Create two traced Xbox360 devices.

Prove:

- each has a distinct `TraceSessionID`;
- each has its own per-session `TraceSeq`;
- every record contains the correct `BusID`, `DeviceID`, and `TraceSessionID`;
- equal sequence values from different devices cannot be mistaken for one stream.

### BusID/DeviceID reuse while the prior session drains

Use deterministic test seams to:

1. create traced Xbox360 session A;
2. begin successful removal so A's public handle is finalized but its transport drain is intentionally held open;
3. create session B on the same bus so the freed `DeviceID` is reused;
4. allow a final in-flight transfer for A during its drain;
5. generate at least one transfer for B;
6. release A's drain.

Prove:

- A and B have the same `BusID/DeviceID` but different `TraceSessionID`;
- A's late drain-window event remains attributed to A;
- B's events remain attributed to B;
- A's `TraceEnd` occurs after A's final drain-window event;
- completeness can be evaluated independently for A and B.

This test represents a real supported lifecycle property of the current DevID allocator and teardown ordering. Do not add product serialization merely to prevent the reuse.

### Creation rollback / DeviceID reuse

Exercise a known auto-attach failure that rolls back the just-created traced device.

Prove:

- Start is emitted before the attempted auto-attach;
- rollback closes that session with exactly one `X360RumbleTraceAbort`;
- no `TraceEnd` is emitted for the aborted session;
- a later creation that reuses the same `BusID/DeviceID` receives a different `TraceSessionID`;
- the two sessions cannot be merged by the log analyzer/completeness rules.

Also exercise the unsafe/unknown creation-outcome path and prove it does **not** emit a premature Abort while the retained logical device remains authoritative.

### Trace records do not invoke VIIPERLogCallback

Provide a synchronous callback observer in the test seam and a separate file-handler capture.

With rumble tracing enabled:

- ordinary existing server diagnostics may still reach the callback observer as before;
- `X360RumbleTraceStart`, raw, parsed, dispatch, and trace-end records reach only the file-only trace sink;
- no rumble trace event is forwarded through `VIIPERLogCallback`.

Do not add timing-race tests for pathological scheduler interleavings.

---

# 10. Manual measurement procedure

Build an instrumented `libVIIPER.dll` from this PR/branch.

For measurement:

```text
VIIPER_X360_RUMBLE_TRACE=1
```

must be present in the host application's environment before the process loads/arms the Xbox360 callback.

Use the separate SteamInputAddonforClaw `RumbleProbe` work order to generate deterministic XInput vibration commands.

Collect:

- `rumble-probe-*.log`
- `libVIIPER.log`
- SteamInputAddonforClaw Runtime log, or CTW measuring-build log

For one target trace session identified by the exact tuple:

```text
BusID / DeviceID / TraceSessionID
```

a run is **trace-complete enough for absence-based reasoning** only when all of the following hold:

1. exactly one `X360RumbleTraceStart` is present for the target tuple;
2. exactly one `X360RumbleTraceEnd` is present for the same tuple;
3. there is **no** `X360RumbleTraceAbort` for that tuple;
4. `TraceEnd.LastTraceSeq = N`;
5. the raw-event set contains **exactly one** `X360RumbleRaw` for every integer `TraceSeq` in the closed range `1..N`, with no gaps, duplicates, zero, or values greater than `N`;
6. for every raw event `TraceSeq=n`, there is **exactly one** `X360RumbleParsed` event with the same `BusID/DeviceID/TraceSessionID/TraceSeq`;
7. for every parsed event:
   - `Recognized=false` -> there must be no dispatch event for that sequence;
   - `Recognized=true CallbackPresent=false` -> there must be no dispatch event for that sequence;
   - `Recognized=true CallbackPresent=true` -> there must be exactly one `X360RumbleCallbackDispatch` event for that same tuple/sequence with the same Left/Right values;
8. there are no raw/parsed/dispatch events for the target tuple after `X360RumbleTraceEnd`;
9. other sessions that reuse the same `BusID/DeviceID` are ignored unless their `TraceSessionID` also matches;
10. there is no `libVIIPER logging backlog droppedLogRecords=...` marker in the relevant trace interval;
11. successful logical retirement reached the existing transport-drain completion fence before TraceEnd;
12. the existing bounded libVIIPER file flush path was given its normal opportunity to run.

If `N == 0`, the raw/parsed/dispatch set must be empty.

If **any** rule fails, the trace is incomplete and a missing individual raw trace is **inconclusive**, not evidence of upstream packet loss.

The async writer deliberately does not surface underlying file-write errors to packet-processing callers. Therefore this completeness contract detects observable queue drops and structural gaps but does not convert the logging subsystem into a correctness dependency. If the log file itself is truncated, malformed, missing the end marker, or otherwise cannot satisfy all rules above, classify the measurement as inconclusive.

Then compare ordered motor pairs and timestamps.

---

# 11. Decision table

## Probe sends 0/0, VIIPER raw trace has no corresponding 0/0

First apply the trace-completeness gate from section 10.

If the session is not proven trace-complete:

```text
result = inconclusive
```

Do not attribute the missing line to the host or transport.

Only when the target-device trace session is complete and has no observed async-log drops may the result be classified as:

```text
no corresponding packet was observed at Xbox360.HandleTransfer
-> investigate upstream of this boundary
   (Windows/XInput/XUSB/USB-IP host delivery)
```

Even then, phrase the result as a measured boundary observation, not as proof of which upstream component lost the command.

Do not modify VIIPER's parser.

## VIIPER raw trace sees 0/0, but parser says Recognized=false

```text
packet reached VIIPER but did not match current Xbox360 parser
```

Capture the exact bytes first. Any parser change requires a separate evidence-backed PR.

## VIIPER raw + parsed 0/0 exist, but no dispatch line

This is a native VIIPER callback-registration/dispatch defect candidate.

Investigate before making any semantic change.

## VIIPER dispatch 0/0 exists, managed callback log does not

The fault is after the Go device parser and at/after the typed callback bridge boundary.

Investigate the C bridge / managed interop next.

## Managed callback also receives 0/0

VIIPER is not the missing-STOP boundary for that reproduction.

Continue in application physical-output/firmware analysis.

---

# 12. Do not fix the product symptom in this PR

Explicitly out of scope:

- VIIPER-generated dead-man STOP;
- synthetic `0/0`;
- rumble retry;
- rumble dedupe;
- alternate packet acceptance;
- callback buffering;
- changing OUT transfer scheduling;
- CTW physical HID behavior;
- SteamInputAddonforClaw physical-rumble policy.

This PR answers one question only:

> What did VIIPER receive, what did the Xbox360 parser accept, and what did VIIPER dispatch to the registered typed callback?

---

# 13. Delivery

Open a focused diagnostic PR.

The PR description must state:

- behavior is unchanged;
- trace is disabled by default;
- no public ABI/header change;
- exact activation variable;
- trace records use the file-only async sink and never invoke `VIIPERLogCallback`;
- trace lifetime is independent of callback registration and remains retained through successful logical-handle finalization until the existing transport drain completes;
- exact trace event names and `BusID/DeviceID/TraceSessionID` identity fields;
- known creation rollback is closed with `X360RumbleTraceAbort`, while unsafe/unknown retained ownership is not prematurely aborted;
- TraceEnd is emitted only after the existing transport-drain fence for every successful Xbox360 retirement path;
- the exact raw/parsed/dispatch sequence-continuity rules used by the trace-completeness gate;
- the rule that a missing record is otherwise inconclusive;
- tests added;
- this instrumentation must not be interpreted as confirmation of a VIIPER defect.

Do **not** merge/adopt this diagnostic revision into SteamInputAddonforClaw's production dependency merely to perform the measurement. Use the PR/branch artifact for the diagnostic run first.
