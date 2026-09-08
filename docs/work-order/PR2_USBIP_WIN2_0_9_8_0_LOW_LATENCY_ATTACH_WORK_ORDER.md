# Work Order — PR2: usbip-win2 0.9.8.0 ABI Migration and Always-On Low-Latency Attach

## Status

Implementation work order for the second step of the Steam Addon for Claw usbip-win2 migration sequence.

This work order targets the **user fork**:

```text
repository: onehoon/VIIPER
branch:     main
baseline:   77a8af547de2253862ede648a212c01d4dd950c1
message:    Reduce Windows USBIP loopback attach latency (#44)
```

This is **not** an upstream VIIPER work order.

The fork is the implementation authority. Upstream `vadimgrn/usbip-win2` is used only as the authoritative external ABI/runtime reference for usbip-win2 `v0.9.8.0`.

The previous Addon-side prerequisite PR is already complete. SteamAddonforClaw can now classify an older installed usbip-win2 package as `UpdateRequired` and route that upgrade through its existing elevated prerequisite setup path. This VIIPER PR must now make the canonical Windows attach implementation compatible with usbip-win2 `0.9.8.0` and make low-latency receive mode the fixed product policy.

---

# 1. Goal

Migrate the fork's Windows localhost USB/IP attach contract from usbip-win2 `0.9.7.7` to **usbip-win2 `0.9.8.0` only**, while preserving all existing fork ownership and fail-close behavior.

Final intended contract after this PR:

```text
Supported usbip-win2 for tracked Windows localhost attach = 0.9.8.0 only
Native PLUGIN_HARDWARE ABI                           = v0.9.8.0
Receive mode                                         = low-latency, always
Native attach                                        = preferred
Command attach                                       = fallback only when current contract permits
Tracked detach                                       = exact stored backend + exact stored import port
Unknown native attach outcome                        = fail closed; never command-fallback
Public libVIIPER C ABI                               = unchanged
```

No compatibility layer for `0.9.7.7` is required because the Steam Addon product is still pre-release and the coordinated Addon migration will move directly to `0.9.8.0`.

---

# 2. Required source review before implementation

Read the current fork implementation first. At minimum inspect:

```text
FORK_ARCHITECTURE.md
docs/libviiper/fork-api.md
internal/server/api/autoattach_contract.go
internal/server/api/autoattach_windows.go
internal/server/api/autoattach_contract_test.go
internal/server/api/autoattach_windows_test.go
.github/workflows/PR_check.yml
.github/workflows/build_base.yml
justfile
```

Also inspect the canonical embedded library attachment callers and ownership tests before touching shared behavior:

```text
lib/viiper/attach_invariants_test.go
lib/viiper/classified_attachment_test.go
lib/viiper/ownership_invariants_test.go
lib/viiper/lifecycle_serialization_test.go
lib/viiper/attachment_state_query_test.go
```

Do not refactor these layers merely because this PR changes the usbip driver ABI. Their ownership semantics are already part of the fork contract.

---

# 3. External ABI reference — usbip-win2 v0.9.8.0

Use this exact upstream usbip-win2 source revision as the ABI reference:

```text
repository: vadimgrn/usbip-win2
tag:        v.0.9.8.0
commit:     83bd1f781d57ed6efdf15530c55710cf5d4482bc
```

Relevant upstream files:

```text
include/usbip/consts.h
include/usbip/vhci.h
drivers/ude/vhci_ioctl.cpp
userspace/libusbip/src/vhci.cpp
userspace/usbip/usbip.cpp
```

Do not copy unrelated upstream architecture into the fork. Only reproduce the ABI and command semantics necessary for the fork's existing localhost attach owner.

## 3.1 Constants

Upstream `v0.9.8.0` defines:

```cpp
constexpr auto BUS_ID_SIZE = 32;
constexpr auto SERIAL_BUFSZ = 16;
```

The existing fork already matches the bus ID and Windows network constants used by the old struct.

## 3.2 `plugin_hardware` ABI

Upstream `include/usbip/vhci.h` defines the relevant layout conceptually as:

```cpp
struct base {
    UINT32 size;
};

struct imported_device_location {
    int port;
    char busid[BUS_ID_SIZE];
    char service[NI_MAXSERV];
    char host[NI_MAXHOST];
};

struct plugin_hardware : base, imported_device_location {
    char serial[SERIAL_BUFSZ];
    bool wsk_events;
};
```

Compared with the fork's current `0.9.7.7` binding, two fields are now required at the end:

```text
serial[16]
wsk_events
```

The v0.9.8.0 driver validates the **full input buffer size** against `sizeof(plugin_hardware)` and also validates the `size` field. Therefore the current 0.9.7.7-sized request must not be sent to a 0.9.8.0 driver.

## 3.3 Driver response length remains the port prefix

The v0.9.8.0 driver sets the request information length to:

```cpp
offsetof(plugin_hardware, port) + sizeof(port)
```

That remains 8 bytes:

```text
size : bytes 0..3
port : bytes 4..7
```

Therefore the fork's existing exact response check remains valid:

```text
bytesReturned == 8
returned port > 0
```

Do not expand the output buffer expectation to the entire input struct.

## 3.4 Low-latency selection

Upstream `userspace/libusbip/src/vhci.cpp` maps:

```text
receive_mode::low_latency
→ plugin_hardware.wsk_events = true
```

and the UDE driver chooses the WSK event-callback receive path when `wsk_events` is true.

Upstream CLI exposes:

```text
--receive-mode=zero-copy
--receive-mode=low-latency
```

with `zero-copy` remaining the default when the option is omitted.

Because this fork's product policy is **always low-latency**, every Windows localhost attach path used by the fork must explicitly select low-latency.

---

# 4. Product decision — no compatibility matrix

The Steam Addon product is not released yet. Do not preserve a dual ABI solely for old development installations.

Required support policy:

```text
0.9.7.7 → no longer the supported final binding
0.9.8.0 → supported binding
later unknown versions → not claimed compatible by this PR
```

Do not add:

- runtime ABI auto-detection;
- `attachIOCTL2977` plus `attachIOCTL2980` structs;
- a version-switching adapter;
- package registry probing inside VIIPER;
- a compatibility table;
- a user-selectable zero-copy/low-latency option;
- environment variables for receive mode;
- a new attachment manager or strategy abstraction.

SteamAddonforClaw owns package-version admission. VIIPER only needs one explicit binding for the package version the product ships.

---

# 5. Current fork invariants — must remain unchanged

## 5.1 Native-first tracked attach

`attachLocalhostClientWithFallback(...)` currently implements the correct authority boundary:

```text
native attach succeeds
→ return native ownership token

native attach fails before ownership can exist
→ command fallback may run exactly once

native attach returns ErrAttachmentOutcomeUnknown
→ do not command-fallback
→ return unknown outcome
```

Preserve this behavior.

Do not change an ABI error that occurs **after the native DeviceIoControl was submitted** into a known pre-ownership failure unless there is concrete evidence that Windows guarantees no mutation occurred.

The existing conservative classification is intentional because command fallback after an unknown native outcome could create a second import whose ownership is unclear.

## 5.2 Exact imported-port ownership

Successful tracked attachment stores:

```text
backend
positive imported port
```

Detach must continue to use that exact stored token.

Do not:

- rediscover a port by VID/PID;
- enumerate and choose the only visible import;
- detach all ports;
- detach by bus ID after ownership is committed;
- convert exact detach into best-effort cleanup.

The v0.9.8.0 migration does not change this ownership model.

## 5.3 Public C ABI remains unchanged

No change is required to:

```text
AttachUSBDevice
AttachUSBDeviceEx
DetachUSBDevice
DetachUSBDeviceEx
GetUSBDeviceAttachmentState
```

No new public enum or C export is required for low-latency mode.

Low-latency is an internal Windows transport policy for this product integration.

---

# 6. Required implementation — native v0.9.8.0 struct

Modify only the existing Windows ABI binding in:

```text
internal/server/api/autoattach_windows.go
```

Current shape:

```go
type attachIOCTL struct {
    Size       uint32
    PortOutput int32
    BusID      [32]byte
    Service    [niMaxServ]byte
    Host       [niMaxHost]byte
}
```

Target shape:

```go
const (
    niMaxHost    = 1025
    niMaxServ    = 32
    serialBufSize = 16
)

type attachIOCTL struct {
    Size       uint32
    PortOutput int32
    BusID      [32]byte
    Service    [niMaxServ]byte
    Host       [niMaxHost]byte
    Serial     [serialBufSize]byte
    WskEvents  bool
}
```

Naming may be adjusted to match existing Go style, but keep the layout obvious and directly traceable to upstream `plugin_hardware`.

Do not introduce a nested inheritance-mirroring hierarchy in Go. One flat struct is simpler and matches the current fork style.

## 6.1 Expected Windows layout

Pin the actual layout with `unsafe.Offsetof` / `unsafe.Sizeof` tests.

Expected values on the supported Windows binding:

```text
Size offset       = 0
PortOutput offset = 4
BusID offset      = 8
Service offset    = 40
Host offset       = 72
Serial offset     = 1097
WskEvents offset  = 1113
WskEvents size    = 1
struct size       = 1116
output prefix     = 8
plugoutIOCTL size = 8
```

Why total size is 1116:

```text
end of Host      = 1097
+ Serial[16]     = 1113
+ bool           = 1114
natural 4-byte struct alignment
→ total 1116
```

Do **not** add a manual padding member unless the actual compiler/test proves the layout does not match. Go already applies trailing struct alignment.

## 6.2 Set low-latency explicitly

Current request creation zero-initializes `attachIOCTL`, fills bus ID/service/host, and sets `Size`.

Add only the product policy field:

```go
var ioctlData attachIOCTL
ioctlData.Size = uint32(unsafe.Sizeof(ioctlData))
...
ioctlData.WskEvents = true
```

Do not make this conditional.

The serial field should remain zero-initialized unless the current fork already has a real, authoritative serial source for this exact attach operation. Do not invent a serial identifier merely because v0.9.8.0 added the field.

The upstream libusbip path also starts from a zero-initialized request and only fills serial when a serial is explicitly supplied.

## 6.3 Preserve response validation

Keep the existing output contract:

```go
attachPortOutputLength = uint32(
    unsafe.Offsetof(attachIOCTL{}.PortOutput) +
        unsafe.Sizeof(attachIOCTL{}.PortOutput))
```

Expected value remains:

```text
8
```

Preserve `validateNativeAttachResponse(...)` behavior:

```text
bytesReturned != 8
→ ErrAttachmentOutcomeUnknown

returned port <= 0
→ ErrAttachmentOutcomeUnknown
```

Do not weaken this just because the input struct becomes larger.

---

# 7. Required implementation — command attach must also be low-latency

The tracked command backend currently builds:

```text
usbip --tcp-port <port> attach -r 127.0.0.1 -b <busid> --terse
```

Change it to explicitly select low-latency:

```text
usbip --tcp-port <port> attach -r 127.0.0.1 -b <busid> --receive-mode=low-latency --terse
```

Recommended minimal code:

```go
func usbipAttachCommandArgs(usbipServerPort uint16, busID string) []string {
    return []string{
        "--tcp-port", strconv.FormatUint(uint64(usbipServerPort), 10),
        "attach",
        "-r", "127.0.0.1",
        "-b", busID,
        "--receive-mode=low-latency",
        "--terse",
    }
}
```

Do not rely on the upstream CLI default. In v0.9.8.0 the default remains zero-copy.

## 7.1 Legacy Windows command path

`autoattach_windows.go` also contains an isolated legacy command attach path that constructs `exec.CommandContext(...)` directly instead of using the tracked `usbipAttachCommandArgs(...)` helper.

If that path remains reachable in the current fork, it must also include:

```text
--receive-mode=low-latency
```

because the product policy is not “tracked path only”; it is “Windows localhost attach uses low-latency.”

Keep the change local. Do not redesign or merge the legacy and tracked ownership systems in this PR.

If a tiny shared constant such as:

```go
const usbipLowLatencyReceiveArg = "--receive-mode=low-latency"
```

avoids spelling drift between the two existing command paths, that is acceptable. Do not build a receive-mode abstraction or option type.

---

# 8. Do not change detach

usbip-win2 v0.9.8.0 does not require a new detach ownership design for this fork.

Preserve:

```go
type plugoutIOCTL struct {
    Size uint32
    Port int32
}
```

and preserve command detach:

```text
usbip detach -p <exact stored port>
```

Do not add receive-mode arguments to detach.

Do not change exact-port validation.

---

# 9. Failure classification must stay fail-closed

This PR changes an ABI field and a transport receive policy. It must not weaken the existing ownership classifications.

Required behavior:

```text
native open/control-device setup fails before attach submission
→ known failure
→ command fallback may be allowed by current contract

DeviceIoControl is submitted and returns an error
→ ErrAttachmentOutcomeUnknown
→ no command fallback

DeviceIoControl reports unexpected byte count
→ ErrAttachmentOutcomeUnknown
→ no command fallback

DeviceIoControl reports non-positive port
→ ErrAttachmentOutcomeUnknown
→ no command fallback

command fallback returns malformed/non-positive terse port
→ attach failure
→ do not invent ownership
```

Do not add retries around `PLUGIN_HARDWARE` just to tolerate hypothetical scheduling windows.

Do not add a second probe to guess whether the driver actually imported after an unknown result. The current unknown-outcome contract is deliberately conservative and is a real protection against duplicate ownership.

---

# 10. Required Windows ABI tests

Update:

```text
internal/server/api/autoattach_windows_test.go
```

The current test is explicitly named for `0.9.7.7` and pins the old 1100-byte request.

Replace it with a v0.9.8.0 contract test, for example:

```go
func TestUSBIPWin20980NativeABIContract(t *testing.T) {
    var request attachIOCTL

    require.Equal(t, uintptr(4), unsafe.Offsetof(request.PortOutput))
    require.Equal(t, uint32(8), attachPortOutputLength)
    require.Equal(t, uintptr(1097), unsafe.Offsetof(request.Serial))
    require.Equal(t, uintptr(1113), unsafe.Offsetof(request.WskEvents))
    require.Equal(t, uintptr(1), unsafe.Sizeof(request.WskEvents))
    require.Equal(t, uint32(1116), attachInputLength)

    require.Len(t, request.BusID, 32)
    require.Len(t, request.Service, 32)
    require.Len(t, request.Host, 1025)
    require.Len(t, request.Serial, 16)

    require.Equal(t, uintptr(8), unsafe.Sizeof(plugoutIOCTL{}))
}
```

Use the repository's current assertion style; the exact helper package is not important.

The test must explicitly fail if a future edit silently returns the native request to the old 0.9.7.7 size.

---

# 11. Required native request-content test

The fork already has a `nativeAttachOps` seam used to verify the IPv4 loopback endpoint without a real kernel driver.

Extend that test or add one focused adjacent test to verify the v0.9.8.0 request sent to `DeviceIoControl` contains:

```text
Size       = 1116
BusID      = expected <bus>-<device>
Service    = expected local USB/IP server port
Host       = 127.0.0.1
Serial     = empty / zero-filled unless an authoritative serial source exists
WskEvents  = true
```

Prefer inspecting the struct received by the existing fake operation.

Do not create a new mock framework.

No real usbip driver should be required for this unit test.

---

# 12. Required command argument tests

Update the existing test in:

```text
internal/server/api/autoattach_contract_test.go
```

Current expected attach command omits receive mode.

New expected attach args must contain exactly one:

```text
--receive-mode=low-latency
```

and must still contain:

```text
--tcp-port <exact server port>
attach
-r 127.0.0.1
-b <exact bus ID>
--terse
```

Example expected slice:

```go
wantAttach := []string{
    "--tcp-port", "3241",
    "attach",
    "-r", "127.0.0.1",
    "-b", "9-12",
    "--receive-mode=low-latency",
    "--terse",
}
```

Keep the exact-port detach assertion unchanged.

If the isolated legacy command path is still reachable and not built from this helper, add the smallest focused test or seam necessary to pin low-latency there as well. Do not rewrite the legacy path into the tracked contract solely to make it testable.

---

# 13. Preserve existing fallback and ownership tests

These tests must continue to pass without weakening their expectations:

```text
TestTrackedAttachFallbackIsSingleLayerAndFailClosed
native known failure → command fallback exactly once
native ErrAttachmentOutcomeUnknown → command fallback zero times
```

Also preserve tests covering:

- exact positive port parsing;
- malformed terse output rejection;
- non-positive port rejection;
- tracked attachment state;
- exact backend retention;
- exact-port detach;
- unknown detach result classification;
- typed-device lifecycle serialization;
- server close-failed behavior after unsafe outcomes.

Do not “fix” a test by changing the ownership contract to match a new implementation shortcut.

---

# 14. Windows CI must actually execute the ABI tests

The current PR workflow has an important coverage gap for this specific change:

```text
build_base.yml / test job
→ runs on ubuntu-latest
→ just test-coverage
→ Windows-only autoattach_windows_test.go is not executed
```

The Windows `libviiper-windows` job builds the DLL but currently does not explicitly run `internal/server/api` Windows tests.

Because this PR is specifically a Windows kernel ABI migration, add a focused Windows test step to the existing `libviiper-windows` job.

Recommended step before the canonical DLL build:

```yaml
- name: Test Windows USB/IP attach contract
  shell: pwsh
  run: |
      go test -count=1 ./internal/server/api
```

This is justified product coverage, not generic CI expansion.

Do not add another workflow or a large Windows test matrix.

Do not move the entire existing Ubuntu suite to Windows.

The purpose is only to ensure that the v0.9.8.0 native layout and low-latency Windows attach tests are executed in PR CI.

---

# 15. Documentation updates required in the fork

## 15.1 `FORK_ARCHITECTURE.md`

Current text pins the tracked native ABI to usbip-win2 `v0.9.7.7`.

Update the architectural source of truth to:

```text
tracked Windows native ABI = usbip-win2 v0.9.8.0
reference commit = 83bd1f781d57ed6efdf15530c55710cf5d4482bc
receive mode = low-latency for the fork's Windows localhost attach contract
```

State that later versions remain unsupported until explicitly validated.

Do not claim that the fork dynamically checks the installed usbip version if it does not.

The Addon integration provides the package-version admission boundary.

## 15.2 `docs/libviiper/fork-api.md`

Update the support matrix and Windows attachment section.

Required semantic changes:

```text
Tracked localhost AttachUSBDevice / DetachUSBDevice
→ Supported with usbip-win2 v0.9.8.0
```

Replace the old v0.9.7.7 ABI pin with the v0.9.8.0 commit above.

Document that:

- native tracked attach sets the v0.9.8.0 WSK-event flag;
- command attach passes `--receive-mode=low-latency`;
- low-latency is fixed for this fork's Windows localhost attach path, not caller-configurable;
- exact imported-port ownership and fail-close behavior are unchanged;
- v0.9.7.7 is not retained as a second supported ABI.

Do not broadly rewrite generic usbip installation documentation in `docs/getting-started/usbip.md` unless implementation exposes a factual inconsistency that must be corrected.

---

# 16. Explicit non-goals

Do not include any of the following in PR2.

## 16.1 No SteamAddonforClaw package update

Do not modify the SteamAddonforClaw repository in this PR.

The later Addon PR will:

```text
bundle USBip-0.9.8.0-x64.exe
update installer hash/version metadata
adopt this PR's canonical libVIIPER artifact
exercise 0.9.7.7 → 0.9.8.0 UpdateRequired migration
```

## 16.2 No usbip installer bundling in VIIPER

VIIPER does not become the driver installer owner.

Do not add an installer download, package registry manager, or elevation flow here.

## 16.3 No receive-mode setting

Do not expose:

```text
zero-copy / low-latency toggle
CLI setting owned by libVIIPER
C API parameter
configuration file option
environment variable
```

The current product decision is low-latency always.

## 16.4 No latency benchmark requirement for merge

Do not block this PR on proving a numerical latency reduction.

The reasons for selecting low-latency are:

- upstream explicitly added it for small/high-frequency traffic such as HID;
- the product workload is controller/HID-oriented;
- the user selected low-latency as the product policy.

Acceptance is based on correctness, ownership, input/output behavior, and lifecycle stability.

An A/B performance study can be done separately if useful, but must not force a receive-mode abstraction into production code.

## 16.5 No unrelated USB transport refactor

Do not refactor:

- `internal/server/usb` request processing;
- device report scheduling;
- Steam Deck report formats;
- Xbox360 report formats;
- callbacks;
- typed-device handle architecture;
- logging architecture;
- bus allocation.

This PR is a Windows attach ABI/receive-path migration only.

---

# 17. Expected files to change

Primary production code:

```text
internal/server/api/autoattach_windows.go
internal/server/api/autoattach_contract.go
```

`autoattach_contract.go` should change only if needed for the command argument string/helper. Do not alter fallback policy.

Tests:

```text
internal/server/api/autoattach_windows_test.go
internal/server/api/autoattach_contract_test.go
```

CI:

```text
.github/workflows/build_base.yml
```

Fork documentation:

```text
FORK_ARCHITECTURE.md
docs/libviiper/fork-api.md
```

Files that should normally **not** change:

```text
libviiper.h
lib/viiper public export definitions
lib/viiper typed-device public APIs
internal/server/usb/*
device/steamdeck/*
device/steamcontroller/*
device/xbox360/*
usbip/usbip.go
```

If implementation requires touching one of those, first prove that the v0.9.8.0 attach contract cannot be implemented inside the existing Windows attachment owner.

---

# 18. Validation — source and unit level

Run at minimum:

```text
go fmt ./...
go test ./...
go vet ./...
git diff --check
```

Because the ABI code is Windows-only, also run on Windows:

```text
go test -count=1 ./internal/server/api
```

Then build the canonical Windows embedded library using the repository-supported path:

```text
just build-libVIIPER Release
```

Do not use the legacy root `build_dll.bat` as the canonical artifact authority. `FORK_ARCHITECTURE.md` defines `just build-libVIIPER Release` and the `libviiper-windows` CI job as the canonical build path for downstream SteamAddonforClaw adoption.

The PR must pass the existing canonical export/header/manifest verification.

---

# 19. Hardware/runtime validation for PR2

A unit test can prove the struct layout and request contents, but it cannot prove the real usbip-win2 0.9.8.0 driver accepts the IOCTL and operates correctly.

Before downstream Addon adoption, validate the canonical Windows library against a machine with usbip-win2 `0.9.8.0` installed.

Minimum VIIPER-level validation:

```text
1. Start VIIPER/libVIIPER localhost USB server.
2. Create a supported logical controller device.
3. Native tracked attach succeeds.
4. Returned import port is positive and recorded exactly.
5. Attachment query reports Attached.
6. Input reports reach the virtual controller normally.
7. Host output/callback path works for the tested controller.
8. Detach targets the exact stored port and succeeds.
9. Attachment query returns Detached afterward.
10. Repeat attach/detach multiple times without leaked imports.
```

For Steam Addon integration priority, validate at least:

```text
Xbox360 presentation
SteamDeck presentation
```

For Steam Deck, verify both input and the existing output callback path. For Xbox360, verify input and rumble/output behavior through the existing supported callback path.

Do not turn this PR into a new controller feature project if an unrelated output feature is already unsupported by the current fork.

---

# 20. Real lifecycle checks

The following are realistic supported conditions and should be exercised where practical before the canonical artifact is adopted by SteamAddonforClaw:

```text
repeated attach → detach
process/server teardown while detached
process/server teardown after attached device has been cleanly detached
runtime restart and fresh attach
USB/IP service/device temporarily unavailable
real native attach failure
real native detach failure
Windows sleep → resume
Windows hibernate → resume
```

For this VIIPER PR, the important question is whether ownership state and exact-port cleanup remain correct.

Full MSI Claw authority behavior involving:

```text
PID1901 ↔ PID1902
HidHide physical isolation
Center M Disabled authority
Steam/BPM presentation policy
PnP physical-controller recovery
```

belongs to the downstream SteamAddonforClaw integration PR, not to VIIPER PR2.

Do not add MSI-specific state to VIIPER to test those scenarios here.

---

# 21. Operation-failure acceptance

A real attach failure must not create an unowned duplicate import.

A real detach failure must not silently erase the tracked ownership token.

Preserve the existing classified public behavior used by the Addon:

```text
Success
RetryableFailure
UnsafeOutcomeUnknown
Invalid
```

The exact classification mapping must remain consistent with the current fork tests.

Do not translate `ErrAttachmentOutcomeUnknown` into a retryable result merely because a second attach might work.

---

# 22. Race / overengineering boundary

Follow the Steam Addon / fork review policy.

This PR must defend real lifecycle safety, but it must not invent synchronization for pathological timing combinations.

Already-existing ownership boundaries are sufficient unless real evidence proves otherwise:

```text
tracked attachment token
exact import port
existing lifecycleMu serialization in canonical libVIIPER
ErrAttachmentOutcomeUnknown fail-close
single command fallback layer
server close-failed state
```

Do not add:

- attach epochs;
- WSK-mode state machines;
- retry coordinators;
- native/command ownership arbiters;
- receive-mode managers;
- extra locks around immutable receive-mode policy;
- background reconciliation loops.

The v0.9.8.0 migration is deterministic: one ABI, one low-latency policy, one existing owner.

---

# 23. Acceptance criteria

PR2 is complete only when all of the following are true.

1. Implementation targets `onehoon/VIIPER` current fork architecture, not upstream VIIPER architecture.
2. The tracked Windows native ABI is pinned to usbip-win2 `v0.9.8.0` commit `83bd1f781d57ed6efdf15530c55710cf5d4482bc`.
3. The old v0.9.7.7-only `attachIOCTL` layout is removed.
4. `attachIOCTL` includes `Serial[16]` and one-byte `WskEvents` in the upstream v0.9.8.0 order.
5. Native request size is pinned by test to 1116 bytes on Windows/amd64.
6. `PortOutput` remains at offset 4.
7. Native output-prefix length remains exactly 8 bytes.
8. `Serial` offset is pinned to 1097.
9. `WskEvents` offset is pinned to 1113 and its size to 1 byte.
10. Native attach always sends `WskEvents=true`.
11. Native attach keeps IPv4 loopback host `127.0.0.1` and current bus/service semantics.
12. No fabricated serial is introduced.
13. Tracked command attach always passes `--receive-mode=low-latency`.
14. Any still-reachable legacy Windows command attach also explicitly selects low-latency.
15. No user/config/API receive-mode selector is introduced.
16. Native success still records the exact positive imported port.
17. Exact stored backend/port detach behavior is unchanged.
18. `ErrAttachmentOutcomeUnknown` still prevents command fallback.
19. Known pre-ownership native failure still has at most one command fallback.
20. Command terse-port validation remains strict.
21. Public libVIIPER C ABI is unchanged.
22. Typed-device ownership/lifecycle semantics are unchanged.
23. Windows `internal/server/api` tests are executed in CI, not only compiled indirectly.
24. `FORK_ARCHITECTURE.md` documents the v0.9.8.0 pin and low-latency policy.
25. `docs/libviiper/fork-api.md` documents v0.9.8.0 as the supported tracked Windows package and removes v0.9.7.7 as a second supported ABI.
26. Full Go tests/vet/format checks pass.
27. Canonical `just build-libVIIPER Release` succeeds on Windows.
28. Existing canonical export/header/manifest verification passes.
29. Real usbip-win2 0.9.8.0 native attach/detach is hardware/runtime-smoke-tested before downstream Addon adoption.
30. No unrelated USB transport/controller refactor is mixed into the PR.

---

# 24. Recommended PR description

Use a concise PR description similar to:

```markdown
## Summary

- Migrate the fork's tracked Windows localhost attach ABI from usbip-win2 0.9.7.7 to 0.9.8.0.
- Always select the usbip-win2 low-latency / WSK-event receive path for native and command attach.
- Preserve exact imported-port ownership, exact-port detach, native-first fallback, and unknown-outcome fail-close behavior.
- Add Windows CI coverage for the v0.9.8.0 ABI contract and update fork architecture/API documentation.

## Non-goals

- No 0.9.7.7 compatibility layer.
- No receive-mode user setting.
- No SteamAddonforClaw package/installer update in this PR.
- No public libVIIPER ABI change.
```

---

# 25. Follow-up after PR2

After PR2 is merged and a successful **main-branch canonical Windows libVIIPER artifact** exists, the next work belongs in SteamAddonforClaw.

Downstream PR3 should:

```text
1. adopt the exact successful main-branch VIIPER commit/artifact;
2. replace bundled usbip-win2 0.9.7.7 with 0.9.8.0;
3. update Addon installer filename/version/SHA-256/publish verification;
4. exercise the already-merged Addon UpdateRequired path using a real 0.9.7.7 → 0.9.8.0 migration;
5. reboot when required;
6. verify exact 0.9.8.0 package readiness;
7. perform Full1902 MSI Claw lifecycle validation with the new low-latency VIIPER runtime.
```

That Addon PR is the correct place to validate the complete product lifecycle:

```text
Velopack delivers updated Addon files
→ Addon sees old usbip package
→ UpdateRequired
→ elevated prerequisite setup installs 0.9.8.0
→ exact version verified
→ new canonical libVIIPER is allowed to attach
→ Full1902 controller ownership/presentation resumes safely
```

Do not pre-implement that integration inside VIIPER PR2.
