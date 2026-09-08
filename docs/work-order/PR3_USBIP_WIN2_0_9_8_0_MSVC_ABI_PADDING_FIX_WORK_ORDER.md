# Work Order — PR3: Fix usbip-win2 0.9.8.0 MSVC `plugin_hardware` ABI Padding

## Status

Implementation work order for a **hardware-reproduced production blocker** in the Steam Addon for Claw integration.

Target repository:

```text
repository: onehoon/VIIPER
branch:     main
baseline:   e00fbf01277a2c354a32b0e54418a9bd917a05ae
```

This work order is a focused correction to the usbip-win2 `v0.9.8.0` migration implemented by the previous VIIPER PR.

It does **not** redesign attachment ownership, fallback behavior, low-latency policy, VIIPER lifecycle, or the public C ABI.

---

# 1. Goal

Fix the Windows native `PLUGIN_HARDWARE` request layout so VIIPER sends the exact Microsoft C++ ABI layout expected by usbip-win2 `v0.9.8.0`.

The current Go binding incorrectly models the upstream C++ multiple-inheritance layout as a flat C-style concatenation and therefore omits the tail padding of the `imported_device_location` base subobject.

Current broken native request:

```text
Serial offset     = 1097
WskEvents offset  = 1113
input size        = 1116
```

Required usbip-win2 `v0.9.8.0` Windows/MSVC layout:

```text
Size offset       = 0
PortOutput offset = 4
BusID offset      = 8
Service offset    = 40
Host offset       = 72
Serial offset     = 1100
WskEvents offset  = 1116
WskEvents size    = 1
struct size       = 1120
output prefix     = 8
plugoutIOCTL size = 8
```

After this PR:

```text
native tracked attach → exact v0.9.8.0 ABI request → low-latency flag true
known pre-submit failure → existing single command fallback policy remains unchanged
submitted native IOCTL with unknown result → existing fail-close policy remains unchanged
successful attach → exact positive imported port ownership remains unchanged
public libVIIPER C ABI → unchanged
```

---

# 2. Why this is a blocker

2026-09-09 MSI Claw hardware validation reproduced the following real product lifecycle:

```text
usbip-win2 0.9.8.0 installed and accepted as Ready
Center M Disabled
PID1902 physical controller present
DirectInput acquire succeeds
first physical input read succeeds
HidHide isolation succeeds
physical Addon ownership becomes Owned
Xbox360 presentation selected
VIIPER native PLUGIN_HARDWARE attach attempted
usbip-win2 interface discovered
control-device handle opened
DeviceIoControl fails immediately
attachment classified ErrAttachmentOutcomeUnknown
canonical runtime fails closed
virtual Xbox360 presentation never appears
```

Because the physical PID1902 controller is correctly hidden by HidHide while the virtual presentation is absent, the user experiences this as **no usable controller at all**.

This is not a theoretical timing race. It is a deterministic ABI mismatch on the normal supported Full1902 startup path.

The Full1902 product contract still requires:

```text
Center M Disabled
→ Addon owns PID1902
→ physical input is isolated
→ exactly one VIIPER virtual presentation is live
```

Do not weaken HidHide isolation or fail-close behavior to mask the attach failure. Fix the native ABI request.

---

# 3. Root cause

## 3.1 Upstream ABI source of truth

Use exactly:

```text
repository: vadimgrn/usbip-win2
tag:        v.0.9.8.0
commit:     83bd1f781d57ed6efdf15530c55710cf5d4482bc
```

Relevant upstream files:

```text
include/usbip/vhci.h
userspace/libusbip/src/vhci.cpp
drivers/ude/vhci_ioctl.cpp
```

Upstream defines:

```cpp
struct base
{
    ULONG size;
};

struct imported_device_location
{
    int port;
    char busid[BUS_ID_SIZE];
    char service[32];
    char host[1025];
};

struct plugin_hardware : base, imported_device_location
{
    char serial[SERIAL_BUFSZ];
    bool wsk_events;
};
```

The important detail is that this is **C++ multiple inheritance**, not a flat packed C struct.

## 3.2 The missed base-subobject padding

The raw fields of `imported_device_location` occupy:

```text
port       4 bytes
busid     32 bytes
service   32 bytes
host    1025 bytes
-----------------
raw      1093 bytes
```

That base type has 4-byte alignment because it contains `int port`.

Therefore its C++ object size is rounded to:

```text
sizeof(imported_device_location) = 1096
```

In `plugin_hardware`, the second base begins at offset 4:

```text
base                         offset    0, size 4
imported_device_location     offset    4, size 1096
next derived member          offset 1100
```

So the three bytes after `host[1025]` are **base-subobject tail padding** and are preserved before the derived `serial` member.

The correct Windows/MSVC layout is therefore:

```text
0       size[4]
4       port[4]
8       busid[32]
40      service[32]
72      host[1025]
1097    <3 bytes imported_device_location tail padding>
1100    serial[16]
1116    wsk_events[1]
1117    <3 bytes final object padding>
1120    end
```

## 3.3 Independent compiler verification

The upstream record definition was independently checked with Clang using the Microsoft x64 ABI target:

```text
clang++ -target x86_64-pc-windows-msvc -std=c++20 \
  -Xclang -fdump-record-layouts -fsyntax-only layout.cpp
```

The compiler record layout is:

```text
0    struct plugin_hardware
0      struct base (base)
0        UINT32 size
4      struct imported_device_location (base)
4        int port
8        char[32] busid
40       char[32] service
72       char[1025] host
1100   char[16] serial
1116   bool wsk_events
       [sizeof=1120, align=4]
```

This is the contract the Go binding must reproduce.

## 3.4 Why the current tests passed while hardware failed

The previous PR work order incorrectly stated:

```text
Serial offset    = 1097
WskEvents offset = 1113
struct size      = 1116
```

and explicitly instructed implementation not to add manual padding.

The Windows unit tests then pinned those incorrect values.

Current code therefore consistently proves only that Go matches the earlier mistaken calculation, not that Go matches the upstream C++ ABI.

This PR3 supersedes the previous work order **only for the numeric `plugin_hardware` layout assumptions and the tests derived from them**.

Do not rewrite the previous historical work order as if it never existed.

---

# 4. Why usbip-win2 rejects the current request

The upstream userspace library performs attach with its compiler-computed C++ object size:

```cpp
ioctl::plugin_hardware r{};
r.size = sizeof(r);

DeviceIoControl(
    dev,
    ctl,
    &r,
    sizeof(r),
    &r,
    outlen,
    &BytesReturned,
    nullptr);
```

The upstream driver retrieves and validates the full `plugin_hardware` input buffer and rejects a size mismatch:

```cpp
vhci::ioctl::plugin_hardware *r{};
size_t length;
auto st = WdfRequestRetrieveInputBuffer(
    request,
    sizeof(*r),
    reinterpret_cast<PVOID*>(&r),
    &length);

if (NT_ERROR(st)) {
    return st;
} else if (length != sizeof(*r)) {
    return STATUS_INVALID_BUFFER_SIZE;
} else if (r->size != length) {
    return USBIP_ERROR_ABI;
}
```

Current VIIPER sends:

```text
inputLength = 1116
Size        = 1116
```

while the v0.9.8.0 Windows ABI requires:

```text
inputLength = 1120
Size        = 1120
```

Therefore the request can fail before the driver reaches USB/IP connect/import processing.

The existing 8-byte output-prefix contract remains correct and must not change.

---

# 5. Required source review before editing

Read the current fork code first:

```text
FORK_ARCHITECTURE.md
docs/libviiper/fork-api.md
internal/server/api/autoattach_windows.go
internal/server/api/autoattach_windows_test.go
internal/server/api/autoattach_contract.go
internal/server/api/autoattach_contract_test.go
lib/viiper/attach_invariants_test.go
lib/viiper/classified_attachment_test.go
lib/viiper/ownership_invariants_test.go
```

Also re-read the previous migration work order:

```text
docs/work-order/PR2_USBIP_WIN2_0_9_8_0_LOW_LATENCY_ATTACH_WORK_ORDER.md
```

Treat its ownership/fallback/low-latency requirements as still authoritative except for the incorrect numeric ABI layout described above.

---

# 6. Required implementation

## 6.1 Correct `attachIOCTL`

Modify only the existing Windows ABI binding in:

```text
internal/server/api/autoattach_windows.go
```

Current broken shape:

```go
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

Required minimal shape:

```go
type attachIOCTL struct {
    Size       uint32
    PortOutput int32
    BusID      [32]byte
    Service    [niMaxServ]byte
    Host       [niMaxHost]byte
    _          [3]byte // MSVC imported_device_location base-subobject tail padding
    Serial     [serialBufSize]byte
    WskEvents  bool
}
```

With the existing 4-byte-aligned fields, Go will then apply the final trailing alignment automatically and produce:

```text
Serial offset    = 1100
WskEvents offset = 1116
sizeof           = 1120
```

Do not add a generalized ABI-marshalling layer, reflection-based serializer, packed byte-buffer builder, CGo dependency, or version-switching abstraction.

This is one known external Windows ABI and one explicit padding requirement.

A clearly named padding member is preferable to a more complex abstraction.

## 6.2 Preserve request initialization

Keep:

```go
var ioctlData attachIOCTL
ioctlData.Size = uint32(unsafe.Sizeof(ioctlData))
```

Continue filling:

```text
BusID     = exact exported <bus>-<device>
Service   = exact local USB/IP server port
Host      = 127.0.0.1
Serial    = zero-filled unless an existing authoritative source exists
WskEvents = true
```

Do not make low-latency conditional.

Do not write arbitrary bytes into the explicit padding field.

Zero initialization is correct.

## 6.3 Preserve output length

Keep:

```go
attachPortOutputLength = uint32(
    unsafe.Offsetof(attachIOCTL{}.PortOutput) +
        unsafe.Sizeof(attachIOCTL{}.PortOutput))
```

Expected:

```text
8
```

Do not change response validation:

```text
bytesReturned must equal 8
PortOutput must be > 0
otherwise attachment outcome remains unknown
```

## 6.4 Do not change detach

`plugoutIOCTL` remains:

```go
type plugoutIOCTL struct {
    Size uint32
    Port int32
}
```

Expected size remains 8.

Do not change exact-port detach ownership.

---

# 7. Preserve fail-close and fallback behavior exactly

The current tracked attach contract is correct and must stay unchanged:

```text
native failure before DeviceIoControl submission
→ known failure
→ command fallback may run exactly once under the existing contract

DeviceIoControl submitted and returns error
→ ErrAttachmentOutcomeUnknown
→ no command fallback

unexpected returned byte count
→ ErrAttachmentOutcomeUnknown
→ no command fallback

non-positive returned import port
→ ErrAttachmentOutcomeUnknown
→ no command fallback
```

Do not reinterpret the current hardware failure as safe for fallback merely because this PR identifies its likely driver-side cause.

The request was submitted to the driver, so the conservative unknown-outcome classification remains the correct generic runtime policy.

Do not add:

- retry loops;
- attach epochs;
- driver-state inspectors;
- post-failure VID/PID rediscovery;
- all-port cleanup;
- automatic command fallback after unknown native outcome;
- a second attachment authority.

Fixing the ABI should make the normal native path succeed without weakening safety.

---

# 8. Small diagnostic improvement required

The 2026-09-09 hardware log showed:

```text
result=unsafe-outcome-unknown
backendCalled=true
```

but did not preserve the underlying Win32 `DeviceIoControl` error in the normal VIIPER log.

That made a real native transport failure unnecessarily difficult to diagnose.

Add the smallest practical error log at the existing IOCTL failure branch.

Recommended shape:

```go
if ioctlErr != nil {
    logger.Error("native PLUGIN_HARDWARE DeviceIoControl failed",
        "error", ioctlErr,
        "inputLength", attachInputLength,
        "outputLength", attachPortOutputLength)

    err = fmt.Errorf(
        "%w: native PLUGIN_HARDWARE DeviceIoControl failed: %v",
        ErrAttachmentOutcomeUnknown,
        ioctlErr)
    return
}
```

Requirements:

- keep the returned error/classification unchanged;
- do not log the entire request buffer;
- do not add a new logging framework;
- do not add verbose per-report logging;
- one error record for the failed mutation is sufficient.

This is diagnostic hardening for a proven operation failure, not speculative telemetry expansion.

---

# 9. Required Windows ABI tests

Update:

```text
internal/server/api/autoattach_windows_test.go
```

The ABI test must pin the **correct Microsoft C++ layout**:

```go
func TestUSBIPWin20980NativeABIContract(t *testing.T) {
    var request attachIOCTL

    if got, want := unsafe.Offsetof(request.Size), uintptr(0); got != want {
        t.Fatalf("plugin_hardware size offset = %d, want %d", got, want)
    }
    if got, want := unsafe.Offsetof(request.PortOutput), uintptr(4); got != want {
        t.Fatalf("plugin_hardware port offset = %d, want %d", got, want)
    }
    if got, want := unsafe.Offsetof(request.BusID), uintptr(8); got != want {
        t.Fatalf("plugin_hardware busid offset = %d, want %d", got, want)
    }
    if got, want := unsafe.Offsetof(request.Service), uintptr(40); got != want {
        t.Fatalf("plugin_hardware service offset = %d, want %d", got, want)
    }
    if got, want := unsafe.Offsetof(request.Host), uintptr(72); got != want {
        t.Fatalf("plugin_hardware host offset = %d, want %d", got, want)
    }
    if got, want := unsafe.Offsetof(request.Serial), uintptr(1100); got != want {
        t.Fatalf("plugin_hardware serial offset = %d, want %d", got, want)
    }
    if got, want := unsafe.Offsetof(request.WskEvents), uintptr(1116); got != want {
        t.Fatalf("plugin_hardware WSK events offset = %d, want %d", got, want)
    }
    if got, want := unsafe.Sizeof(request.WskEvents), uintptr(1); got != want {
        t.Fatalf("plugin_hardware WSK events size = %d, want %d", got, want)
    }
    if got, want := attachInputLength, uint32(1120); got != want {
        t.Fatalf("plugin_hardware input length = %d, want %d", got, want)
    }
    if got, want := attachPortOutputLength, uint32(8); got != want {
        t.Fatalf("plugin_hardware output length = %d, want %d", got, want)
    }
    if got, want := unsafe.Sizeof(plugoutIOCTL{}), uintptr(8); got != want {
        t.Fatalf("plugout_hardware size = %d, want %d", got, want)
    }
}
```

Use current repository assertion style if preferred.

The test must specifically prevent regression back to:

```text
Serial=1097 / WskEvents=1113 / Size=1116
```

## 9.1 Update native request-content test

Current test hard-codes:

```go
captured.Size != 1116
```

Change the expected value to:

```text
1120
```

Prefer deriving the expected request size from the explicitly pinned ABI constant/test where practical, while still keeping at least one clear numeric ABI assertion.

Continue verifying:

```text
BusID = expected value
Service = expected port
Host = 127.0.0.1
Serial = zero-filled
WskEvents = true
```

The explicit padding field must remain zero-filled after normal request construction.

A focused assertion is acceptable:

```go
if captured.Padding != [3]byte{} { ... }
```

if the padding field is named rather than anonymous.

Do not invent a mock framework.

---

# 10. Preserve all existing ownership and transport tests

The following behavior must remain covered and passing:

```text
known native pre-submit failure → command fallback once
unknown native result → no command fallback
positive exact native port accepted
wrong byte count rejected as unknown
zero/negative port rejected as unknown
tracked backend retained
exact imported port retained
exact-port detach
unknown detach fail-close
low-latency command argument present exactly once
legacy command path remains low-latency if still reachable
```

Do not modify tests merely to weaken the existing ownership contract.

---

# 11. Windows CI requirement

The corrected ABI test is Windows-only and must execute in Windows CI.

The previous migration already added/uses the focused Windows `internal/server/api` test execution. Preserve that coverage.

At minimum the implementation PR must demonstrate:

```text
go test -count=1 ./internal/server/api
```

on a Windows runner.

Also run the existing repository-required checks and canonical libVIIPER build.

Do not add a new workflow or large test matrix for this fix.

---

# 12. Documentation handling

Do not rewrite the historical PR2 work order.

This PR3 work order is the corrective record and explicitly supersedes PR2's incorrect `plugin_hardware` numeric layout assumptions.

If current non-historical architecture/API documentation contains any of the incorrect values:

```text
Serial offset = 1097
WskEvents offset = 1113
plugin_hardware size = 1116
```

update those current documents to:

```text
Serial offset = 1100
WskEvents offset = 1116
plugin_hardware size = 1120
```

Do not broadly rewrite documentation that only states:

```text
usbip-win2 = v0.9.8.0
receive mode = low-latency
```

because those policies remain correct.

---

# 13. Explicit non-goals

Do not include any of the following:

- usbip-win2 0.9.7.7 compatibility;
- runtime driver-version detection inside VIIPER;
- a second ABI struct for legacy versions;
- user-selectable receive mode;
- zero-copy fallback;
- changes to SteamAddonforClaw prerequisite policy;
- HidHide changes;
- controller-mode/routing policy changes;
- public libVIIPER C ABI changes;
- controller identity changes;
- new attachment manager/state machine;
- retries after unknown native mutation outcome;
- broad refactoring of Windows auto-attach code.

This PR fixes one concrete ABI defect.

---

# 14. Expected implementation diff

The final implementation should remain small.

Expected primary files:

```text
internal/server/api/autoattach_windows.go
internal/server/api/autoattach_windows_test.go
```

Possible additional files only when required by current factual documentation/tests:

```text
FORK_ARCHITECTURE.md
docs/libviiper/fork-api.md
```

Do not create new runtime packages or abstractions.

---

# 15. Acceptance criteria

Implementation is complete when all of the following are true.

## ABI

```text
PortOutput offset = 4
BusID offset      = 8
Service offset    = 40
Host offset       = 72
Serial offset     = 1100
WskEvents offset  = 1116
WskEvents size    = 1
attach input size = 1120
attach output size = 8
plugout size      = 8
```

## Request policy

```text
Host = 127.0.0.1
Service = exact VIIPER USB/IP listen port
BusID = exact exported device bus/device ID
Serial = zero unless current authoritative source exists
padding = zero
WskEvents = true
```

## Ownership/failure policy

```text
native success → exact native backend + exact positive port
known pre-submit native failure → existing command fallback policy
unknown submitted native outcome → no fallback, fail closed
exact-port detach unchanged
```

## Diagnostics

A failed native `PLUGIN_HARDWARE` `DeviceIoControl` leaves the underlying Windows error visible in `libVIIPER.log` without changing classification.

## Verification

```text
Windows internal/server/api tests pass
existing Go tests pass
canonical Release libVIIPER build passes
public generated C header remains unchanged
no new exported C ABI symbols
```

---

# 16. Hardware validation after merge

After the corrected libVIIPER artifact is adopted by SteamAddonforClaw, validate on the real MSI Claw with usbip-win2 `0.9.8.0`:

```text
1. Cold boot with Center M Disabled.
2. Confirm Addon prerequisite status = Ready.
3. Confirm PID1902 DirectInput first valid state succeeds.
4. Confirm HidHide physical isolation succeeds.
5. Confirm initial Xbox360 native attach succeeds.
6. Confirm libVIIPER reports a positive exact import port.
7. Confirm the Xbox360 controller is visible and receives input.
8. Enter Steam/BPM and confirm SteamDeck presentation transition succeeds.
9. Leave Steam/BPM and confirm Xbox360 presentation returns.
10. Confirm output/rumble still works.
11. Restart the Addon Runtime and confirm normal ownership recovery.
12. Sleep/Resume once and verify presentation recovery.
```

The critical regression signal from the 2026-09-09 failure must disappear:

```text
Opened device handle
→ immediate unsafe-outcome-unknown
→ Xbox360AttachUnsafeOutcomeUnknown
```

Expected instead:

```text
Opened device handle
→ IOCTL completed, bytesReturned=8, portOutput>0
→ Successfully attached device via IOCTL
→ canonical attachment state = attached with exact native port
```

If the corrected 1120-byte ABI still produces a real `DeviceIoControl` failure, use the newly preserved Win32 error to diagnose that concrete failure. Do not add speculative compatibility/fallback machinery in advance.

---

# 17. Codex implementation guidance

Implement this as a focused bug-fix PR from current `main`.

Before editing:

1. inspect current main and confirm the broken values still exist;
2. verify upstream `v0.9.8.0` sources at commit `83bd1f781d57ed6efdf15530c55710cf5d4482bc`;
3. preserve all current ownership/fail-close invariants;
4. make the smallest ABI/layout correction;
5. update the focused Windows tests;
6. run required verification;
7. ensure the generated/public libVIIPER C ABI is unchanged.

Do not broaden the PR into general USB/IP refactoring.
