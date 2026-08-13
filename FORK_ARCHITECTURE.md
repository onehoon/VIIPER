# VIIPER Fork Architecture

## Purpose

This fork carries additional embedded-library and device work for SteamInputAddonforClaw. It retains upstream VIIPER structure where practical and keeps the Handheld Companion-compatible API available for existing integrations.

## Current embedded architecture

- `lib/viiper` is the canonical embedded C ABI for new fork development.
- Typed libVIIPER device handles are the preferred ownership model.
- `lib/viiper` currently exposes the Classic Steam Controller (Gordon) through a typed ABI.
- Current local Windows attachment uses existing `autoAttachLocalhost` behavior.
- Explicit attach/detach APIs are not implemented yet.
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
logical device lifetime -> typed Gordon/Xbox360 objects
Windows attachment      -> explicit USB/IP attach/detach
report-routing lifetime -> neutral/live routing
```

This is a target design, not a statement that explicit attach/detach is currently available.

## Fork-added devices

This fork contains device implementations not currently present upstream, including `device/steamcontroller` and `device/steamdeck`. Required fork devices should be exposed through typed `lib/viiper` wrappers rather than a new generic controller-manager API.

The Steam Deck does not yet have a typed `lib/viiper` wrapper.

The intended virtual outputs are a persistent Classic Steam Controller (Gordon, `28DE:1102`) and a temporary Xbox 360 controller for the Game Bar route.

## Server lifecycle

Canonical server wrappers use the `active`, `closing`, `close-failed`, and `closed` lifecycle states. Server-owned mutation APIs share one per-server lifecycle boundary; partial close is fail-closed.

## Build and CI

Build the canonical embedded library with `just build-libVIIPER Release`.

Canonical `lib/viiper` changes require focused tests, lifecycle race coverage, vet, the Windows shared-library build, generated-header validation, and DLL export validation.

## Upstream synchronization

Keep fork-specific architectural changes localized where practical. Avoid unnecessary changes to upstream device and USB/IP core code so future synchronization remains reviewable.
