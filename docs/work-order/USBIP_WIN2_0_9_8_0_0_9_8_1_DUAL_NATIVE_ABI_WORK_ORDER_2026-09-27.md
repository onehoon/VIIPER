# Work Order — usbip-win2 0.9.8.0 / 0.9.8.1 Dual Native ABI Support

## Target

Repository:

```text
onehoon/VIIPER
```

Implementation branch: create a fresh implementation branch from the latest `main`.

Reviewed main baseline at design time:

```text
abc9bd0aa1dc061aca92f75cdf6069e259de9476
Add Xbox360 USB/IP OUT boundary trace (#52)
```

Implement from the latest `main` at execution time. Do not undo later main changes.

This is a focused Windows usbip-win2 native-attach compatibility PR. It must not change controller presentation policy, USB/IP transport scheduling, zero-copy receive policy, public libVIIPER C ABI, or the existing tracked ownership/fail-close contract.

---

## 1. Goal

Make fork VIIPER's Windows native `PLUGIN_HARDWARE` attach path explicitly support both official usbip-win2 releases:

```text
0.9.8.0
0.9.8.1
```

The installed package version must be determined **before any native attach IOCTL is submitted**.

VIIPER must then construct the exact Microsoft C++ ABI layout for that installed version and issue exactly one native `DeviceIoControl` attempt.

Required high-level behavior:

```text
read installed usbip-win2 DisplayVersion
        |
        +-- 0.9.8.0 -> build 0.9.8.0 plugin_hardware (1120 bytes)
        |
        +-- 0.9.8.1 -> build 0.9.8.1 plugin_hardware (1124 bytes)
        |
        +-- missing / malformed / other
                -> do not submit native PLUGIN_HARDWARE
                -> known pre-submit failure
                -> existing fallback policy decides what happens next
```

Never probe ABI compatibility by submitting one layout and retrying another layout after failure.

---

## 2. Source review / authoritative references

### 2.1 usbip-win2 0.9.8.0

Repository:

```text
vadimgrn/usbip-win2
tag: v.0.9.8.0
commit: 83bd1f781d57ed6efdf15530c55710cf5d4482bc
```

Relevant upstream files:

```text
include/usbip/vhci.h
drivers/ude/vhci_ioctl.cpp
userspace/libusbip/src/vhci.cpp
userspace/innosetup/setup.iss
```

The 0.9.8.0 location base is:

```cpp
struct imported_device_location
{
    int port;

    char busid[BUS_ID_SIZE];
    char service[32];
    char host[1025];
};
```

The native attach request is:

```cpp
struct plugin_hardware : base, imported_device_location
{
    char serial[SERIAL_BUFSZ];
    bool wsk_events;
};
```

The already-correct fork mapping on current main is:

```text
Size offset       = 0
Port offset       = 4
BusID offset      = 8
Service offset    = 40
Host offset       = 72
Serial offset     = 1100
WskEvents offset  = 1116
request size      = 1120
response prefix   = 8
```

The explicit three-byte tail padding after `host[1025]` is required by the MSVC C++ base-subobject layout and must remain.

### 2.2 usbip-win2 0.9.8.1

Repository:

```text
vadimgrn/usbip-win2
tag: v.0.9.8.1
commit: 55e1fa7f0c2157017b02dc1f3236e98169a535e4
```

Relevant upstream change:

```text
6b3af1f — ude: add location_hash to imported_device_location
```

0.9.8.1 changes the location base to:

```cpp
struct imported_device_location
{
    int port;
    ULONG location_hash; // OUT, hash(host,service,busid)

    char busid[BUS_ID_SIZE];
    char service[32];
    char host[1025];
};
```

This is an ABI change because `plugin_hardware` inherits `imported_device_location`.

The required 0.9.8.1 Microsoft-layout mapping is:

```text
Size offset          = 0
Port offset          = 4
LocationHash offset  = 8
BusID offset         = 12
Service offset       = 44
Host offset          = 76
Serial offset        = 1104
WskEvents offset     = 1120
request size         = 1124
response prefix      = 8
```

Derivation:

```text
imported_device_location raw fields:
port          4
location_hash 4
busid        32
service      32
host       1025
----------------
raw        1097

alignment = 4
sizeof(imported_device_location) = 1100

base size = 4
location base starts at 4
serial starts at 4 + 1100 = 1104
serial[16] ends at 1120
wsk_events is at 1120
final object alignment rounds sizeof(plugin_hardware) to 1124
```

The driver still validates the complete request size:

```cpp
WdfRequestRetrieveInputBuffer(
    request,
    sizeof(*r),
    reinterpret_cast<PVOID*>(&r),
    &length);

if (length != sizeof(*r))
    return STATUS_INVALID_BUFFER_SIZE;

if (r->size != length)
    return USBIP_ERROR_ABI;
```

Therefore a 0.9.8.0 1120-byte request must not be sent to a 0.9.8.1 driver.

### 2.3 Location hash semantics

In 0.9.8.1, `location_hash` is explicitly an **OUT / driver-computed value**.

The upstream userspace assignment path expects it to be zero before submission.

VIIPER must therefore:

```text
LocationHash = 0 on request construction
```

Do not compute, persist, compare, or promote `location_hash` into VIIPER attachment ownership.

VIIPER's authoritative successful-attach ownership remains:

```text
backend + exact positive imported port
```

### 2.4 Installed package version source

usbip-win2's official Inno Setup installer keeps the same AppId across these releases:

```text
{199505b0-b93d-4521-a8c7-897818e0205a}
```

and uses the application executable version as `AppVersion`.

Windows therefore exposes the installed package version through the standard Inno uninstall entry:

```text
HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\
{199505b0-b93d-4521-a8c7-897818e0205a}_is1

DisplayVersion
```

SteamInputAddonforClaw already uses this exact registry entry and `DisplayVersion` as its usbip-win2 package-version authority in:

```text
src/SteamInputAddonforClaw/Prerequisites/UsbIpWin2Provisioning.cs
WindowsUsbIpWin2PackageProbe
```

Use the same official package evidence in VIIPER.

Important lifecycle qualification:

Both official 0.9.8.0 and 0.9.8.1 Inno Setup scripts declare:

```text
AlwaysRestart=yes
```

`DisplayVersion` is the installed package record. It is the correct package-version authority for choosing the supported ABI, but it is not independent proof that a newly installed kernel driver is already the driver currently loaded by Windows if a required restart has not yet completed.

Therefore:

```text
runtime ABI selection
    -> use official package DisplayVersion

hardware validation after installing/switching usbip-win2 versions
    -> complete the installer-required Windows restart first
    -> only then launch the canonical VIIPER validation build
```

Do not add a driver-hash inspector, loaded-driver version authority, INF parser, or trial IOCTL mechanism solely to defend against a deliberately deferred required restart. Treat completed restart after install/version transition as an explicit validation/adoption precondition.

Do not create a second version authority based on driver hashes, timestamps, INF scraping, executable path guessing, or trial IOCTLs.

---

## 3. Current fork behavior that must be preserved

Read current versions of:

```text
FORK_ARCHITECTURE.md
docs/libviiper/fork-api.md

internal/server/api/autoattach.go
internal/server/api/autoattach_contract.go
internal/server/api/autoattach_windows.go
internal/server/api/autoattach_windows_test.go
internal/server/api/config_windows.go

lib/viiper/attachment_test.go
lib/viiper/classified_attachment_test.go
lib/viiper/attach_invariants_test.go
lib/viiper/attachment_state_query_test.go
```

Also read the historical work orders for context only:

```text
docs/work-order/PR2_USBIP_WIN2_0_9_8_0_LOW_LATENCY_ATTACH_WORK_ORDER.md
docs/work-order/PR3_USBIP_WIN2_0_9_8_0_MSVC_ABI_PADDING_FIX_WORK_ORDER.md
```

Do not treat the old PR2 low-latency policy as current. Current main has intentionally restored Windows localhost attach to **zero-copy**.

Current required transport policy remains:

```text
native WskEvents = false
command --receive-mode=zero-copy
```

Do not change it in this PR.

Current tracked attachment safety remains:

```text
native pre-submit failure
    -> known failure
    -> existing command fallback may run

native DeviceIoControl submitted and returns error
    -> ErrAttachmentOutcomeUnknown
    -> no command fallback

unexpected native response length
    -> ErrAttachmentOutcomeUnknown
    -> no command fallback

non-positive native import port
    -> ErrAttachmentOutcomeUnknown
    -> no command fallback

success
    -> retain exact backend + exact positive import port
    -> detach that exact port only
```

Preserve this contract exactly.

---

## 4. Required design

### 4.1 Detect ABI before device submission

Add one small Windows-only installed-version reader.

Preferred implementation uses the already-present `golang.org/x/sys` dependency:

```go
import "golang.org/x/sys/windows/registry"
```

Read:

```text
HKLM
SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\
{199505b0-b93d-4521-a8c7-897818e0205a}_is1
DisplayVersion
```

Use the 64-bit registry view matching the official x64 installer.

Do not shell out to PowerShell, `reg.exe`, WMIC, or `usbip -V` for native ABI selection.

A minimal internal enum is sufficient:

```go
type usbipWin2NativeABI uint8

const (
    usbipWin2NativeABIUnknown usbipWin2NativeABI = iota
    usbipWin2NativeABI0980
    usbipWin2NativeABI0981
)
```

Exact version mapping:

```text
"0.9.8.0" -> usbipWin2NativeABI0980
"0.9.8.1" -> usbipWin2NativeABI0981
anything else -> unsupported
```

Trimming surrounding whitespace is acceptable.

Do not add semver libraries.

Do not add a generalized package-version service or registry manager.

### 4.2 Detection must happen before native ownership can become ambiguous

The order should be:

```text
validate function arguments
-> read installed package version
-> choose exact native ABI
-> build exact request
-> discover VHCI interface
-> open handle
-> DeviceIoControl exactly once
-> validate exact 8-byte response prefix and positive port
```

If registry access fails, `DisplayVersion` is missing/malformed, or the version is unsupported:

```text
DeviceIoControl must not run
backendCalled=false
return an ordinary known pre-submit error
```

Do not classify a failure that occurs before native IOCTL submission as `ErrAttachmentOutcomeUnknown`.

The existing fallback layer may handle that known failure according to its current policy.

Do not add a special second fallback manager.

### 4.3 Keep two explicit ABI structs

Do not try to make one struct conditionally reinterpret both layouts.

Use two clear Windows structs.

Required conceptual shape:

```go
type attachIOCTL0980 struct {
    Size                          uint32
    PortOutput                    int32
    BusID                         [32]byte
    Service                       [niMaxServ]byte
    Host                          [niMaxHost]byte
    ImportedDeviceLocationPadding [3]byte
    Serial                        [serialBufSize]byte
    WskEvents                     bool
}

type attachIOCTL0981 struct {
    Size                          uint32
    PortOutput                    int32
    LocationHash                  uint32
    BusID                         [32]byte
    Service                       [niMaxServ]byte
    Host                          [niMaxHost]byte
    ImportedDeviceLocationPadding [3]byte
    Serial                        [serialBufSize]byte
    WskEvents                     bool
}
```

Do not embed one ABI struct inside another.

Do not use a packed byte serializer, reflection marshaller, CGo layout shim, generated ABI layer, or version-strategy class hierarchy.

Two external ABIs are supported; two explicit structs are simpler and easier to audit.

### 4.4 Request construction

For both versions preserve:

```text
Size      = exact sizeof(selected request)
Port      = initially zero / driver output
BusID     = exact exported "<bus>-<device>"
Service   = exact local VIIPER USB/IP listen port
Host      = "127.0.0.1"
Serial    = zero-filled unless an existing authoritative source already exists
WskEvents = false
padding   = zero
```

For 0.9.8.1 additionally preserve:

```text
LocationHash = 0
```

Do not make receive mode version-dependent.

### 4.5 Keep the native DeviceIoControl seam small

Current `nativeAttachOps.pluginHardware` is typed to the single 0.9.8.0 `*attachIOCTL`.

It must be adjusted so tests can submit either explicit layout without creating a generalized ABI framework.

One acceptable minimal direction is a raw request seam:

```go
pluginHardware func(
    handle windows.Handle,
    request unsafe.Pointer,
    inputLength uint32,
    outputLength uint32,
) (bytesReturned uint32, err error)
```

The production implementation remains one thin `windows.DeviceIoControl` call.

Tests may cast the request to `*attachIOCTL0980` or `*attachIOCTL0981` based on the already-selected ABI/input length.

An equally small pair of typed helper calls is acceptable if it produces less code.

Do not introduce:

- an ABI manager;
- a serializer interface;
- an attach request factory hierarchy;
- reflection;
- generic registry abstractions;
- another ownership object.

### 4.6 Output prefix remains common

The response required by VIIPER remains only:

```text
Size + PortOutput prefix = 8 bytes
```

For both versions:

```text
outputLength = 8
bytesReturned must equal 8
PortOutput must be > 0
```

Do not read `location_hash` as part of successful ownership validation.

The native driver may modify more of the METHOD_BUFFERED system buffer internally, but VIIPER's authoritative result remains the exact positive import port returned in the established output prefix.

### 4.7 Detach ABI does not change

Both releases retain:

```cpp
struct plugout_hardware : base
{
    int port;
};
```

Therefore:

```text
sizeof(plugout_hardware) = 8
```

Do not add a version branch to native detach.

Continue detaching only the exact positive port retained from the successful attach.

---

## 5. Registry version reader test seam

Tests must never depend on the developer machine's real usbip-win2 installation.

Add the smallest injectable seam to the existing Windows native test operations.

For example, extending `nativeAttachOps` with a function such as:

```go
readInstalledVersion func() (string, error)
```

is acceptable.

Production uses the real registry reader.

Tests return deterministic values:

```text
0.9.8.0
0.9.8.1
unsupported
missing/error
```

Do not add a mock framework.

Do not mutate HKLM from tests.

---

## 6. Required Windows ABI tests

Update `internal/server/api/autoattach_windows_test.go`.

### 6.1 Pin the 0.9.8.0 ABI independently

Keep a dedicated numeric ABI test proving:

```text
Size offset       = 0
Port offset       = 4
BusID offset      = 8
Service offset    = 40
Host offset       = 72
Serial offset     = 1100
WskEvents offset  = 1116
WskEvents size    = 1
request size      = 1120
response size     = 8
```

Do not weaken the existing 0.9.8.0 regression coverage.

### 6.2 Add a dedicated 0.9.8.1 ABI test

Pin:

```text
Size offset          = 0
Port offset          = 4
LocationHash offset  = 8
BusID offset         = 12
Service offset       = 44
Host offset          = 76
Serial offset        = 1104
WskEvents offset     = 1120
WskEvents size       = 1
request size         = 1124
response size        = 8
```

Also keep:

```text
BUS_ID_SIZE = 32
NI_MAXSERV  = 32
NI_MAXHOST  = 1025
SERIAL_BUFSZ = 16
plugout_hardware size = 8
```

### 6.3 Version selection tests

Prove:

```text
DisplayVersion=0.9.8.0 -> 0.9.8.0 request path only
DisplayVersion=0.9.8.1 -> 0.9.8.1 request path only
```

For each, verify exactly one native submit.

### 6.4 0.9.8.0 request content

Verify:

```text
Size = 1120
BusID correct
Service correct
Host = 127.0.0.1
padding = zero
Serial = zero
WskEvents = false
```

### 6.5 0.9.8.1 request content

Verify:

```text
Size = 1124
LocationHash = 0
BusID correct
Service correct
Host = 127.0.0.1
padding = zero
Serial = zero
WskEvents = false
```

### 6.6 Unsupported / missing version must not submit

For each of:

```text
DisplayVersion missing
registry read error
DisplayVersion malformed
DisplayVersion = 0.9.7.8
DisplayVersion = 0.9.8.2
```

prove:

```text
native DeviceIoControl call count = 0
error is a known pre-submit failure
error is NOT ErrAttachmentOutcomeUnknown
```

Do not require support for those versions.

### 6.7 Never retry another ABI after submission

For both supported versions, make the fake native `DeviceIoControl` return an error after it is called.

Prove:

```text
native call count = 1
result = ErrAttachmentOutcomeUnknown
no second ABI request is submitted
tracked command fallback is not invoked
```

This is the critical safety regression test.

A submitted 1120-byte request must never be followed by 1124, and a submitted 1124-byte request must never be followed by 1120.

### 6.8 Response validation remains identical

For both selected ABIs verify the condition directly:

```text
bytesReturned == 8 && PortOutput > 0
    -> success

otherwise
    -> ErrAttachmentOutcomeUnknown
```

In particular:

```text
bytesReturned != 8 -> ErrAttachmentOutcomeUnknown
PortOutput <= 0    -> ErrAttachmentOutcomeUnknown
```

### 6.9 Existing timing semantics and required ABI evidence

Preserve the current low-volume `attachment-timing` diagnostics and extend the **native attach** timing record with mandatory ABI evidence.

Every native attach timing record must include:

```text
installedVersion
selectedABI
inputLength
```

Required interpretation:

```text
DisplayVersion = 0.9.8.0
    installedVersion = "0.9.8.0"
    selectedABI      = "0.9.8.0"
    inputLength      = 1120

DisplayVersion = 0.9.8.1
    installedVersion = "0.9.8.1"
    selectedABI      = "0.9.8.1"
    inputLength      = 1124

unsupported/malformed observed version
    installedVersion = observed value when available
    selectedABI      = "unsupported"
    inputLength      = 0

registry value unavailable/read failure
    installedVersion = empty or one stable unavailable representation
    selectedABI      = "unsupported"
    inputLength      = 0
```

Keep the representation deterministic and cover it with focused tests. Do not log a guessed request size when no ABI was selected.

Existing semantics must remain:

```text
unsupported/missing version -> backendCalled=false
DeviceIoControl submitted   -> backendCalled=true
submitted failure           -> result=unsafe-outcome-unknown
successful request          -> result=success
```

The mandatory fields are diagnostic evidence only. They must not influence fallback, ownership, retry, or teardown behavior.

Do not build a new timing/logging subsystem solely for ABI detection. Reuse the existing `attachment-timing` record.

---

## 7. Existing tests and contracts that must remain passing

Preserve all current tests for:

```text
known native pre-submit failure -> command fallback exactly once
unknown native outcome -> no command fallback
command --terse exact positive-port parsing
zero-copy command argument
native exact positive-port ownership
exact-port detach
unknown detach -> fail-close
attachment-state query
typed device removal
server close-failed behavior
managed transport drain
X360 USB/IP OUT diagnostic tracing
```

Do not weaken an existing assertion just to accommodate the new ABI branch.

---

## 8. Current documentation updates required in implementation PR

Update current authoritative fork documentation:

```text
FORK_ARCHITECTURE.md
docs/libviiper/fork-api.md
```

Replace the current single-version claim:

```text
tracked native ABI is pinned to usbip-win2 v0.9.8.0
later versions are not claimed compatible
```

with the factual supported matrix:

```text
Windows tracked native attach supports:
- usbip-win2 0.9.8.0 native plugin_hardware ABI
- usbip-win2 0.9.8.1 native plugin_hardware ABI

Selection is based on the official installed package DisplayVersion
before native DeviceIoControl submission.
```

Document that:

```text
0.9.8.0 -> 1120-byte request
0.9.8.1 -> 1124-byte request with zero-initialized location_hash
both -> zero-copy
both -> exact positive imported-port ownership
detach -> common 8-byte plugout_hardware ABI
unknown versions -> no native ABI guess
```

Do not rewrite historical work orders PR2/PR3. They remain historical implementation records.

If another current document still states that only 0.9.8.0 is supported, update that current statement only.

---

## 9. Explicit non-goals

Do not include:

- SteamInputAddonforClaw package upgrade to 0.9.8.1;
- Addon prerequisite-policy changes;
- installer download/SHA changes;
- automatic usbip-win2 installation;
- driver binary hash matching;
- INF version scraping;
- `usbip -V` subprocess detection for native ABI selection;
- trial IOCTL ABI probing;
- 1120 -> 1124 retry;
- 1124 -> 1120 retry;
- support for 0.9.7.x;
- support for hypothetical 0.9.8.2+;
- dynamic arbitrary-ABI parsing;
- `location_hash` ownership tracking;
- a new attach manager;
- a new state machine;
- a new registry abstraction layer;
- low-latency mode;
- user-selectable receive mode;
- USB/IP server scheduling changes;
- rumble parser/guard changes;
- X360 boundary-trace changes;
- HidHide changes;
- Steam Deck identity changes;
- public libVIIPER C ABI changes.

This PR is only the native ABI compatibility foundation required before the Addon can consider adopting usbip-win2 0.9.8.1.

---

## 10. Expected implementation scope

Expected primary source files:

```text
internal/server/api/autoattach_windows.go
internal/server/api/autoattach_windows_test.go
FORK_ARCHITECTURE.md
docs/libviiper/fork-api.md
```

A very small Windows-only helper file is acceptable if it makes registry reading materially clearer, but do not create a package/service solely for one registry value.

`go.mod` should not require a new module dependency because `golang.org/x/sys` is already present.

No public generated header changes are expected.

---

## 11. Required verification

Run the repository's normal checks from current main plus focused Windows coverage.

At minimum:

```text
go test -count=1 ./internal/server/api
go test ./lib/viiper/...
go test ./...
go vet ./...
git diff --check
just build-libVIIPER Release
go run ./lib/viiper/exportverify   -header dist/libVIIPER/libVIIPER.h   -def dist/libVIIPER/libVIIPER.def
```

The Windows-only ABI and registry-selection tests must execute on a Windows CI runner.

Expected public ABI result:

```text
no new exported C function
no generated libVIIPER.h semantic change
no export-list change
```

If the public C ABI changes, stop and investigate.

---

## 12. Hardware validation after implementation

Do not update SteamInputAddonforClaw's prerequisite pin yet.

First produce a canonical VIIPER artifact and validate both package versions separately on real MSI Claw hardware.

Because both official installers specify `AlwaysRestart=yes`, every install or version transition used for this validation has a hard preparation rule:

```text
install/switch target usbip-win2 package
-> complete the required Windows restart
-> sign back in
-> only then launch the canonical VIIPER validation build
```

Do not treat a newly written `DisplayVersion` value, before that restart, as proof that the currently loaded kernel driver matches the package record.

### 12.1 usbip-win2 0.9.8.0 regression

With official 0.9.8.0 installed:

```text
1. Install/switch to official usbip-win2 0.9.8.0 and complete the required Windows restart.
2. After reboot, confirm registry DisplayVersion = 0.9.8.0.
3. Start canonical VIIPER path.
4. Capture the native attachment-timing record and confirm:
       installedVersion = 0.9.8.0
       selectedABI = 0.9.8.0
       inputLength = 1120
5. Confirm native attach returns bytesReturned=8 and port>0.
6. Confirm Xbox360 presentation works.
7. Confirm SteamDeck presentation transition works.
8. Confirm exact-port detach and reattach work.
9. Confirm Runtime restart recovery.
10. Confirm one Sleep/Resume cycle.
11. Confirm existing X360 output/rumble diagnostics remain operational.
```

This is the compatibility regression gate. 0.9.8.0 must not be broken by adding 0.9.8.1 support.

### 12.2 usbip-win2 0.9.8.1 validation

With official 0.9.8.1 installed:

```text
1. Install/switch to official usbip-win2 0.9.8.1 and complete the required Windows restart.
2. After reboot, confirm registry DisplayVersion = 0.9.8.1.
3. Start canonical VIIPER path.
4. Capture the native attachment-timing record and confirm:
       installedVersion = 0.9.8.1
       selectedABI = 0.9.8.1
       inputLength = 1124
5. Confirm native attach returns bytesReturned=8 and port>0.
6. Confirm Xbox360 presentation works.
7. Confirm SteamDeck presentation transition works.
8. Confirm exact-port detach and reattach work.
9. Confirm Runtime restart recovery.
10. Confirm one Sleep/Resume cycle.
11. Re-run the existing Xbox360 rumble reproduction with the current boundary trace enabled.
```

`LocationHash=0` is request-construction evidence and must be proven by the deterministic 0.9.8.1 unit test in section 6.5. Do not add hot-path logging or another runtime inspector solely to expose that field during hardware validation.

The rumble comparison is observational at this stage. usbip-win2 0.9.8.1 contains upstream OUT-transfer fixes, but this PR must not assume those fixes resolve the existing latch investigation.

Do not add VIIPER behavior changes based solely on the version upgrade without field evidence.

---

## 13. Acceptance criteria

The implementation is complete only when all are true:

1. VIIPER recognizes official installed `DisplayVersion` 0.9.8.0 and 0.9.8.1.
2. Version detection occurs before any native attach IOCTL submission.
3. 0.9.8.0 uses the exact 1120-byte ABI.
4. 0.9.8.1 uses the exact 1124-byte ABI.
5. 0.9.8.1 `LocationHash` is zero-initialized and is not promoted into VIIPER ownership state.
6. Both ABIs use `WskEvents=false` / zero-copy.
7. Both require the exact 8-byte response prefix and a positive import port.
8. Missing/unsupported package version causes zero native IOCTL submissions.
9. Once native `DeviceIoControl` is submitted, a failure never triggers another ABI attempt.
10. Existing unknown-outcome fail-close behavior is unchanged.
11. Existing command fallback behavior for known pre-submit failures is unchanged.
12. Native detach remains the common exact-port 8-byte `plugout_hardware` path.
13. Current 0.9.8.0 behavior remains covered and passing.
14. New 0.9.8.1 layout and selection tests pass on Windows.
15. Full tests, vet, Release libVIIPER build, and export verification pass.
16. No public libVIIPER C ABI changes occur.
17. Current architecture/API docs state dual 0.9.8.0/0.9.8.1 native ABI support and preserve zero-copy/exact-port/fail-close semantics.
18. SteamInputAddonforClaw remains unchanged until separate hardware validation and adoption work.
19. Native attach `attachment-timing` records expose `installedVersion`, `selectedABI`, and `inputLength` so hardware ABI selection is auditable from captured logs.
20. Hardware validation for each installed usbip-win2 version begins only after the installer-required Windows restart has completed.

---

## 14. Implementation guidance

Keep this PR small.

The desired architecture is:

```text
official package DisplayVersion
        |
        v
small exact version switch
        |
        +-- explicit 0980 struct
        +-- explicit 0981 struct
        |
        v
one DeviceIoControl submission
        |
        v
existing exact-port ownership / fail-close
```

The goal is not to future-proof every possible usbip-win2 ABI.

The goal is to support the two concrete official ABIs we actually need while preserving one clear ownership authority and one safe attach/teardown path.
