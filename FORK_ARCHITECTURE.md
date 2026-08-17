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

Typed lifecycle mutations owned by one server are serialized at that server's
`lifecycleMu` boundary. A later operation observes the authoritative
attachment state and exact token committed by the earlier operation; it does
not act on a pre-lock snapshot. `close-failed` is server-scoped: it blocks
ordinary mutation for every typed device owned by that server while retaining
diagnostic ownership evidence.

`CloseUSBServer` establishes the closing boundary before taking its
authoritative bus snapshot, tears down buses in ascending BusID order, and
waits for callback and managed USB/IP transport work before returning.

Successful logical teardown is not rolled back when a later bus or transport
operation fails. A retry processes only remaining authoritative buses.

Attached typed devices store their exact attachment backend and positive
usbip-win2 import port. Detach uses that stored token only. Unknown attach or
detach outcomes transition the owning server to `close-failed`; no destructive
automatic retry is attempted.

Separate `USBServerHandle` instances have independent lifecycle state. Within
one process, `virtualbus` BusID allocation is process-global, so same-process
isolation tests and callers must use distinct BusIDs; this does not establish
cross-process BusID coordination.

## Diagnostic logging

`libVIIPER` owns its own diagnostic log rather than depending on an embedding
application to persist it. On Windows, `NewUSBServer` writes `libVIIPER.log`
beside the directory containing the actually loaded `libVIIPER.dll` module
(resolved from the loaded module itself, never the process executable path,
current working directory, or an application-specific data directory).
Non-Windows builds have no file sink in this fork; a supplied
`VIIPERLogCallback` still works normally. If module-path resolution or the
file open fails, that is diagnostic-only: `NewUSBServer` and controller
routing are never affected, and no fallback stdout/stderr CLI-style output is
introduced into the embedded DLL.

When the Windows owned file sink is available, it uses exactly one
`libVIIPER.log`, containing current-local-calendar-day diagnostics only.
Records append during the same day. On the first write after the local date
changes — whether that is a fresh `NewUSBServer` on a new day or the process
simply remaining alive across midnight — the same file is reset in place and
reused for the new day. On a successful reset, the triggering record is
preserved as the first record of the new day. If the reset fails, file
persistence is suppressed for the rest of that day rather than appending to
now-stale content; a `VIIPERLogCallback`, if supplied, is entirely
unaffected either way. No dated archive (`libVIIPER-2026-08-16.log`),
numbered rotation (`libVIIPER.log.1`), size limit, compression, or
background cleanup is maintained. This uses the
machine's local date, not UTC, and there is no timezone configuration.

The optional `VIIPERLogCallback` supplied to `NewUSBServer` is an observer,
not the persistence mechanism: passing `NULL` never disables the file, and
supplying a callback never causes a record to be written into the file
twice. Both destinations receive the same structured record through the
existing `internal/log.MultiHandler` fan-out. The callback remains
synchronous and unchanged; callers should still keep callback code short.

File persistence is asynchronous. The owned file's `io.Writer` is a bounded,
non-blocking FIFO queue drained by one process-wide writer goroutine (shared
by every `NewUSBServer` in the process, along with the daily-rollover state
above — no per-server rollover state, no competing resets); a producer (e.g.
`AttachUSBDeviceEx`/`DetachUSBDeviceEx` recording their timing diagnostics)
never waits on the filesystem, and the daily reset itself only ever happens
inside that same writer goroutine, never on a caller's thread. If the queue
saturates, the diagnostic record is dropped and counted rather than blocking
a controller/routing operation; a compact backlog notice is written once
capacity is available again. `CloseUSBServer` requests a best-effort,
time-bounded flush of that queue, after releasing the server's lifecycle
lock, on a successful close; a stuck or slow filesystem never makes close
itself appear to hang or fail, and never holds that lock while waiting. None
of this — module-path resolution failure, file-open failure, write failure,
queue saturation, flush timeout, or a failed daily reset — ever changes a
classified attach/detach/removal result, stored attachment state, or any
other lifecycle outcome; logging remains diagnostic only.

A zero/stale/unresolvable `USBServerHandle` has no server to own a
`VIIPERLogCallback` for. Diagnostics for that case (e.g. an invalid-handle
`AttachUSBDeviceEx` call) go to a library-owned, file-only fallback logger —
never to any particular server's callback, and never through Go's
process-global `slog.Default()`. `libVIIPER` never calls `slog.SetDefault`
at all: every logger it constructs and uses is its own explicit logger, so
neither the embedding process's global default logger nor any other
`USBServerHandle` in the same process is ever affected by one server's
construction.

Routine lifecycle, attach/detach, classified-failure, and teardown diagnostics
are low-volume and always active. Canonical teardown records cover typed
removal, bus removal, and server close with the logical identity, tracked
attachment evidence where relevant, teardown phase, classified result, and
before/after lifecycle state. Canonical attachment and teardown records are
snapshotted under the lifecycle lock and emitted after that lock is released;
they remain behavior-neutral. Per-input/per-frame
paths (state setters, input reports, publisher loops) do not log; raw packet
logging remains a separate, off-by-default mechanism (`internal/log.RawLogger`).

Rejected mutation warnings use the same boundary: de-duplication state is
updated under `lifecycleMu`, but the warning is emitted only after unlock.

Canonical tracked attachment and detachment backend diagnostics follow the
same boundary. Records produced during the serialized native operation are
captured while `lifecycleMu` is held and synchronously replayed only after the
owning lock is released. `VIIPERLogCallback` therefore remains synchronous
before the public lifecycle API returns, without weakening lifecycle
serialization; the staging and replay are behavior-neutral. This statement
does not extend to the legacy `clib`/TCP/server logging stack unless separately
verified.

## Build and CI

Build the canonical embedded library with:

```text
just build-libVIIPER Release
```

Canonical changes require focused tests, lifecycle race coverage, vet, the
Windows shared-library build, generated-header validation, and export checks.

### Canonical Windows dependency artifact

A successful main-branch build of the `libviiper-windows` job produces a
self-describing canonical artifact, `libVIIPER-windows-amd64.zip`, containing
`libVIIPER.dll`, `libVIIPER.h`, `libVIIPER.def`, `licenses.txt`, and
`viiper-artifact.json`. The DLL, header, and manifest are always produced by
the same job invocation of `just build-libVIIPER Release`; no separate job
rebuilds any of these files.

`viiper-artifact.json` records the artifact's source identity as the full
40-character Git commit SHA of the build (`git rev-parse HEAD`), never a
branch name, short SHA, or mutable tag, plus SHA-256 hashes of the exact DLL
and header produced by that build. The same job recomputes both hashes and
fails the build if they do not match the manifest (see
`scripts/generate-libviiper-manifest.ps1` and
`scripts/verify-libviiper-manifest.ps1`).

Pull request builds run the same manifest generation and verification for
validation only; PR CI does not upload artifacts and is never an adoption
candidate. Only a successful main-branch build is eligible for downstream
adoption. Any downstream consumer, including SteamInputAddonforClaw, must pin
an exact commit/artifact and must not depend on a mutable "latest" artifact.

### Cross-repository dependency notification

`.github/workflows/notify-addon-dependency.yml` runs on `workflow_run` after
`Dev Snapshot Build` completes, and proceeds only when that run's conclusion
was `success`, its `head_branch` was `main`, and its trigger `event` was
`push`. It validates `head_sha` is exactly 40 hexadecimal characters, then
sends a `repository_dispatch` (`viiper-canonical-ready`) to
`onehoon/SteamInputAddonforClaw` carrying that exact `commit` and a
diagnostic-only `source_run_id`. The GitHub App installation token it
generates is scoped only to `SteamInputAddonforClaw` with `contents: write`,
the minimum needed to send a `repository_dispatch`; it never reads Actions
runs or writes pull requests.

This dispatch payload is only a hint/input, never adoption authority. The
generated canonical artifact for the exact dispatched commit remains the
actual dependency source. The Addon's `viiper-dependency-update.yml` receiver
independently rediscovers and re-verifies the exact push/main/success run,
its canonical artifact name, its manifest, and its DLL/header hashes before
adopting anything — it never trusts the dispatch payload's claims. Managed
ABI compatibility is never auto-inferred: the receiver mechanically vendors
the artifact into a Draft PR and stops. That Draft PR is never auto-merged;
manual human review of the ABI/runtime diff and manual merge are required.

If App credentials are unavailable, token creation fails, SHA validation
fails, or the dispatch call fails, the sender workflow fails visibly. It does
not fall back to `GITHUB_TOKEN`, retry with a different commit, or select a
different run.

## Upstream synchronization

Keep fork-specific changes localized. Avoid unnecessary changes to device and
USB/IP core code so future synchronization remains reviewable.
