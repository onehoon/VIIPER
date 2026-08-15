# VIIPER Fork Architecture

## Purpose

This fork carries the canonical embedded library and typed device work used by
SteamInputAddonforClaw. It preserves the upstream-compatible API surface while
keeping fork-specific lifecycle and ownership guarantees explicit.

For consumer-facing exports, lifecycle examples, and platform limits, see
[`docs/libviiper/fork-api.md`](docs/libviiper/fork-api.md). This document is the
architectural source of truth for the fork.

## Current embedded architecture

- `lib/viiper` is the canonical embedded C ABI for new fork development.
- Typed libVIIPER device handles are the preferred ownership model.
- The fork exposes typed Steam Controller and Steam Deck devices, together with
  Xbox360 and the other supported device families.
- `AttachUSBDevice` and `DetachUSBDevice` provide tracked localhost USB/IP
  attachment lifecycle for typed device handles on Windows.
- The tracked native ABI is pinned to usbip-win2 `v0.9.7.7`
  (`7c219953101cc5d0ec9a0bcb3eb87259cf72bedd`). Later versions are unsupported
  until their ABI and runtime behavior are explicitly validated.
- Non-Windows builds remain ABI-compatible, but tracked localhost attachment
  is unsupported and fails safely without recording ownership.
- `clib/` remains a compatibility flat API. New integrations must not use it
  as their architectural base.

## Lifetime model

```text
process lifetime        -> libVIIPER module/runtime
USB server/bus lifetime -> long-lived caller-owned resources
logical device lifetime -> typed device handles
Windows attachment      -> explicit USB/IP attach/detach ownership
report routing          -> neutral/live device state
```

A typed device removal ends only the logical device lifetime. It does not
schedule empty-bus cleanup. Callers own buses and explicitly invoke
`RemoveUSBBus` or close the owning USB server.

## Steam Deck device

The Steam Deck wrapper provides:

```text
CreateSteamDeckDevice
SetSteamDeckDeviceState
SetSteamDeckOutputCallback
RemoveSteamDeckDevice
RemoveSteamDeckDeviceEx
```

Its default identity is `28DE:1205`. The wrapper shares generic USB identity,
attachment, detachment, typed-handle, and removal lifecycle APIs. The generic
output callback carries the existing normalized Steam Deck host-output stream;
it does not add MSI Claw-specific behavior or claim Addon rumble adoption.
Basic non-gyro Steam Deck input has been validated on MSI Claw EX. Lifecycle
and recovery validation remains pending; rumble/haptics and gyro remain
separate feature tracks.

## Lifecycle and failure rules

Canonical server wrappers use `active`, `closing`, `close-failed`, and
`closed` lifecycle states. Server-owned mutation APIs share one lifecycle
boundary. Partial close is fail-closed.

`CloseUSBServer` establishes the closing boundary before taking its
authoritative bus snapshot, tears down buses in ascending BusID order, and
waits for callback and managed USB/IP transport work before returning.

Successful logical teardown is not rolled back when a later bus or transport
operation fails. A retry processes only remaining authoritative buses.

Attached typed devices store their exact attachment backend and positive
usbip-win2 import port. Detach uses that stored token only. Unknown attach or
detach outcomes transition the owning server to `close-failed`; no destructive
automatic retry is attempted.

## Build and CI

Build the canonical embedded library with:

```text
just build-libVIIPER Release
```

Canonical changes require focused tests, lifecycle race coverage, vet, the
Windows shared-library build, generated-header validation, and export checks.

## Upstream synchronization

Keep fork-specific changes localized. Avoid unnecessary changes to device and
USB/IP core code so future synchronization remains reviewable.
