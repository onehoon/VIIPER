# Work Order — DualSense / DualSense Edge Rumble Stop Compatibility

Date: 2026-10-21  
Repository: onehoon/VIIPER  
Target: main (implementation via one focused PR; this document is the instruction only)  
Baseline reviewed: 6e952500165673c2c27e1b4723aab49e81133e2c

## Objective and observed behavior

Fix the VIIPER fork's **rumble-only DualSense / DualSense Edge output callback** so a host-generated zero-strength *neutral rumble termination report* can stop a previously forwarded nonzero motor command instead of leaving the physical MSI Claw motors active until the Addon's five-second safety timer expires.

Observed on 2026-10-21: Ghost of Tsushima Director's Cut produces appropriately brief vibration with the Steam Controller presentation but anomalously long vibration with the virtual DualSense Edge presentation on the same MSI Claw. The Addon log shows last nonzero physical motor writes followed roughly five seconds later by zero output. This is consistent with (but does not alone prove) expiration of `DualSenseEdgeRumbleFeedbackAdapter.DefaultSafetyStop = 5s`.

This is a **targeted compatibility correction**, **not** a request to emulate native DualSense USB-audio haptics, adaptive triggers, LED output, or to shorten the Addon's safety timer. Do not add mandatory packet-tracing logs as a prerequisite for this deterministic parser/test correction.

## Code audit: current behavior and missing case

1. `device/dualsense/device.go`: `HandleTransfer` (interrupt OUT, endpoint 3) and `HandleControl` (HID SET_REPORT) both call `parseRumbleOutputReport`.
2. `parseRumbleOutputReport` accepts a report of at least 48 bytes with `ReportIDOutput` and **only** forwards it if `out[1]&0x01 != 0 || out[39]&0x04 != 0`.
3. `out[3]` is the small/right motor, and `out[4]` is the large/left motor. The current offsets and v1/v2 update-flag bit positions are correct.
4. A flagged `0/0` explicitly stops both motors and is already covered by tests. **Missing case:** after a forwarded nonzero command, an unflagged, zero-motor, zero-control-bit report is dropped. The consumer therefore cannot clear the physical motor until its independent timeout.
5. `device/dualsense/output_report_test.go` tests valid v1/v2 motor updates, flagged zero stops, LED-only filtering, and callbacks across rearm, but not a preceding nonzero command followed by an unflagged *neutral termination* report.
6. `lib/viiper/dualsense.go` exposes a stable six-argument output callback and currently forwards the parsed two motor bytes (legacy LED arguments are zero). Keep that ABI untouched. `docs/libviiper/fork-api.md` explicitly documents the current rumble-only contract.
7. The Addon's consumer (`onehoon/SteamAddonforClaw/src/SteamInputAddonforClaw/Feedback/PresentationRumbleFeedback.cs`) maps those bytes directly to the physical sink and schedules a five-second dead-man stop. There is no requirement to modify that separate repository for this PR.

## Verified external references and limits

- **SDL3, `src/joystick/hidapi/SDL_hidapi_ps5.c`**, `HIDAPI_DriverPS5_UpdateEffects` and `HIDAPI_DriverPS5_RumbleJoystick`: builds an initially zeroed effect state; a **nonzero** rumble enables either legacy `0x01` or improved `flag2 0x04`, with haptics-select `0x02`. When both stored rumble magnitudes are zero, SDL deliberately leaves the emulated-rumble enable bits **off** to return to audio haptics. A rumble stop update can therefore be represented by a valid USB report containing all-zero flags and all-zero motor bytes. See https://github.com/libsdl-org/SDL/blob/main/src/joystick/hidapi/SDL_hidapi_ps5.c#L706-L746 and https://github.com/libsdl-org/SDL/blob/main/src/joystick/hidapi/SDL_hidapi_ps5.c#L1011-L1026.
- **Linux hid-playstation**, flag definitions: `flag0 0x01` compatible rumble, `flag0 0x02` haptics select, `flag2 0x04` improved rumble; lightbar uses a *separate* `flag1 0x04`. The unflagged motor data alone does **not** constitute a protocol-mandated STOP. See https://github.com/torvalds/linux/blob/master/drivers/hid/hid-playstation.c#L153-L170.
- **HHD**, `src/hhd/controller/virtual/dualsense/__init__.py`: preserves `self.rumble` and emits an explicit zero force-feedback event when a following report lacks either classic-rumble flag. This demonstrates a stateful compatibility policy, **not** a normative Sony interpretation; it can also stop on an unrelated LED-only report. See https://github.com/hhd-dev/hhd/blob/master/src/hhd/controller/virtual/dualsense/__init__.py#L325-L358.
- **InputPlumber**, `src/input/target/dualsense.rs`: distinguishes 48-byte short and 63-byte full USB output reports and implements an explicit Steam Input compatibility workaround for selected short reports lacking rumble enable bits. Do **not** port its flag injection wholesale; it is a separate policy with different hardware transport. See https://github.com/ShadowBlip/InputPlumber/blob/main/src/input/target/dualsense.rs#L663-L702.
- **VIIPER upstream**, `device/dualsense/device.go`, `parseOutputReport`: unconditionally forwards motor bytes for each accepted report, ignoring validity when deciding whether to emit a motor callback. This can clear latched rumble but can also spuriously stop it on LED-only writes; **do not revert to upstream's unconditional behavior**. See https://github.com/Alia5/VIIPER/blob/main/device/dualsense/device.go#L269-L286.
- **DualSenseClient**, `ValidFlags.cs`: validity bits mark which output fields should be applied; a zero-valued field with no corresponding valid bit should not automatically overwrite hardware state. See https://github.com/DualSenseClient/DualSenseClient/blob/main/source/DualSenseClient.Controllers/DualSense/Output/ValidFlags.cs.
- **Legion Go ROGueENEMY port**, `virt_ds5.c`, has additional haptics-select gating and is **not** the preferred model for this fix: https://github.com/corando98/ROGueENEMY/blob/master/virt_ds5.c.

Crucial distinction: the proposed extra STOP is a **stateful two-motor bridge compatibility behavior** to support an observed SDL-style neutral termination pattern. It is **not** a claim that an all-zero HID packet is universally a Sony-defined STOP. Ghost of Tsushima's exact incoming report sequence has not yet been captured.

## Required implementation: one small PR

**Files in scope:** `device/dualsense/device.go`, `device/dualsense/output_report_test.go`, and the existing API contract document `docs/libviiper/fork-api.md` (plus `docs/devices/dualsense.md` only if needed for consistent documentation).

1. Define named, local bit constants for the three relevant fields to remove magic numbers: valid flag0 compatible vibration (`0x01`), flag0 haptics select (`0x02`), valid flag2 compatible vibration2 (`0x04`). Keep report offsets stable; do not undertake a general protocol rewrite.
2. Keep the existing handling of flagged v1/v2 updates **identical**, including nonzero updates, flagged `0/0` STOPs, motor order, returned callback struct, and the lack of LED emission.
3. Add the smallest possible **per-device/per-armed-callback nonzero-rumble state**. Update it only for actual emitted motor callbacks. Reset it on callback clearing/replacement and new device creation, to avoid treating a later session's neutral packet as an old session's STOP. Prefer a tiny helper shared by interrupt OUT and HID SET_REPORT, not two diverging copies of state logic.
4. Add a conservative *neutral termination* compatibility path: if (a) a nonzero command was previously forwarded in this active callback session, (b) both raw motor bytes are exactly zero, (c) neither v1 nor v2 rumble-valid bit is set, and (d) **all three valid-flag bytes are exactly zero** (`out[1] == 0 && out[2] == 0 && out[39] == 0`), emit **one** zero-strength `OutputState` to the existing callback and clear active state. Subsequent identical neutral packets when already stopped must not emit additional callbacks.
5. Preserve the existing **ignore** behavior for reports carrying non-rumble feature flags when there is no valid classic rumble update. In particular, LED-only (`flag1 0x04`), player LEDs (`flag1 0x10`), trigger FFB-only (`flag0 0x04/0x08`), haptics-select-only (`flag0 0x02`), and no-rumble reports with **nonzero** arbitrary motor bytes must not cause unsolicited motor changes. This conservative filter deliberately does **not** make HHD's broader "any unflagged report stops rumble" assumption.
6. Keep all accepted original report lengths/paths intact (`len >= 48`, including full USB output reports); malformed/short/wrong report ID must not mutate rumble state. Keep USB/IP OUT acknowledgement/control transfer behavior intact.
7. Keep the existing callback mutex and callback lifecycle guarantees. Ensure state transitions do not inadvertently invoke callbacks while holding a lock that allows callback reentrancy/deadlock. No new background workers, native exports, ABI field additions, transport changes, or logging flood.
8. Clarify the fork API docs: valid v1/v2 `0/0` is an explicit STOP; an **all-flags-zero/all-motors-zero** report may emit a one-shot compatibility STOP **only after a forwarded nonzero rumble**; unrelated feature-only reports remain ignored. Mark this explicitly as fork compatibility behavior rather than Sony protocol truth.

### Decision matrix (mandatory test contract)

| Previous forwarded state | `flag0` | `flag1` | `flag2` | motor small/large | Expected callback |
| --- | --- | --- | --- | --- | --- |
| stopped | `00` | `00` | `00` | `00/00` | none |
| stopped | `01` | `00` | `00` | `12/34` | nonzero 12/34 |
| active | `00` | `00` | `00` | `00/00` | **one STOP** |
| stopped after STOP | `00` | `00` | `00` | `00/00` | none |
| active | `01` | `00` | `00` | `00/00` | flagged STOP (existing) |
| active | `00` | `00` | `04` | `00/00` | flagged STOP (existing) |
| active | `00` | `04` | `00` | `00/00` | none (LED only) |
| active | `00` | `10` | `00` | `00/00` | none (player LED only) |
| active | `04` | `00` | `00` | `00/00` | none (trigger only) |
| active | `02` | `00` | `00` | `00/00` | none (haptics select only) |
| active | `00` | `00` | `00` | `12/34` | none (unflagged motor bytes) |
| active | `00` | `04` | `00` | `12/34` | none (stale motor bytes on LED update) |
| stopped | `00` | `00` | `04` | `56/78` | nonzero 56/78 |
| active | `00` | `00` | `04` | `00/00` | flagged STOP |

This matrix is a deliberate *conservative first repair*. If the actual game requires stopping on combined LED + neutral reports, do **not** silently broaden the semantics: first report the newly demonstrated ambiguity and propose a separately tested policy.

## Test changes

Extend existing `device/dualsense/output_report_test.go` with clear, independent sequences:

- **SDL-like v1 and v2:** nonzero START -> unflagged all-zero neutral STOP -> repeated neutral ignored -> new nonzero START works.
- **Both transports:** execute the same test matrix through `HandleTransfer` interrupt OUT and `HandleControl` HID SET_REPORT.
- **Both virtual models:** `New(nil)` and `NewEdge(nil)`; ensure behavior is identical.
- **Feature-only isolation:** nonzero START -> LED/player-LED/trigger-only/haptics-select-only writes (with zero or stale nonzero motor bytes) -> no STOP -> neutral all-zero report -> one STOP.
- **ABI/callback lifecycle:** nonzero -> `SetOutputCallback(nil)` -> next session callback registration -> unflagged neutral must not replay old STOP; new START -> new neutral -> exactly one STOP. Two device instances must not share active state.
- **Malformed:** length 47, wrong report ID, wrong OUT endpoint or direction; no callback and no erroneous state transitions. Also exercise 64-byte input length when accepted by existing path.
- **Existing cases:** all current v1/v2 and explicit-zero tests, LED ignored tests, callback rearm tests continue passing.

Test commands: `go test ./device/dualsense`, `go test -race ./device/dualsense` where supported, `go test ./...`, and `just test` if available in the developer environment. Confirm build/API compatibility of `lib/viiper` without changing exported headers.

## Acceptance criteria and non-goals

- No additional five-second latched vibration **for the SDL-style neutral termination sequence**; unit tests prove delivery of STOP before Addon dead-man timeout would be needed.
- No extra STOP from unrelated LED, player-LED, trigger-only, or haptics-select-only report as specified above.
- Shared parser/classification behavior on interrupt OUT and HID SET_REPORT; ordinary valid rumble callbacks unchanged.
- No new exported API, generated header changes, USB audio endpoint/device interfaces, adaptive-trigger emulation, physical rumble scaling adjustments, or changes to Steam Deck / Xbox360 implementations.
- No changes in `onehoon/SteamAddonforClaw` as part of this PR. User will independently rebuild/re-pin VIIPER and conduct the Ghost of Tsushima manual A/B test.
- Describe precisely in the PR what is **proven by deterministic source/tests** versus what still needs **actual game validation**. Do not claim the exact Ghost of Tsushima output bytes were captured or that this test proves all DualSense haptic compatibility.

## Implementation review checklist

1. State lifetime follows the currently registered callback/session, not a global singleton.
2. A callback of `OutputState{}` means stopped (not LED control); motor mapping is unchanged.
3. Only one emitted compatibility STOP per active-to-stopped transition.
4. The event classifier handles one incoming report exactly once; both host ingress routes use it.
5. Respect preexisting callback reentrancy/lifetime contracts; avoid nested callback invocation under the new state lock.
6. Keep modifications small and focused. No speculative generic HID engine, packet-cache subsystem, new diagnostics pipeline, or timers.

## Source links

- Fork code: https://github.com/onehoon/VIIPER/blob/6e952500165673c2c27e1b4723aab49e81133e2c/device/dualsense/device.go
- Fork tests: https://github.com/onehoon/VIIPER/blob/6e952500165673c2c27e1b4723aab49e81133e2c/device/dualsense/output_report_test.go
- Fork API: https://github.com/onehoon/VIIPER/blob/6e952500165673c2c27e1b4723aab49e81133e2c/docs/libviiper/fork-api.md
- Addon adapter: https://github.com/onehoon/SteamAddonforClaw/blob/main/src/SteamInputAddonforClaw/Feedback/PresentationRumbleFeedback.cs
