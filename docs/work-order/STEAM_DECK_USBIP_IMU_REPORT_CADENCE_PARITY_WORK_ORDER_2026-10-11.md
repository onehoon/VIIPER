# Work Order — Steam Deck USB/IP IMU Report Cadence Parity (VIIPER Only)

**Date:** 2026-10-11  
**Repository:** `onehoon/VIIPER`  
**Source baseline inspected:** `main` at `0689d18416cf6cde9ba4fed04eb373a1a3383ddc`  
**Implementation owner:** local Codex  
**Physical Steam Input verification owner:** user, **after merge**; it is NOT a PR review, CI, or merge gate.  
**Deliverable:** one focused VIIPER implementation PR. **This document is a work order, not a code change.**

## 1. Goal and evidence

Improve the existing VIIPER **Steam Deck** virtual controller's HID input-report delivery timing to match the real Steam Deck's publicly documented **1 ms USB interrupt polling descriptor / approximately 4 ms state-report cadence**, while keeping its Valve input protocol and the Addon's contract intact.

Observed user feedback, not a laboratory-controlled timing measurement:

- The same MSI Claw + standalone Full1902 Addon sensor path produces choppy **Steam Input Gyro to Mouse** when the virtual presentation is Steam Deck.
- Switching only the Steam-facing virtual presentation to the newly supported **DualSense Edge** produces subjectively smooth/perfect gyro in the user's test.
- The Addon's previously measured physical gyro source runs at approximately 100 Hz; both virtual presentations use the same motion source and nominal 250 Hz Addon publisher.
- The Edge result makes a common physical sensor polling defect less likely; **it does not prove whether the difference is VIIPER delivery or Steam Input's device-specific processing**. Do not promise this PR will resolve Steam Input smoothing.

**Decision:** make the independently grounded Steam Deck transport corrections together in **one VIIPER PR**; do not ask the user for three separate single-knob A/B builds.

### 1.1 Public implementation references (evidence, not code dependencies)

1. [SDL Steam Deck HIDAPI implementation](https://github.com/libsdl-org/SDL/blob/main/src/joystick/hidapi/SDL_hidapi_steamdeck.c):
   - `HIDAPI_DriverSteamDeck_InitDevice` explicitly says *"Always 1kHz according to USB descriptor, but actually about 4 ms"* and sets `update_rate_us = 4000`.
   - `HIDAPI_DriverSteamDeck_HandleState` advances `sensor_timestamp_ns` by that **4 ms per received Deck report**; the Deck packet itself has no Sony-style sample timestamp.
   - The report consumer requires the Valve 64-byte report header, type `ID_CONTROLLER_DECK_STATE`, and length 64.
2. [Linux `hid-steam.c`](https://github.com/torvalds/linux/blob/master/drivers/hid/hid-steam.c):
   - Steam Deck `sensor_update_rate_us` starts at **4000** and may be refreshed from `ATTRIB_CONNECTION_INTERVAL_IN_US`.
   - `steam_do_deck_sensors_event` advances its synthetic sensor timestamp once per received sensor report; it does **not** imply 1000 actual gyro samples/second.
   - Steam Deck native IMU byte offsets and signed axis definitions are already supported by VIIPER; do not casually remap or rescale them.
3. [HIDMaestro real-device-based composite Steam Deck profile](https://github.com/hifihedgehog/HIDMaestro/blob/master/profiles/valve/steam-deck-composite.json):
   - A captured Steam Deck composite descriptor uses **`interval: 1`** on the controller interrupt IN endpoint, with **`idleFrameIntervalMs: 4`** for generated frames.
   - HIDMaestro's frame/pending-read behavior is useful as a conceptual comparison; **do not port its whole USB stack, 5-interface descriptor, CDC endpoints, or additional threads** to VIIPER.
   - HIDMaestro's particular real-unit capture and device identity are not evidence that the current successfully enumerated VIIPER device needs a broad identity/descriptor redesign.
4. [VIIPER DualSense Edge input path](../../device/dualsense/device.go) is a **local reference** for bounded latest-state notification plus cancellable interrupt reads. Edge's packet layout, timestamp, and calibration are Sony-specific and must NOT be transplanted into Valve reports.

**Important distinction:** USB interrupt `bInterval=1` means the host can poll at a 1 ms interval; it **does not require** a new 64-byte Deck report on every millisecond. A real Deck commonly reports at roughly 4 ms.

## 2. Current VIIPER code: exact reachable mismatch

Review current code again immediately before implementation; the paths below were inspected at the baseline SHA.

### 2.1 `device/steamdeck/device.go`

- The Steam Deck controller interface is number **2**, controller interrupt endpoint number **3** (`0x83` IN); it currently has `BInterval: 0x06` (6 ms). Keyboard/mouse are separate interfaces/endpoints.
- `UpdateInputState` -> `setInputState` locks, increments `d.frame`, and replaces `d.inputState` with a copy.
- The controller `HandleTransfer` immediately snapshots `d.inputState` and returns `st.buildReport(st.Frame)` for **every** incoming IN request. It does not wait for fresh state.
- The virtual Deck reports `AttributeConnectionIntervalUs = 4000` (feature response), which disagrees with the current controller endpoint `bInterval=6` and the observed physical USB descriptor.
- Controller `HandleControl` `GET_REPORT` intentionally snapshots immediately; preserve this behavior. Feature commands, Lizard mode, rumble callback, and setting responses are established contracts.
- The 64-byte report is built in [`device/steamdeck/inputstate.go`](../../device/steamdeck/inputstate.go), starting with `01 00 09 40`, a 32-bit Frame at bytes 4:8, accel bytes 24:30 and gyro bytes 30:36. These are **not** change targets.

### 2.2 `internal/server/usb/server.go`

The existing async interrupt-IN worker:

```go
interval := endpointInterval(dev.GetDescriptor(), ep)
// Each attempt:
attemptCtx, attemptCancel = context.WithTimeout(urbCtx, interval)
respData = s.processSubmit(attemptCtx, dev, ep, dir, nil, nil)
if respData == nil && errors.Is(attemptCtx.Err(), context.DeadlineExceeded) {
    // Returns lastInResp[ep] if available; otherwise retries after a short wait.
}
```

And `endpointInterval` currently computes:

```go
return time.Duration(epDesc.BInterval) * time.Millisecond
```

**This means `bInterval` is not merely a USB descriptor field in VIIPER.** It is also the internal no-data wait budget for asynchronous USB/IP IN handling, and timeout can replay `lastInResp[ep]`.

Consequences:

- Current Deck interrupt reads complete with the latest snapshot immediately, not necessarily on the 4 ms Addon publication cadence.
- **Changing `BInterval: 6 -> 1` alone is inadequate**: it also shortens the server's fallback timeout to 1 ms and can cause extra cached reports without new controller state.
- The server supports asynchronous IN, unlink, managed drain, and cancel; these must remain intact.

### 2.3 `device/dualsense/device.go`

Edge `UpdateInputState` replaces the latest state and maintains a **capacity-one notification channel**. Its controller interrupt `HandleTransfer` waits for the next state or its context deadline, then uses a current/cached snapshot. The Sony report also has its own sequence/timestamp construction.

**Take only the delivery/notification concept**; keep Steam Deck's own Frame semantics and Valve report generation.

## 3. Required implementation — one bounded PR

### 3.1 Match the real Deck's USB descriptor

In `device/steamdeck/device.go` change **only the controller interrupt IN endpoint** (`controllerEndpointNumber`) to:

```go
BInterval: 0x01, // Real Deck: 1 ms USB poll capability; sensor reports ~4 ms
```

Do **not** change the device VID/PID, interface ordering, HID report descriptor, USB/IP address, report size, the existing keyboard/mouse endpoint intervals, CDC interfaces, or board/firmware/serial identity.

Keep `AttributeConnectionIntervalUs = 4000`. That attribute describes the expected sensor/report cadence, not the USB poll descriptor.

### 3.2 Separate USB descriptor interval from the software IN wait budget

Adjust `internal/server/usb/server.go` **narrowly** so the Steam Deck controller endpoint uses a **4 ms logical report wait/fallback budget**, even while its advertised USB `bInterval` is 1 ms. Other endpoints and all other device families retain their present `endpointInterval` behavior.

**Preferred minimal seam:** an optional single-method device-specific override declared by the server at the existing interval-selection call site. For example (illustrative; adapt names to the current repository):

```go
// Local to internal/server/usb; not a generic controller abstraction.
type interruptINWaitBudget interface {
    InterruptINWaitBudget(ep uint32) time.Duration
}

func interruptINWaitDuration(dev usb.Device, ep uint32) time.Duration {
    if d, ok := dev.(interruptINWaitBudget); ok {
        if wait := d.InterruptINWaitBudget(ep); wait > 0 {
            return wait
        }
    }
    return endpointInterval(dev.GetDescriptor(), ep)
}
```

On `*steamdeck.SteamDeck`, return `4*time.Millisecond` **only for controller endpoint 3**, and zero for others to select the existing fallback behavior.

It is acceptable to choose an equivalently smaller device-local solution **if** it preserves the layering and proves the same invariant. **Do not introduce a general USB scheduler, additional transport manager, global 4 ms timer, broad device family switch, new state owner, or another worker.** The optional method is justified only by the concrete need to express two distinct existing contracts: the physical USB descriptor versus VIIPER's async-IN software deadline.

Do **not** change the existing interval/fallback semantics globally: that could regress DualSense Edge, Xbox360, and other USB/IP devices.

### 3.3 Make Deck controller IN response follow the latest state update

In `device/steamdeck/device.go`, reuse the existing snapshot lock and add **one bounded, coalesced notification mechanism** analogous to Edge's current input notification. Requirements:

- `UpdateInputState` still performs the existing deep state copy and Frame increment **once per SetState**, under the current lock. Do not use caller-owned state pointers, do not change the native `SetSteamDeckDeviceState` ABI.
- Publish a **nonblocking** latest-state notification *after* storing the new snapshot. A capacity-one signal is sufficient; drop/coalesce superseded notifications, **never queue hundreds of historical IMU frames**.
- For controller endpoint **3 only**, wait on the notification or incoming `ctx.Done()`. A successful notification returns **the latest locked snapshot** encoded as the existing Valve input report, not an old queued payload.
- On the **ordinary 4 ms deadline**, return the latest state as a valid 64-byte report (or let the existing server's proven bounded fallback return a cached report, provided it cannot report stale content after a newer stored snapshot). Choose one clearly documented behavior, not two separate retries.
- On **unlink, drain, shutdown, actual context cancellation**, return promptly without an uncancellable wait or a delayed success report. Preserve the server's existing pending-URB cleanup/join behavior.
- Preserve controller `GET_REPORT` as immediate snapshot access; don't route control transfers through the notification wait.
- Preserve the existing latest-state Frame when publishing it. Don't advance Frame just because the host polls and don't add a timestamp into Valve bytes. A repeated value from a real ~100 Hz gyro in otherwise fresh 250 Hz controller state is normal; **do not interpolate or re-filter** the IMU.
- Ensure a fresh neutral or first input state can be obtained during initial attach/first host read without indefinite blocking. A single initial neutral notification (as Edge does) or bounded request deadline is sufficient.
- Handle ordinary host behavior with more than one outstanding IN request without introducing stale state queues, runaway 1 ms cached completion, indefinite hanging reads, or out-of-order snapshots. Reuse the existing cancellation and response serialization; no speculative race state machine.

The goal is **fresh-report-biased, bounded-latency Steam Deck HID delivery**, not 1000 generated reports/second or a synthetic gyro clock.

### 3.4 Preserve the working Valve and product contracts

This PR is **VIIPER ONLY**. Do not edit the SteamInputAddonforClaw repository, its Sensor API polling, ~250 Hz managed publisher, ABI/header/DLL pin, MSI Center M ownership, Full1902 routing, HidHide, physical PID1901/1902, shutdown/restart/suspend, or physical rumble routing.

Keep unchanged:

- Valve VID `0x28DE` / PID `0x1205`, report type `0x09`, header `01 00 09 40`, 64-byte payload and byte offsets;
- `Pitch/Yaw/Roll`, accel axes, quaternion-zero/raw gyro+accel IMU mode, scalings, and `AttributeConnectionIntervalUs=4000`;
- all face/rear/Steam/QAM buttons, stick/trigger values, feature/setting commands, haptics/rumble callbacks;
- native device creation, ownership/teardown, bus, classified detach/removal results, current callback lifecycle;
- DualSense and DualSense Edge report timing/encoding/callbacks, Xbox360 and other VIIPER device models.

Do not copy Sony's sequence number, Sony's timestamp or calibration report, or Edge's accelerometer/gyro unit scales into Steam Deck.

**Separate observation (out of scope):** the current virtual Deck responds with neutral mouse/keyboard reports even after Steam disables Lizard mode, whereas HIDMaestro parks unused secondary endpoints. That is an additional possible USB/IP traffic difference, but altering Lizard-mode keyboard/mouse semantics here would expand the regression surface. Leave them unchanged in this PR unless direct code-level evidence establishes they actually interfere with the controller endpoint, and document any future investigation separately.

## 4. Required automated coverage (no physical hardware needed)

Use the existing Go test infrastructure, primarily:

- `device/steamdeck/steamdeck_test.go`
- `internal/server/usb/transport_async_in_test.go`
- `internal/server/usb/descriptor_test.go` if useful
- existing `device/dualsense` regression tests and native/ownership tests

Tests must be **deterministic wherever possible**. Use controlled channels, context deadlines and existing fake USB/IP sessions. Do not assert scheduler-perfect millisecond delivery on ordinary CI.

Minimum checks:

1. **Descriptor separation:** controller `0x83` advertises `bInterval=1`; Deck controller software IN wait budget is 4 ms; feature attribute still says 4000 us; other device endpoints retain old behavior.
2. **Notification/Frame:** after state A then B before a read, response uses B; no FIFO stale replay. The latest Valve Frame corresponds to last `UpdateInputState`, not number of reads.
3. **Bounded reading:** pending IN becomes ready when new state is stored; when no update arrives, it is released by the deadline/latest-state fallback, without busy-spin or unbounded goroutines.
4. **Control-path independence:** `GET_REPORT` still returns the immediate latest state; FEATURE_SET/GET and callback/haptics behavior remain intact.
5. **Cancellation/lifecycle:** pending read stops on cancellation/unlink/managed drain; worker is joined and no stale IN completion appears after detach. Reuse existing realistic lifecycle cases rather than inventing tiny instruction-level races.
6. **USB/IP behavior:** a realistic series of incoming IN URBs and 4 ms-ish SetState updates does not force cached duplicate responses at the **1 ms descriptor interval**. Include at least one fake transport-level scenario with a live/pending request and a fresh update, rather than testing the device in isolation only.
7. **Unchanged report compatibility:** existing Steam Deck 64-byte, button, gyro/accel, force, system key and feature-report tests still pass; other device families' tests remain green.

Run the normal repository Go test/build checks (at minimum `go test ./...` and whichever repo build/format checks already exist). If race tests are supported by the existing CI environment, use focused `go test -race` on the touched packages; do not add race-defense mechanisms solely for a hypothetical interleaving.

**No requirement to produce a physical Steam Input video, access a Claw, or prove smoothness before approval/merge.** Those are exclusively user-owned post-merge observations.

## 5. Implementation acceptance / review boundary

Accept the PR when:

- The Deck controller's **USB poll descriptor and actual state notification cadence are separately represented**: 1 ms advertised capability, 4 ms logical-report behavior.
- Ordinary host reads prefer the newest available `SetState` snapshot and do not turn `bInterval=1` into unconditional 1 kHz cached-state replies.
- Read timeout, cancellation, unlink, detach and native teardown behave correctly under normal Windows USB/IP lifecycles.
- No Valve report/feature/IMU wire incompatibility, other virtual-device regression, duplicate sensor correction, or application-level architecture change has been introduced.
- Focused and existing automated tests pass.

Do **not** block for hypothetical sub-instruction races, speculative 1000 Hz real-sensor support, perfect host scheduling, unsupported multi-session operation, or absence of physical hardware verification.

The outcome is **protocol/transport parity improvement**, not a guarantee of gyro-to-mouse smoothness. Steam's Windows Valve/Sony-specific input handling remains unobservable and could still explain a residual difference.

## 6. Developer notes / recommended PR description

Explain the three coherent elements together: (1) real Deck 1 ms poll descriptor, (2) separate 4 ms async-IN wait budget, (3) Edge-inspired coalesced latest-state wakeup **while preserving Valve report semantics**.

Explicitly say why **changing only `bInterval`** is incorrect in VIIPER's current USB/IP server: `endpointInterval` doubles as the software timeout and triggers cached report reuse. Prefer a very small implementation with one clear source of state and the existing server lifecycle. No new USB/IP pacing subsystem, timers or compatibility wrappers.

Keep the user comparison concise: *"The same Addon source felt smooth as DualSense Edge but choppy as Steam Deck; this PR aligns Deck virtual HID timing with the real Deck references. Final subjective gyro quality will be checked separately after merge."*
