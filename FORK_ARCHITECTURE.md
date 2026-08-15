# VIIPER Fork Architecture

## Purpose

This fork carries additional embedded-library and device work for SteamInputAddonforClaw. It retains upstream VIIPER structure where practical and keeps the Handheld Companion-compatible API available for existing integrations.

For consumer-facing function lists, lifecycle examples, platform limits, and
integration guidance, see [`docs/libviiper/fork-api.md`](docs/libviiper/fork-api.md).
This file is the architectural source of truth; the API guide translates the
same contracts for applications embedding the generated C ABI.

## Current embedded architecture

- `lib/viiper` is the canonical embedded C ABI for new fork development.
- Typed libVIIPER device handles are the preferred ownership model.
- `lib/viiper` currently exposes the Classic Steam Controller (Gordon) through a typed ABI.
- `AttachUSBDevice` and `DetachUSBDevice` provide tracked localhost USB/IP
  attachment lifecycle for typed device handles on Windows.
- The tracked native ABI is deliberately pinned to usbip-win2 `v0.9.7.7`
  (`7c219953101cc5d0ec9a0bcb3eb87259cf72bedd`). This is the established
  SteamInputAddonforClaw baseline; usbip-win2 `v0.9.7.8` and later versions
  are unsupported until their ABI and runtime behavior are explicitly
  validated.
- Non-Windows builds remain ABI-compatible, but do not support tracked
  localhost attachment. They fail safely without recording attachment
  ownership.
- `clib/` remains a compatibility and historical flat API for Handheld Companion-derived integration.

New SteamInputAddonforClaw integration must not use `clib` as its architectural base. Do not remove or casually break `clib` compatibility behavior.

### Caller-owned bus lifetime

In this fork, a typed `Remove*Device` operation ends only the logical device
lifetime. It does not schedule or perform empty-bus cleanup. Buses are
caller-owned, long-lived runtime resources and remain available for subsequent
logical devices until the caller explicitly invokes `RemoveUSBBus` or closes
the owning USB server. This intentionally differs from upstream's timed
empty-bus cleanup behavior and prevents background cleanup from racing the
embedded server close lifecycle.

## Target architecture

```text
process lifetime        -> libVIIPER module/runtime
USB server/bus lifetime -> long-lived embedded runtime
logical device lifetime -> typed Steam output devices (Steam Deck target, Gordon retained) / Xbox360 objects
Windows attachment      -> explicit USB/IP attach/detach (implemented for localhost)
report-routing lifetime -> neutral/live routing
```

This is a target design. The Windows localhost attachment primitive is
implemented; explicit attachment policy, including host/device routing and
suspend/resume revalidation, remains future work.

## Fork-added devices

This fork contains device implementations not currently present upstream, including `device/steamcontroller` and `device/steamdeck`. Required fork devices should be exposed through typed `lib/viiper` wrappers rather than a new generic controller-manager API.

The Steam Deck has a minimal typed `lib/viiper` wrapper (`CreateSteamDeckDevice`,
`SetSteamDeckDeviceState`, `RemoveSteamDeckDevice`/`RemoveSteamDeckDeviceEx`),
default identity `28DE:1205`. It shares the generic `GetUSBDeviceIdentity`,
`AttachUSBDevice`, and `DetachUSBDevice` APIs and the same typed-handle
ownership/removal lifecycle as the other typed devices. It currently covers
input state only; no output/rumble/haptics callback is exposed yet, and it is
not claimed as production-default. Hardware Steam recognition/input testing
is still pending.

Current Addon production baseline:

```text
Gordon 28DE:1102
```

Target after hardware validation:

```text
persistent Steam Deck 28DE:1205
+ temporary Xbox360 for Game Bar
```

Gordon remains a supported/reference typed device.

## Server lifecycle

Canonical server wrappers use the `active`, `closing`, `close-failed`, and `closed` lifecycle states. Server-owned mutation APIs share one per-server lifecycle boundary; partial close is fail-closed.

The lifecycle boundary covers every server-owned mutation, including bus
creation/removal, typed device creation/removal, attachment, detachment, and
server close. `CloseUSBServer` establishes the closing boundary before taking
its authoritative bus snapshot, tears down buses in ascending BusID order,
and waits for callback and managed USB/IP transport work before returning.

Successful logical teardown is not rolled back when a later bus or transport
operation fails. A retry processes only the remaining authoritative buses and
does not finalize an already completed device or attachment twice.

An attached typed device stores its exact attachment backend and positive
usbip-win2 import port. Detach uses that stored token only. If attach or detach
has an unknown outcome, the owning server transitions to `close-failed`; no
automatic retry or destructive logical cleanup is attempted for that record.

Gordon consumers that need to classify typed removal failures may use the
additive `RemoveSteamControllerDeviceEx` result export. It distinguishes
successful finalization, known retryable failure, unsafe unknown ownership,
and invalid handle/lifecycle use. The legacy bool removal export remains
available and reports success only when the typed handle was finalized.

## Build and CI

Build the canonical embedded library with `just build-libVIIPER Release`.

Canonical `lib/viiper` changes require focused tests, lifecycle race coverage, vet, the Windows shared-library build, generated-header validation, and DLL export validation.

## Upstream synchronization

Keep fork-specific architectural changes localized where practical. Avoid unnecessary changes to upstream device and USB/IP core code so future synchronization remains reviewable.
