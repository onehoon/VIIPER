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

Install it when `CreateXbox360Device` creates the typed logical device and:

```text
VIIPER_X360_RUMBLE_TRACE == 1
```

was observed at creation.

The trace context contains only diagnostic state needed for this investigation:

```text
file-only logger
BusID
DeviceID
per-device TraceSeq
enabled flag
```

Use the canonical logical identity already owned by the wrapper:

```text
BusID
DeviceID
```

in every trace event. Do not use only `TraceSeq` as identity.

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

The trace context ends only when the typed Xbox360 logical device is actually removed/finalized.

Keep all callback teardown behavior unchanged.

## 6.4 Trace session completeness markers

When trace is enabled for a device, emit:

```text
Event=X360RumbleTraceStart
BusID=<bus>
DeviceID=<device>
TraceSeq=0
```

after the trace identity is established.

Before the logical Xbox360 device is finalized, emit:

```text
Event=X360RumbleTraceEnd
BusID=<bus>
DeviceID=<device>
LastTraceSeq=<n>
```

through the same file-only async sink.

The existing async writer can report queue saturation as:

```text
libVIIPER logging backlog droppedLogRecords=<n>
```

The trace-end marker is part of the evidence contract: if it is absent, the trace session is incomplete and absence-based conclusions are not allowed.

Do not make logging completeness a controller-lifecycle success criterion. A missing trace-end record remains diagnostic-only and must never block device removal or server close.

---

# 7. Required trace points

Use a per-Xbox360-device monotonic diagnostic sequence number so lines from one host OUT transfer can be correlated.

The counter is diagnostic-only.

Every rumble trace event must also include:

```text
BusID=<canonical bus id>
DeviceID=<canonical logical device id>
```

because multiple Xbox360 typed devices may write to the same `libVIIPER.log`. `TraceSeq` alone is not globally unique.

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

### Device removal ends trace

On typed Xbox360 removal/finalization:

- callback clearing keeps its existing behavior;
- one trace-end marker is attempted before finalization;
- trace state is no longer retained after the device is finalized;
- direct teardown callback clearing through `clearDeviceCallbackLocked` does not accidentally clear the trace before the logical device is removed.

### Multiple Xbox360 devices

Create two traced Xbox360 devices.

Prove:

- each has its own per-device `TraceSeq`;
- every record contains the correct `BusID` and `DeviceID`;
- equal sequence values from different devices cannot be mistaken for one stream.

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

For the target `BusID/DeviceID`, a run is **trace-complete enough for absence-based reasoning** only when all of the following hold:

1. `X360RumbleTraceStart` is present;
2. `X360RumbleTraceEnd` is present for the same `BusID/DeviceID`;
3. the sequence range is internally consistent for the captured events;
4. there is no `libVIIPER logging backlog droppedLogRecords=...` marker between the relevant start/end interval;
5. shutdown/removal completed normally enough for the existing bounded libVIIPER file flush path to run.

If any of these conditions is missing, a missing individual raw trace is **inconclusive**, not evidence of upstream packet loss.

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
- trace lifetime is Xbox360-device lifetime, independent of callback registration;
- exact trace event names and `BusID/DeviceID` identity fields;
- the trace-completeness gate and the rule that a missing record is otherwise inconclusive;
- tests added;
- this instrumentation must not be interpreted as confirmation of a VIIPER defect.

Do **not** merge/adopt this diagnostic revision into SteamInputAddonforClaw's production dependency merely to perform the measurement. Use the PR/branch artifact for the diagnostic run first.
