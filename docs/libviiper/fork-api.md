# Fork-specific libVIIPER API

This page documents the canonical embedded API used by this fork. It is
intended for applications that embed `libVIIPER.dll` directly, especially
SteamInputAddonforClaw and applications derived from it.

The canonical API is generated from `lib/viiper`. Do not use the repository
root [`libviiper.h`](../../libviiper.h) as the canonical header; that file is
the legacy flat compatibility API. Build the shared library to generate the
matching header under `dist/libVIIPER/`.

## Support matrix

| Capability | Windows | Linux/macOS |
| --- | --- | --- |
| Typed virtual device creation | Supported | Compile-compatible; validate the target USB/IP client separately |
| Typed device state and callbacks | Supported | Compile-compatible; runtime support depends on the USB/IP client |
| Tracked localhost `AttachUSBDevice` / `DetachUSBDevice` | Supported with usbip-win2 `v0.9.7.7` | Not provided by this fork |
| Caller-owned bus lifetime | Supported | Same canonical lifecycle contract |

The tracked Windows attachment ABI is pinned to usbip-win2 `v0.9.7.7`, commit
`7c219953101cc5d0ec9a0bcb3eb87259cf72bedd`. Version `v0.9.7.8` is intentionally
unsupported because the upstream release carried a memory-corruption/BSOD
warning. Later versions require explicit ABI and runtime validation before
they can be considered compatible.

Non-Windows builds remain compile-compatible, but they must fail safely for
tracked localhost attachment. They must not record a fake attachment token or
claim that a device is attached.

## Handles and return values

All handles are opaque `uintptr_t` values. Treat them as capability tokens:

- never inspect or persist their internal value;
- pass a handle only to the matching typed API;
- stop using a device handle after its `Remove*Device` succeeds;
- expect stale, zero, wrong-type, and arbitrary handles to return `false`;
- keep callback function pointers alive for as long as they are registered.

Return semantics are defined by each export. Boolean compatibility exports
(`AttachUSBDevice`, `DetachUSBDevice`, `Create*Device`, `Set*DeviceState`,
`Remove*Device`, and similar) return `true` on success and `false` on
failure. Classified `*Ex` exports (`AttachUSBDeviceEx`, `DetachUSBDeviceEx`,
`RemoveSteamControllerDeviceEx`, `RemoveSteamDeckDeviceEx`, and similar)
return their documented operation-specific result enum directly instead of a
bool; see "Classified attach/detach results" and "Classified typed-device
removal" below for the exact enums. Output handle and output ID pointers
must be non-NULL. Invalid input is rejected before device construction, bus
registration, attachment, or handle creation.

## Classic Steam Controller trigger state

The generated `SteamControllerDeviceState` contains independent digital
trigger flags immediately after `L1` and `R1`:

```c
uint8_t L1, R1, L2, R2;
```

`LTrigger` and `RTrigger` remain the independent analog travel values. Setting
`L2` or `R2` does not change either analog value. The report layer sets the
corresponding digital full-pull bit when the explicit flag is true **or** when
the analog value reaches the existing full-pull threshold (`26000`). This lets
an application represent a digital click at any analog travel, including zero,
without losing the physical trigger position.

The `L2`/`R2` fields are part of the canonical C ABI and generated client
contract. Always consume `dist/libVIIPER/libVIIPER.h` and the DLL produced from
the same commit. Do not mix a header or generated client from before this
field-layout change with a newer DLL (or vice versa).

## Lifecycle order

The normal ownership order is:

```text
NewUSBServer
  -> CreateUSBBus
  -> Create*Device
  -> Set*DeviceState / Set*Callback / AttachUSBDevice
  -> DetachUSBDevice
  -> Remove*Device
  -> RemoveUSBBus
  -> CloseUSBServer
```

### Server states

The canonical wrapper uses four states:

```text
active -> closing -> closed
             |
             v
        close-failed -> retry CloseUSBServer -> closed
```

All server-owned mutations are accepted only in `active`:

- `CreateUSBBus` / `RemoveUSBBus`;
- every typed device create operation;
- every typed device remove operation;
- attachment and detachment mutations.

After `closing` or `close-failed`, callers may use only the explicitly safe
diagnostic/identity operations and retry `CloseUSBServer` according to the
failure state. A failed close is fail-closed; callers must not recreate buses,
retry an unknown attachment outcome, or continue normal mutation.

### Detached-ready typed devices

A typed device created with `autoAttachLocalhost=false` is immediately
usable in the `detached` state: its `Set*DeviceState` and
`Set*OutputCallback`/`Set*RumbleCallback` exports accept mutation before the
device has ever been attached. `AttachUSBDevice` is the only operation that
performs a Windows localhost attachment; creation itself never does.

An explicit `DetachUSBDevice` ends only that attachment, not the logical
device's lifetime. State and callback mutation remain valid on the same
typed handle while detached, and the same handle may be explicitly
reattached via `AttachUSBDevice` any number of times while the owning server
remains `active`. Only a successful typed `Remove*Device` (or `*Ex` variant)
ends the logical device's lifetime; `DetachUSBDevice` never does.

### Bus ownership

Typed device removal is intentionally **device-only**. A successful
`RemoveXbox360Device`, `RemoveSteamControllerDevice`, or other typed remove:

1. clears the device callback;
2. detaches a known attachment token, if present;
3. drains managed USB/IP work;
4. removes the logical device and finalizes its handle;
5. leaves the empty bus alive.

The caller owns that bus until `RemoveUSBBus` or `CloseUSBServer`. This differs
from the upstream timed empty-bus cleanup behavior and is required for a
long-lived embedded server.

### Close and partial failure

`CloseUSBServer` establishes the lifecycle boundary before taking its bus
snapshot. Buses are processed in ascending numeric BusID order, and devices
within a bus use registration order. A successful bus removal is finalized
immediately; a later failure does not restore or revisit it.

If transport or logical teardown fails:

- the server enters `close-failed`;
- already-finalized device handles remain invalid;
- surviving buses and their valid diagnostic handles remain available only as
  allowed by the failure contract;
- a retry uses a fresh bus snapshot and does not repeat completed teardown;
- an unknown attach/detach outcome blocks destructive cleanup and automatic
  retry.

## Attachment API

The tracked attachment functions operate on a typed device handle:

```c
bool AttachUSBDevice(uintptr_t deviceHandle);
bool DetachUSBDevice(uintptr_t deviceHandle);
bool GetUSBDeviceIdentity(uintptr_t deviceHandle,
                          uint32_t* outBusID,
                          uint32_t* outDeviceID);
```

`AttachUSBDevice` records the verified backend and exact positive import port.
`DetachUSBDevice` uses that stored token; it does not rediscover a port or
blindly retry another backend.

The attachment result is classified as:

- known failure: safe to report as failure;
- verified success: backend and positive port are recorded;
- unknown outcome: the server enters `close-failed`, no automatic retry is
  attempted, and the token is retained only for diagnostics.

### Classified attach/detach results

`AttachUSBDevice`/`DetachUSBDevice` collapse that classification into a bare
`bool`. Callers that need to distinguish a safe, explicitly retryable failure
from an unsafe unknown outcome should use the classified `Ex` variants
instead of trying to infer native ownership from a `bool`:

```c
typedef enum {
    VIIPER_ATTACH_SUCCESS = 0,
    VIIPER_ATTACH_RETRYABLE_FAILURE = 1,
    VIIPER_ATTACH_UNSAFE_OUTCOME_UNKNOWN = 2,
    VIIPER_ATTACH_INVALID = 3
} USBDeviceAttachResult;

typedef enum {
    VIIPER_DETACH_SUCCESS = 0,
    VIIPER_DETACH_RETRYABLE_FAILURE = 1,
    VIIPER_DETACH_UNSAFE_OUTCOME_UNKNOWN = 2,
    VIIPER_DETACH_INVALID = 3
} USBDeviceDetachResult;

USBDeviceAttachResult AttachUSBDeviceEx(uintptr_t deviceHandle);
USBDeviceDetachResult DetachUSBDeviceEx(uintptr_t deviceHandle);
```

`AttachUSBDeviceEx` and `DetachUSBDeviceEx` are not a separate mutation path:
`AttachUSBDevice`/`DetachUSBDevice` call the exact same classified operation
and return `true` only for `..._SUCCESS`, `false` for every other result.

- `..._SUCCESS`: the device was successfully attached/detached, or was
  already in that state (attach/detach remain idempotent; the same valid
  device already attached/detached succeeds without a second backend call).
- `..._RETRYABLE_FAILURE`: the operation had a known failure. Ownership
  remains known — for detach, the exact existing attachment token remains
  authoritative — the server remains active, and an explicit later retry is
  safe.
- `..._UNSAFE_OUTCOME_UNKNOWN`: native ownership cannot be determined
  safely. The device's attachment record becomes `attachmentOutcomeUnknown`
  and the owning server enters `close-failed`, matching the existing
  fail-closed contract. **If the device is already in
  `attachmentOutcomeUnknown`, subsequent `AttachUSBDeviceEx` or
  `DetachUSBDeviceEx` calls must return `UNSAFE_OUTCOME_UNKNOWN` again
  without invoking the backend or attempting any destructive retry. Do not
  downgrade this state to `INVALID`** — unknown means native ownership
  evidence remains unsafe, not that the handle became invalid.
- `..._INVALID`: a zero/stale/wrong-type handle, or a mutation the current
  server lifecycle does not permit (for example after the server has
  finished closing). No attachment attempt is made.

### Reading attachment state without mutating it

`AttachUSBDeviceEx`/`DetachUSBDeviceEx` report the outcome of a mutation.
`GetUSBDeviceAttachmentState` is the read-only counterpart: it reports the
current tracked attachment ownership state without attempting to change it.

```c
typedef enum {
    VIIPER_ATTACHMENT_DETACHED = 0,
    VIIPER_ATTACHMENT_ATTACHED = 1,
    VIIPER_ATTACHMENT_OUTCOME_UNKNOWN = 2
} USBDeviceAttachmentState;

bool GetUSBDeviceAttachmentState(uintptr_t deviceHandle,
                                 USBDeviceAttachmentState* outState);
```

**This reports VIIPER's own tracked localhost attachment ownership only. It
is not a Windows PnP, HID, or XInput readiness signal.**
`VIIPER_ATTACHMENT_ATTACHED` means VIIPER completed its tracked localhost
attach operation and retains the verified attachment token, backend, and
positive import port — it does not mean the Windows PnP node has enumerated,
HID interfaces are ready, XInput is ready, or Steam has discovered the
device. An application must still perform its own exact PnP
stabilization/ownership checks; this query never substitutes for that.
`VIIPER_ATTACHMENT_DETACHED` means no tracked localhost attachment is
currently active for that logical device; it does not by itself confirm
Windows-side disappearance where a consumer requires that.

State meanings:

- `VIIPER_ATTACHMENT_DETACHED`: the typed logical device is alive with no
  active tracked localhost attachment. If the owning server is `active`, an
  explicit attach may be attempted through the normal classified contract.
- `VIIPER_ATTACHMENT_ATTACHED`: the typed logical device is alive and VIIPER
  retains the exact verified attachment token (backend and positive import
  port) that `DetachUSBDeviceEx` will use.
- `VIIPER_ATTACHMENT_OUTCOME_UNKNOWN`: the typed device handle is still
  authoritative, but native attachment ownership cannot be determined
  safely — the same `attachmentOutcomeUnknown` state `AttachUSBDeviceEx`/
  `DetachUSBDeviceEx` report as `UNSAFE_OUTCOME_UNKNOWN`. The query still
  succeeds and reports this state; it does not fail merely because the
  owning server has entered `close-failed`. This remains the fail-closed
  signal to never attempt a destructive Attach/Detach retry.

Lifecycle and failure behavior:

- Like `GetUSBDeviceIdentity`, the query succeeds for a still-authoritative
  handle whether the owning server is `active` or `close-failed` — an
  unrelated `close-failed` condition never erases known attachment evidence.
  It fails (returns `false`, does not write `outState`) when the server is
  `closing` or `closed`.
- Fails for a `NULL outState`, a zero/stale/wrong-type handle, or a removed
  typed handle. `outState` is valid only when the function returns `true`.
- This is a pure read: it never invokes the attach or detach backend, and
  never mutates the attachment record, the stored token, server lifecycle
  state, or USB transport state. Repeated queries never call either backend.
- The state is snapshotted under the same server lifecycle lock that
  serializes `AttachUSBDeviceEx`/`DetachUSBDeviceEx`, so a query never
  observes a torn read of attachment state mid-mutation.

The legacy `clib` attachment functions are separate compatibility APIs. New
fork integrations should use the typed canonical functions instead of mixing
legacy error-only attachment with typed ownership records.

## Typed device families

The canonical wrappers currently include:

| Device | Create | State/callback | Remove |
| --- | --- | --- | --- |
| Steam Controller | `CreateSteamControllerDevice` | `SetSteamControllerDeviceState`, `SetSteamControllerOutputCallback` | `RemoveSteamControllerDevice`, `RemoveSteamControllerDeviceEx` |
| Xbox 360 | `CreateXbox360Device` | `SetXbox360DeviceState`, `SetXbox360RumbleCallback` | `RemoveXbox360Device`, `RemoveXbox360DeviceEx` |
| DualShock 4 | `CreateDS4Device` | `SetDS4DeviceState`, `SetDS4OutputCallback` | `RemoveDS4Device` |
| DualSense | `CreateDualSenseDevice`, `CreateDualSenseEdgeDevice` | `SetDualSenseDeviceState`, `SetDualSenseOutputCallback` | `RemoveDualSenseDevice` |
| Keyboard | `CreateKeyboardDevice` | `SetKeyboardDeviceState`, `SetKeyboardLEDCallback` | `RemoveKeyboardDevice` |
| Mouse | `CreateMouseDevice` | `SetMouseDeviceState` | `RemoveMouseDevice` |
| Nintendo Switch 2 Pro | `CreateNS2ProDevice` | `SetNS2ProDeviceState`, `SetNS2ProOutputCallback` | `RemoveNS2ProDevice` |
| Steam Deck | `CreateSteamDeckDevice` | `SetSteamDeckDeviceState`, `SetSteamDeckOutputCallback` | `RemoveSteamDeckDevice`, `RemoveSteamDeckDeviceEx` |

`lib/viiper` exposes the Steam Deck (`28DE:1205` by default) through a typed
wrapper: `CreateSteamDeckDevice`, `SetSteamDeckDeviceState`,
`SetSteamDeckOutputCallback`, `RemoveSteamDeckDevice`, and the classified
`RemoveSteamDeckDeviceEx`. It
reuses the shared `GetUSBDeviceIdentity`, `AttachUSBDevice`, and
`DetachUSBDevice` APIs rather than device-specific attach/detach entry
points, and it participates in the same server/bus/typed-handle ownership
model as every other typed device. The output callback carries the existing
generic Steam Deck host-output stream, including rumble, haptic, haptic-pulse,
and audio commands. It does not perform MSI Claw-specific translation or
claim SteamInputAddonforClaw rumble adoption. Basic non-gyro Steam Deck input
has been validated on MSI Claw EX. Lifecycle and recovery validation remains
pending; rumble/haptics and gyro remain separate feature tracks.

#### Steam Deck state ABI

The canonical `SteamDeckDeviceState` is **76 bytes**. Its final four
`uint16_t` fields are `LPadForce`, `RPadForce`, `LStickForce`, and
`RStickForce`, at offsets 68, 70, 72, and 74 respectively.

Steam Deck input transport uses a 64-byte report with report type `0x09`
and header length `64`. OpenSD's Steam Deck hardware userspace driver --
the original physical-device report model -- documents byte 3 as "Always
64 for input reports" and identifies bytes 60:64 as `l_stick_force`/
`r_stick_force`. InputPlumber carries and consumes that same report model.
Linux `hid-steam` currently documents and decodes 56 bytes of known Deck
state ending at the two pad-pressure values, and current SDL's
`SteamDeckStatePacket_t` likewise only exposes fields through pad pressure.
Those two consumer-visible structures do not establish report bytes 60:64
as padding: they only mean Linux and SDL do not currently read that far.
Handheld Companion also preserves the same tail in its virtual Steam Deck
target.

`LStickForce`/`RStickForce` are independent from the `L3`/`R3` digital
click bits earlier in the report. VIIPER preserves the raw tail generically;
a consumer without corresponding thumbstick sensor data may leave both
fields zero. The exact physical interpretation and units of these two raw
values should not be inferred beyond the available implementation evidence
-- treat them as a raw capacitive-related signal traceable to OpenSD's
hardware-driver report model, not a confirmed Valve pressure-sensor
specification.

A prior revision of this document and of `SteamDeckDeviceState` removed
`LStickForce`/`RStickForce` and shrank the struct to 72 bytes. That
revision kept the pre-existing `DeckInputPayloadLen = 56` header value
unchanged and interpreted it as the canonical wire-state boundary, reasoning
that Linux/SDL's shorter consumer-visible Deck state proved bytes 60:64
unused. That reasoning was incomplete: it did not check OpenSD's or
InputPlumber's physical Steam Deck report model or Handheld Companion's
Steam Deck implementation, all of which read/write that same tail. This
revision restores the 76-byte contract and the evidence above so the same
mistake is not repeated from a partial read of Linux/SDL alone.

Consumers **must** use a matching generated `libVIIPER.h`/DLL (or `.so`)
pair for this revision. Mixing a 72-byte `SteamDeckDeviceState` definition
with a 76-byte DLL, or a 76-byte definition with a 72-byte DLL, is an ABI
mismatch and will corrupt or misread trailing state fields.

### Classified typed-device removal

Typed families that expose classified removal have their own removal enum and
`*Ex` export. The legacy bool export for a given device remains the
compatibility API and returns `true` only for that family's `_SUCCESS` value.

The result is returned by the same removal operation; do not pair removal
with a process-global or thread-local last-status query.

#### Steam Controller

`RemoveSteamControllerDevice` remains the compatibility bool API. New
consumers that must preserve native ownership semantics should use:

```c
typedef enum {
    VIIPER_REMOVE_SUCCESS = 0,
    VIIPER_REMOVE_RETRYABLE_FAILURE = 1,
    VIIPER_REMOVE_UNSAFE_OUTCOME_UNKNOWN = 2,
    VIIPER_REMOVE_INVALID = 3
} SteamControllerDeviceRemoveResult;

SteamControllerDeviceRemoveResult RemoveSteamControllerDeviceEx(
    uintptr_t deviceHandle);
```

- `VIIPER_REMOVE_SUCCESS`: the typed device handle was finalized.
- `VIIPER_REMOVE_RETRYABLE_FAILURE`: ownership remains known and the same
  handle may be retried explicitly. This includes a known detach failure or a
  known logical-removal failure after detach completed.
- `VIIPER_REMOVE_UNSAFE_OUTCOME_UNKNOWN`: attachment/detachment ownership is
  not safely known. Do not retry Remove, Detach, or Attach destructively, and
  preserve the server's fail-closed recovery evidence.
- `VIIPER_REMOVE_INVALID`: the handle, device type, or server lifecycle is
  invalid for this operation.

The legacy bool export returns `true` only for `VIIPER_REMOVE_SUCCESS` and
`false` for every other result.

#### Steam Deck

`RemoveSteamDeckDevice` remains the compatibility bool API. New consumers
that must preserve native ownership semantics should use:

```c
typedef enum {
    VIIPER_STEAMDECK_REMOVE_SUCCESS = 0,
    VIIPER_STEAMDECK_REMOVE_RETRYABLE_FAILURE = 1,
    VIIPER_STEAMDECK_REMOVE_UNSAFE_OUTCOME_UNKNOWN = 2,
    VIIPER_STEAMDECK_REMOVE_INVALID = 3
} SteamDeckDeviceRemoveResult;

SteamDeckDeviceRemoveResult RemoveSteamDeckDeviceEx(
    uintptr_t deviceHandle);
```

The values carry the same semantics as the Steam Controller enum above (success,
retryable failure, unsafe-unknown outcome, and invalid handle/lifecycle use),
under the Steam Deck's own distinct enum and constant names. Do not assume
`SteamControllerDeviceRemoveResult` is the only classified removal
enum, and do not mix the two families' constants.

The legacy bool export returns `true` only for
`VIIPER_STEAMDECK_REMOVE_SUCCESS` and `false` for every other result.

#### Xbox 360

`RemoveXbox360Device` remains the compatibility bool API. Consumers that
need the shared classified ownership lifecycle should use:

```c
typedef enum {
    VIIPER_XBOX360_REMOVE_SUCCESS = 0,
    VIIPER_XBOX360_REMOVE_RETRYABLE_FAILURE = 1,
    VIIPER_XBOX360_REMOVE_UNSAFE_OUTCOME_UNKNOWN = 2,
    VIIPER_XBOX360_REMOVE_INVALID = 3
} Xbox360DeviceRemoveResult;

Xbox360DeviceRemoveResult RemoveXbox360DeviceEx(uintptr_t deviceHandle);
```

The values have the same meanings as the other classified removal APIs. The
legacy bool export returns `true` only for
`VIIPER_XBOX360_REMOVE_SUCCESS` and `false` for every other result.

## Callback and teardown contract

Callbacks are captured under the device callback lock and invoked after that
lock is released. A callback may therefore re-enter a safe canonical API
without creating a callback-lock deadlock. Steam Deck output callbacks use a
single compatibility-preserving normalization path: an empty control payload
with a nonzero report ID is synthesized before normalization; a leading
`0x00` is removed only when the input contains more than one byte; and the
normalized payload is bounded to 64 bytes. The callback receives the exact
stored length, and its temporary C-owned buffer is valid only during the
synchronous callback. Callers must copy retained data.

During typed removal and bus/server teardown, callback clearing occurs before
detach and logical removal. Already-running callback invocations are allowed
to finish, but no later dispatch may capture the cleared callback. The public
remove/close operation returns only after managed USB/IP transport work and
async non-EP0 IN workers have drained.

Applications should keep callback code short and avoid blocking unrelated
application threads. If a callback must wait for application work, ensure the
application can release it during device removal or server shutdown.

## Diagnostic logging

```c
typedef enum {
    VIIPER_LOG_DEBUG = -4,
    VIIPER_LOG_INFO  = 0,
    VIIPER_LOG_WARN  = 4,
    VIIPER_LOG_ERROR = 8,
} VIIPERLogLevel;

typedef void (*VIIPERLogCallback)(VIIPERLogLevel level, const char* message);
```

`NewUSBServer`'s `logCallback` parameter is unchanged. What changed is
ownership: `libVIIPER` owns its own diagnostic log and does not depend on
the embedding application to persist it.

- On Windows, `NewUSBServer` independently attempts to provide `libVIIPER.log`
  beside the directory containing the actually loaded `libVIIPER.dll`
  module, regardless of whether `logCallback` is `NULL` — module-path
  resolution or the file open can still fail safely (see below), in which
  case there is simply no file sink for that process. This fork has no
  file-sink implementation for non-Windows builds; a supplied `logCallback`
  still works normally there.
- When the owned file sink is available, it uses exactly one `libVIIPER.log`,
  containing current-local-calendar-day diagnostics only (local date, no
  timezone configuration). Records append during the same day. On the first
  write after the local date changes — including the process simply staying
  alive across midnight, not only a fresh `NewUSBServer` — the same file is
  reset in place and reused for the new day. On a successful reset, the
  triggering record is preserved as the first record of the new day. If the
  reset fails, file persistence is suppressed for the rest of that day
  rather than appending to now-stale content; `logCallback`, if supplied, is
  entirely unaffected either way. No dated archive, numbered rotation, size
  limit, compression, or
  background cleanup is maintained — retention is deliberately this simple.
- `VIIPERLogCallback` is an optional observer/mirror, not the persistence
  mechanism, and remains synchronous and entirely unaffected by file
  retention. Passing `NULL` does not disable the file; passing a callback
  does not cause a record to be written into the file twice — both
  destinations receive the same record.
- The owned file is written asynchronously: a bounded, non-blocking queue
  decouples `AttachUSBDeviceEx`/`DetachUSBDeviceEx`/etc. from the actual
  filesystem write, so a slow disk, antivirus, or filter driver cannot add
  to the wall-clock time those calls take to return. The daily reset above
  happens only inside that same background writer, never on the calling
  thread. If the queue saturates under a burst, the diagnostic record is
  dropped and counted rather than blocking the caller; `CloseUSBServer`
  requests a best-effort, time-bounded flush of that queue — after
  releasing its own lifecycle lock — on success, but a flush timeout never
  changes `CloseUSBServer`'s own result and never holds that lock while
  waiting.
- A zero/stale/unresolvable handle has no server to own a callback, so its
  diagnostics (e.g. an invalid-handle `AttachUSBDeviceEx` call) go to a
  library-owned, file-only fallback logger — never to any particular
  server's `VIIPERLogCallback`, and never through Go's process-global
  `slog.Default()`.
- If module-path resolution, the log file open, or a daily reset fails, that
  failure is silently absorbed: it never fails `NewUSBServer`, never changes
  attachment/removal/lifecycle classification, and never falls back to
  stdout/stderr.
- Routine lifecycle, attachment, classified-failure, and the attachment-
  timing diagnostics documented above are low-volume and always active
  through this path. Per-input/per-frame state updates and report/publisher
  loops do not log through this mechanism; raw packet logging is a separate,
  off-by-default facility.

## Minimal C usage

The following is a lifecycle sketch. Use the generated
`dist/libVIIPER/libVIIPER.h` from the same build as the DLL.

```c
USBServerConfig config = {0};
USBServerHandle server = 0;
if (!NewUSBServer(&config, &server, NULL)) {
    return false;
}

uint32_t bus = 0;
if (!CreateUSBBus(server, &bus)) {
    CloseUSBServer(server);
    return false;
}

SteamControllerDeviceHandle device = 0;
if (!CreateSteamControllerDevice(server, &device, bus, false, 0, 0)) {
    RemoveUSBBus(server, bus);
    CloseUSBServer(server);
    return false;
}

/* State updates and callback registration happen while the server is active. */
SetSteamControllerDeviceState(device, state);

/* Optional Windows-only tracked attachment. */
AttachUSBDevice(device);

DetachUSBDevice(device);
RemoveSteamControllerDevice(device);
RemoveUSBBus(server, bus);
CloseUSBServer(server);
```

In production code, check every return value and treat a failed close or
unknown attachment outcome as a lifecycle fault requiring operator or host
application handling.

## Build and validation

Build the canonical library and generated header with:

```text
just build-libVIIPER Release
```

Before consuming a new DLL/header pair, validate:

```text
go test ./...
go test -race ./internal/server/usb ./lib/viiper
go vet ./...
git diff --check
```

CI additionally validates the Windows shared-library build, generated C
header layout, exported symbols, and canonical lifecycle race tests. The DLL
and header must come from the same commit; do not mix artifacts from upstream
VIIPER with this fork's canonical ABI.

### Canonical dependency artifact and manifest

Main-branch CI packages the Windows canonical build as
`libVIIPER-windows-amd64.zip`, alongside a `viiper-artifact.json` manifest
that records the artifact's source identity (full Git commit SHA) and
SHA-256 hashes of the exact `libVIIPER.dll` and `libVIIPER.h` in that
package. CI regenerates and independently reverifies these hashes in the same
job that produces the DLL/header, so the manifest cannot describe a different
build than the one it ships with.

Pull request builds generate and verify the same manifest for validation
only; they do not upload artifacts and are not adoption candidates. Only a
successful main-branch build produces an artifact eligible for downstream
adoption. Consumers must pin an exact commit/artifact rather than depending
on a mutable "latest" build.

SteamInputAddonforClaw consumes this artifact through a two-repository
automation pipeline:

```text
VIIPER main push
  -> Dev Snapshot Build completes successfully
  -> notify-addon-dependency.yml validates success/main/push and the exact
     40-character head_sha, then sends repository_dispatch
     (viiper-canonical-ready) with that commit
  -> SteamInputAddonforClaw's viiper-dependency-update.yml independently
     re-verifies the exact commit's canonical artifact, manifest, and hashes
  -> mechanical Draft dependency PR
  -> human ABI/runtime review
  -> manual merge only
```

The dispatch payload's commit is only a hint/input, not dependency
authority — the generated canonical artifact for that exact commit remains
the actual dependency source, and the Addon receiver never trusts the
payload's claims without independently re-deriving them. Managed ABI
compatibility is never auto-inferred from this pipeline, and the resulting
Draft PR is never auto-merged.
