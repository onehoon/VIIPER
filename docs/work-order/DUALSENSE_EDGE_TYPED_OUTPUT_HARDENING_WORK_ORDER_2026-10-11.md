# Work Order — DualSense Edge Typed Output Hardening: Rumble Validity, Descriptor Isolation, and Focused Tests

**Date:** 2026-10-11  
**Status:** Ready for local Codex implementation  
**Target repository:** `onehoon/VIIPER`  
**Baseline reviewed:** `main` at `8902b5308a58e2fc86fb8fda20eed7df8d8ff664` (re-check current HEAD before editing)  
**Upstream reference:** `Alia5/VIIPER` at `0c6c50d353abb7129f5b163087fa72f6a2985f99` (2026-10-11)  
**Delivery:** One small VIIPER-only PR. Local Codex implements code and automated tests; user performs any physical Windows/Steam Input validation **after merge**. Hardware tests are neither implementation prerequisites nor review/CI/merge blockers.

---

## 1. Context and product decision

The standalone Steam Addon for Claw Full1902 design will optionally offer a **virtual DualSense Edge** in Steam/BPM instead of the existing virtual Steam Deck. Xbox360 remains the normal Windows virtual controller.

**Addon architecture:** https://github.com/onehoon/SteamAddonforClaw/blob/main/docs/DUALSENSE_EDGE_PRESENTATION_ARCHITECTURE_2026-10-11.md

This work order is **only** for the VIIPER fork. Do not begin Addon C# interop, UI/settings, mappings, motion reader integration, PRs, dependency adoption, or hardware verification here.

The current fork already has the required Typed API family:
- `CreateDualSenseEdgeDevice`;
- `SetDualSenseDeviceState`;
- `SetDualSenseOutputCallback`;
- `RemoveDualSenseDevice` and classified `RemoveDualSenseDeviceEx`;
- common `GetUSBDeviceIdentity`, `GetUSBDeviceAttachmentState`, `AttachUSBDeviceEx`, `DetachUSBDeviceEx`.

These are present in `lib/viiper/dualsense.go`, `docs/libviiper/fork-api.md`, and the **pinned** Addon generated `libVIIPER.h`. Do **not** reimplement them, introduce Edge-only handles/ownership, create a second USB server/bus, or backport upstream's older unclassified removal lifecycle.

Comparing fork and upstream `device/dualsense/device.go` found no missing Edge HID/report fields; the fork adds callback synchronization. The actual requested cleanup is narrowly scoped:

1. Make classic-rumble state updates respect the USB DualSense **output validity flags** (shared DualSense + Edge behavior).
2. Eliminate the shared mutable descriptor product-string map between normal DualSense and Edge.
3. Add focused, hardware-free regression tests, including the relevant Edge Typed path.

**Goal:** Make the existing fork an appropriately reliable provider for the Addon's later Edge integration without changing its native ABI or established lifecycle.

---

## 2. Required source reading

Read the current source **before writing any code**:

| File | What to inspect |
| --- | --- |
| `device/dualsense/const.go` | USB report IDs, button definitions, Edge VID/PID, report lengths |
| `device/dualsense/device.go` | `New` / `NewEdge`, descriptor construction, `HandleTransfer`, `HandleControl`, `parseOutputReport`, callback invocation |
| `device/dualsense/state.go` | `InputState`, `OutputState`, input serialization and neutral defaults |
| `device/dualsense/descriptor.go` | `defaultDescriptor`, its map and other referenced fields |
| `device/dualsense/callback_lifecycle_test.go` | Existing callback behavior, including callback reentry/clear |
| `lib/viiper/dualsense.go` | Existing Typed creation/state/callback/Ex removal; Edge uses common `*dualsense.DualSense` |
| `lib/viiper/classified_removal_parity_test.go` | Existing Edge classified-removal test and enum contract |
| `lib/viiper/ownership_invariants_test.go` | Existing nil output pointer and Edge creation safeguards |
| `lib/viiper/attachment_state_query_test.go` | Existing family coverage; add a focused Edge case if useful |
| `.github/workflows/build_base.yml` | Existing test and x64 canonical export / header projection workflow |

External *reference for report semantics*, not a dependency:
- [Linux `hid-playstation.c` DualSense report definitions](https://github.com/torvalds/linux/blob/master/drivers/hid/hid-playstation.c): `dualsense_output_report_common`, `DS_OUTPUT_VALID_FLAG0_COMPATIBLE_VIBRATION`, `DS_OUTPUT_VALID_FLAG2_COMPATIBLE_VIBRATION2`, `DS_OUTPUT_VALID_FLAG0_HAPTICS_SELECT`, and LED-only update logic.
- [Upstream VIIPER DualSense](https://github.com/Alia5/VIIPER/blob/main/device/dualsense/device.go) for parity. **Do not simply replace fork files with upstream versions**: fork ownership/callback safety deliberately differs.

When assessing lifecycle risks, follow the actual Full1902 product scope: one user, one interactive session, no RDP/Fast User Switching, and actual sleep/restart/PnP/fail-close/teardown safety. Do not add speculative locks/state machines for pathological interleavings.

---

## 3. Finding A — Output validity flags currently ignored for motors

### 3.1 Current concrete path

`device/dualsense/device.go` currently parses every accepted report as:

~~~go
func parseOutputReport(out []byte) OutputState {
    feedback := OutputState{
        RumbleSmall: out[3],
        RumbleLarge: out[4],
    }
    // The function already inspects out[2] to recognize LED updates.
    // ...
    return feedback
}
~~~

The two real entry points use it:
- `HandleTransfer`, `usbip.DirOut`, endpoint 3, Report ID `0x02`, length >= 48;
- `HandleControl`, HID `SET_REPORT` output report `0x02`, length >= 48.

The Sony USB output report's motor bytes **must not be interpreted as updates merely because a valid output packet arrived**.

Example reachable failure:
1. Host sends an enabled rumble request with nonzero motor bytes.
2. Host later sends a lightbar/player LED-only update with zero or incidental motor bytes and **no rumble-valid flag**.
3. Existing parser supplies zero/incidental motor values to the VIIPER callback; a future Addon rumble bridge would apply a spurious motor change, potentially a false STOP.

This is a correctness issue shared with upstream. It is relevant to the specifically planned basic two-motor Edge feedback support.

### 3.2 Verified USB report layout

For the VIIPER-supported **USB** report (not Bluetooth `0x31`):

| Index relative to complete USB report | Field | Relevant bits |
| --- | --- | --- |
| `out[0]` | Report ID | `0x02` |
| `out[1]` | `valid_flag0` | bit 0 (`0x01`) = compatible vibration; bit 1 (`0x02`) = haptics select |
| `out[2]` | `valid_flag1` | bit 2 (`0x04`) = lightbar; bit 4 (`0x10`) = player LEDs |
| `out[3]` | `motor_right` | USB small/right motor value |
| `out[4]` | `motor_left` | USB large/left motor value |
| `out[39]` | `valid_flag2` | bit 2 (`0x04`) = compatible vibration v2 |
| `out[44]` | player LEDs | Existing parser location |
| `out[45:48]` | RGB lightbar | Existing parser location |

Source: Linux DualSense `dualsense_output_report_usb` / `dualsense_output_report_common`. Verify layout once against current upstream and VIIPER descriptor before coding. `HAPTICS_SELECT` by itself does not indicate a new pair of classic motor intensities.

A **motor update** occurs when either:
- `out[1] & 0x01 != 0` (**compatible vibration v1**), OR
- `out[39] & 0x04 != 0` (**compatible vibration v2**).

The v2 path matters for current DualSense/Edge compatible-rumble senders. Explicit enabled `0,0` is a genuine STOP and must be applied; absent rumble-valid flags are **not** a STOP.

### 3.3 Required implementation

Keep the existing **exported** `DSOutputCallback` signature and `OutputState` wire structure unchanged. Keep the existing LED parsing behavior; do not introduce a new callback version or add validity flags to the exported C ABI.

Implement the smallest **per-device last-valid classic motor state**:
- The current accepted output-report parser must identify whether the report **actually updates rumble** using the two flags above.
- On a rumble-valid report, update the device's cached small/large motor values, including explicit zeros.
- On an LED-only or otherwise non-rumble-valid report, **do not overwrite** those cached motor values with unused packet bytes.
- Continue passing a coherent `OutputState` to the already registered callback, preserving the current callback-on-accepted-report contract (including LED events). LED-only callbacks should report the last valid motor state, **not invented zero**.
- Both interrupt OUT and HID `SET_REPORT` must share **one** extraction/update behavior; do not duplicate report interpretation in two places.
- A newly created device's cached motor state starts at stopped (`0,0`); no persistence or cross-device global state.
- Preserve the current callback lock's reentry behavior: never invoke client callback while holding a newly introduced state lock or the existing callback-selection lock.
- Use existing `DualSense.mtx` if suitable for the tiny mutable cache, or a comparably minimal locally owned field; no new manager, output worker, callback layer, atomic state machine, or async queue.

Illustrative approach (**not** a required exact patch):

~~~go
// A single private per-device cache, initially (0,0).
// Keep this update serialized without holding the lock across callback invocation.
func (d *DualSense) decodeOutputReport(out []byte) OutputState {
    d.mtx.Lock()
    defer d.mtx.Unlock()

    if out[1]&0x01 != 0 || out[39]&0x04 != 0 {
        d.lastRumbleSmall = out[3]
        d.lastRumbleLarge = out[4]
    }

    result := OutputState{
        RumbleSmall: d.lastRumbleSmall,
        RumbleLarge: d.lastRumbleLarge,
    }
    // Keep the existing flag-gated LED/player-LED parsing semantics.
    // ...
    return result
}
~~~

It is acceptable to keep `parseOutputReport` as a separate helper with previous-state arguments instead, if that yields a smaller change. Do not implement unconditional callback suppression for non-rumble packets, because `DSOutputCallback` also exposes LEDs and the existing contract delivers accepted reports. Consumers that need to distinguish a *new* rumble command from a state snapshot would require separate future ABI design; that is **outside this PR**.

**Bounded policy:** Fix the proven false motor reset without expanding scope to all PlayStation haptics. Preserve ordinary two-motor feedback; no adaptive triggers, advanced HD haptic emulation, speaker/audio, new LED behavior or DualSense-specific Windows driver changes.

---

## 4. Finding B — Edge changes the shared default product-string map

Current constructor:

~~~go
d := &DualSense{
    descriptor: defaultDescriptor,
    metaState: metaState,
}
// ...
if edge {
    d.descriptor.Device.IDProduct = DefaultPIDDSEdge
    d.descriptor.Strings[2] = "DualSense Edge Wireless Controller"
}
~~~

Go copies the descriptor **struct**, but its `Strings` map remains aliased to `defaultDescriptor.Strings`. Thus after one `NewEdge(nil)`, a subsequently created ordinary `New(nil)` can return the Edge product-name string even while exposing normal PID `0x0CE6`.

**Required small fix:** Clone only the mutable `Strings` map when creating the device (e.g., `maps.Clone(defaultDescriptor.Strings)`) before changing the Edge product string. Avoid a broad descriptor refactor/deep-copy framework.

Tests must confirm:
- `New(nil)` still exposes normal VID/PID and ordinary product label;
- `NewEdge(nil)` exposes Sony `0x054C:0x0DF2` and Edge product label;
- after Edge creation, a newly created normal DualSense still has normal label;
- an Edge instance retains Edge label even if other devices are constructed later.

The scope includes this cheap concrete correctness fix; it does **not** require broad USB descriptor redesign.

---

## 5. Focused automated test requirements

Use existing package tests, not a new test harness.

### 5.1 Classic rumble validity

Add deterministic table-driven tests (e.g. `device/dualsense/output_report_test.go`). Use legal USB report `0x02`, length 48 or 64 as accepted by current code, and vary the flag bytes explicitly.

| Sequence / packet | Expected last-known motor result |
| --- | --- |
| Fresh device, LED-only (`flag1 = 0x04`, no rumble-valid flags, arbitrary motor bytes) | `0,0` |
| V1 valid `flag0 = 0x01`, motors `0x11,0x22` | `0x11,0x22` |
| Following LED-only, motor bytes `0,0` | Retain `0x11,0x22` |
| Following LED-only, motor bytes `0x7f,0xfe` | Retain `0x11,0x22` |
| V2 valid `flag2 = 0x04`, motors `0x33,0x44` | `0x33,0x44` |
| `flag0 = 0x02` (haptics select **only**), unused motor bytes | Retain previous motors |
| V1 or V2 valid motors `0,0` | Explicitly stop both |
| LED and rumble simultaneously valid | Update motors **and** preserve existing RGB/player parsing |
| `valid_flag0 = 0`, `valid_flag2 = 0`, zero motor bytes after prior rumble | Retain previous motors |

Run the behavioral assertions through **both** `HandleTransfer` and `HandleControl` with a captured `SetOutputCallback` consumer. Show that both paths observe the same per-device state. Test separate device instances have independent cached motor state. Keep the existing reentrant callback-clear tests passing.

### 5.2 Edge HID/state regression

Add a few small checks using `NewEdge(nil)`, current `UpdateInputState`, and `buildUSBInputReport` or the normal USB IN handler:

- Edge VID/PID = Sony `0x054C:0x0DF2`;
- USB input Report ID = `0x01`, total report size = `64`;
- `ButtonEdgeL4 = 0x00400000` becomes input-report `b[10]` bit 6;
- `ButtonEdgeR4 = 0x00800000` becomes input-report `b[10]` bit 7;
- both paddles can be independent and simultaneous;
- PS `0x00010000` and Cross `0x00000020` survive the report mapping, including the PS+Cross chord;
- gyro and accel signed values survive little-endian placement in `b[16:28]` without swapped signs or truncation;
- negative/neutral stick and D-Pad values retain existing encoding.

These tests establish the **VIIPER report contract** only. They cannot prove Windows/Steam Input enumerates Edge or recognizes paddles, PS+Cross, IMU, Game Bar input or physical rumble. Do not invent hardware tests or external controller dependencies.

### 5.3 Typed/native lifecycle regression

Existing `lib/viiper/classified_removal_parity_test.go` already includes `TestDualSenseEdgeUsesClassifiedDualSenseRemoval`. Existing `ownership_invariants_test.go` rejects `CreateDualSenseEdgeDevice` with a nil output pointer. Do not duplicate those tests mechanically.

If small, extend the current `TestGetUSBDeviceAttachmentStateAcceptsTypedFamilies` table in `lib/viiper/attachment_state_query_test.go` with `dualsense.NewEdge(nil)` so Edge is covered for the shared detached -> attached query path. Reuse its existing mocked backend, not real USBIP/PnP. Retain existing classified unknown-outcome/fail-close tests.

### 5.4 CI commands

~~~bash
go test ./device/dualsense ./lib/viiper
go test -race ./device/dualsense ./lib/viiper
go test ./...
~~~

Ensure `.github/workflows/build_base.yml`'s **focused test** and **focused race test** steps include `./device/dualsense`; the existing `just test-coverage` runs `./...` but focused canonical coverage should be explicit. Do not remove its existing Steam Controller, Steam Deck, USBIP or virtual bus targets.

Since this PR changes no C ABI, the normal canonical libVIIPER build/header/export/manifest checks should continue passing without changing output exports.

---

## 6. Non-goals and change boundaries

**No change** to:
- public `CreateDualSense[Edge]Device`, `SetDualSenseDeviceState`, `SetDualSenseOutputCallback`, `RemoveDualSenseDeviceEx` signatures or enum numbers;
- `DSDeviceState`, `DSOutputCallback` ABI layout;
- USBIP transport, `virtualbus`, generic attachment/teardown or typed handle ownership;
- Xbox360/SteamDeck/SteamController/DS4 devices and their native feedback;
- generated native exports, Windows artifacts or Addon's pinned DLL;
- DualSense report format, paddle bit locations, accelerometer/gyro scales or descriptor HID usages;
- Steam/Windows policy, QAM mapping, Windows Game Bar, Addon settings;
- PlayStation advanced haptics, adaptive triggers, audio, speaker/microphone, Bluetooth stack, RGB or custom LED functionality beyond retaining existing callback semantics.

Do not add future-proof compatibility managers, separate output state machines, extra locks for highly implausible instruction-level races or a generic multi-controller framework. The known device cache is only for meaningful host report semantics.

---

## 7. Code review / merge acceptance

Merge readiness requires:
1. V1 **and V2** ordinary compatible-rumble flags are decoded correctly.
2. LED-only packets cannot falsely clear prior motor state, and a flagged `0,0` request **does** stop.
3. USB interrupt OUT and HID control output use the same logic.
4. Existing C callback semantics, LED fields and callback clear/reentry behavior remain usable.
5. DualSense and Edge descriptor identities no longer share a mutable product-name map.
6. Focused report tests cover Edge paddles/PS/IMU and normal/Edge constructors.
7. Existing Typed lifecycle, classified-removal safety, Go tests/race tests, build/export checks pass.
8. The implementation does not introduce an extra native lifecycle manager or disturb existing Xbox360/SteamDeck behavior.

**PR policy:** Review only realistic user-impacting defects. Do not block for theoretical races or unsupported environments. Local Codex is responsible for the source and automated tests. Physical device/gamepad/Steam Input/Game Bar verification is **user-only after merge**, and is never mandatory for code completion or PR merge.

---

## 8. Deliverable / handoff

One narrowly scoped PR in **onehoon/VIIPER**, with:
- actual implementation in `device/dualsense/device.go`;
- new or extended `device/dualsense/*_test.go`;
- optional one small `lib/viiper/attachment_state_query_test.go` table extension;
- one focused change to `.github/workflows/build_base.yml`;
- a short PR description specifying source-backed v1/v2 validity behavior, shared descriptor map correction, test commands and explicit unchanged native ABI.

**Stop after the VIIPER PR.** A separate future Addon PR will adopt the resulting pinned DLL/header through its existing verified provenance process and implement the requested Developer Menu Edge presentation. Do not touch the Addon in this work order.
