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
