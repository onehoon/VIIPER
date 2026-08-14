# Fork architecture and API changes

This entry summarizes the fork-specific changes now present on `main`. It is
not an upstream VIIPER release note; it describes the contracts that an
application must follow when consuming this fork.

## Embedded API foundation

- Added the canonical typed `lib/viiper` server, bus, device, callback, and
  attachment lifecycle.
- Added typed wrappers for Gordon (Classic Steam Controller), Xbox 360,
  DualSense/Edge, DualShock 4, keyboard, mouse, and Nintendo Switch 2 Pro.
- Added safe opaque-handle validation, identity lookup, null output guards, and
  exactly-once handle finalization.
- Kept `clib/` as a compatibility API rather than using it as the new
  architecture.

## Ownership and lifecycle

- Buses are caller-owned in canonical `lib/viiper` and survive typed device
  removal.
- Server-owned mutations share one per-server lifecycle boundary.
- Close supports partial failure and retry without revisiting finalized buses,
  devices, or handles.
- Unknown attachment or detachment outcomes fail closed and prevent automatic
  destructive retry.
- Callback clearing, logical removal, transport drain, and handle finalization
  have a deterministic order.

## Windows USB/IP attachment

- Added tracked localhost attach/detach with exact backend and positive import
  port ownership.
- The supported native baseline is usbip-win2 `v0.9.7.7`, commit
  `7c219953101cc5d0ec9a0bcb3eb87259cf72bedd`.
- usbip-win2 `v0.9.7.8` and later versions are unsupported until explicitly
  validated for ABI and runtime compatibility.
- Non-Windows builds remain compile-compatible but do not claim tracked
  localhost attachment support.

## Transport and callback safety

- Canonical embedded servers disable legacy automatic empty-bus cleanup.
- Managed USB/IP connections and async non-EP0 IN workers are drained before
  public remove/close operations return.
- Callback registration and clear operations are synchronized across all typed
  devices, while callback invocation occurs outside the callback lock.
- Gordon runtime state has dedicated synchronization for polling, host feature
  commands, and reset paths.

## Integration guidance

Use the generated `dist/libVIIPER/libVIIPER.h` and matching DLL from the same
commit. Start with [Fork-specific libVIIPER API](../libviiper/fork-api.md) and
[Fork Architecture](../../FORK_ARCHITECTURE.md). Do not mix this fork's
canonical header/DLL with upstream artifacts or assume that Linux attachment
behavior is equivalent to the tested Windows backend.
