# Work Order — USB/IP OUT RET_SUBMIT Payload Protocol Fix

Date: 2026-09-28

Target repository:

```text
onehoon/VIIPER
```

Target branch:

```text
main
```

Upstream reference:

```text
Alia5/VIIPER
commit 6a5602af3241d6054db5a544eac062b230d4ca3c
"USBIP-Server: Fix: do not append a payload to RET_SUBMIT for OUT transfers"
```

This is a deliberately narrow protocol-correctness PR.

Do **not** merge or rebase upstream VIIPER v0.8.0 as part of this work.
Do **not** import unrelated v0.7.1/v0.8.0 changes.

---

## 1. Goal

Correct the USB/IP server response for host-to-device `CMD_SUBMIT` OUT transfers.

For an OUT transfer:

- the request payload is consumed by the emulated device;
- `RET_SUBMIT.ActualLength` must report the number of OUT bytes consumed;
- the `RET_SUBMIT` response must **not** append a device-to-host payload body.

Current code correctly reports `ActualLength = len(outPayload)`, but it still passes the device handler's `respData` into `writeRet`.

The transport layer must explicitly discard any returned `respData` for OUT transfers before writing `RET_SUBMIT`.

---

## 2. Mandatory architecture constraints

Before implementation, read and preserve:

```text
FORK_ARCHITECTURE.md
docs/libviiper/fork-api.md
```

The current fork architecture remains authoritative.

This PR must not change:

- canonical `lib/viiper` public ABI;
- typed device handles;
- classified attach/detach behavior;
- attachment ownership tracking;
- `close-failed` behavior;
- usbip-win2 0.9.8.0 / 0.9.8.1 ABI selection;
- detached-ready device semantics;
- Steam Deck or Xbox360 report formats;
- output/rumble callback semantics;
- Addon-specific behavior;
- logging architecture;
- batching policy;
- asynchronous IN handling.

This fix belongs only to the generic USB/IP transport response boundary.

---

## 3. Current code path

Relevant production file:

```text
internal/server/usb/server.go
```

Current OUT/EP0 response path is conceptually:

```go
respData := s.processSubmit(ctx, dev, ep, dir, setup, outPayload)
actualLen := uint32(len(respData))
if dir == usbip.DirOut {
    actualLen = uint32(len(outPayload))
}
if err := writeRet(seq, actualLen, respData, ep == 0); err != nil {
    return err
}
```

This preserves the correct OUT `ActualLength`, but if a device implementation returns non-empty bytes from `HandleTransfer(..., DirOut, ...)`, those bytes can currently be appended after the `RET_SUBMIT` header.

That is not a valid OUT completion payload.

---

## 4. Required production change

Make the minimum transport-level correction:

```go
respData := s.processSubmit(ctx, dev, ep, dir, setup, outPayload)
actualLen := uint32(len(respData))
if dir == usbip.DirOut {
    actualLen = uint32(len(outPayload))
    respData = nil
}
if err := writeRet(seq, actualLen, respData, ep == 0); err != nil {
    return err
}
```

Equivalent code is acceptable if it preserves the same simple invariant.

### Required invariant

For every USB/IP OUT `CMD_SUBMIT` completion:

```text
RET_SUBMIT.ActualLength = number of OUT request bytes consumed
RET_SUBMIT payload body = empty
```

This applies independently of what the emulated device handler accidentally or intentionally returns.

---

## 5. Why the transport layer owns this rule

Do not fix this by changing individual devices to promise `nil` on OUT.

Current production devices already normally return `nil` for OUT, including the active Steam Deck and Xbox360 paths, but protocol correctness must not depend on every device implementation preserving that convention forever.

The server knows the USB/IP transfer direction and is therefore the correct single authority for deciding whether a `RET_SUBMIT` body is legal.

Do not add:

- a new device interface;
- a new result wrapper;
- a new OUT-specific device contract;
- validation duplicated into each controller implementation;
- additional locks, state, epochs, queues, or retry machinery.

---

## 6. Regression test

Extend the existing USB/IP transport tests rather than creating a second test framework.

Preferred test location:

```text
internal/server/usb/usbip_out_trace_test.go
```

The existing harness already exercises real `CMD_SUBMIT -> handleUrbStream -> RET_SUBMIT` transport behavior and checks OUT `ActualLength`.

Add one focused regression that deliberately uses a fake/probe device whose OUT `HandleTransfer` returns a non-empty byte slice.

Example fake behavior:

```go
func (d *outResponseProbe) HandleTransfer(
    _ context.Context,
    _ uint32,
    dir uint32,
    payload []byte,
) []byte {
    if dir == usbip.DirOut {
        d.received = append(d.received, append([]byte(nil), payload...))
        return []byte{0xAA, 0xBB, 0xCC}
    }

    return nil
}
```

The exact helper name is not important.

The test must send a normal OUT `CMD_SUBMIT` and verify all of the following:

1. The fake device receives the complete original OUT payload.
2. The returned `RET_SUBMIT` has the matching sequence number.
3. `RET_SUBMIT.ActualLength == len(outPayload)`.
4. No bytes from the fake device's returned `[]byte{0xAA, 0xBB, 0xCC}` are appended to the response.
5. The client can parse the next USB/IP response boundary correctly, proving no unexpected bytes were left in the stream.

Prefer a real stream-boundary assertion over merely testing a local helper function.

A good shape is two sequential OUT submissions:

```text
OUT seq=N
  device returns fake response bytes
  -> read exactly one RET_SUBMIT header

OUT seq=N+1
  -> next parsed response must begin at the next RET_SUBMIT header
```

This proves the first OUT completion did not contaminate the TCP stream with an illegal response body.

---

## 7. Preserve existing Xbox360 diagnostic semantics

Current `usbip_out_trace_test.go` also validates the optional Xbox360 OUT diagnostics:

```text
X360USBIPOutIngress
ProbeHandleTransfer
X360USBIPOutWriterAccepted
```

Do not change their meaning.

In particular:

- `TraceUSBIPOutIngress` still records the received host OUT payload;
- `TraceUSBIPOutWriterAccepted.ActualLength` remains the consumed OUT request length;
- writer-accepted must still only be emitted after `writeRet` accepts the response bytes;
- no extra trace event is needed for this protocol fix.

This PR is not another rumble diagnostic PR.

---

## 8. Explicit non-goals

Do not include any of the following:

- upstream v0.8.0 merge/rebase;
- upstream DS4 `UpdateFlags` ABI change;
- DS4 callback changes;
- dependency upgrades;
- usbip-win2 installer changes;
- usbip-win2 version-probing changes;
- attach/detach changes;
- Steam Deck changes;
- Xbox360 rumble decoding changes;
- rumble tracing expansion;
- report timing changes;
- write batching changes;
- IN transfer scheduling changes;
- new public exports;
- Addon repository changes;
- documentation rewrites unrelated to this exact fix.

If implementation work uncovers a separate defect, do not expand this PR. Record it separately.

---

## 9. Validation

Run at minimum:

```text
go test ./internal/server/usb/...
go test ./...
go vet ./...
```

Also run the repository's normal canonical build/validation path required for a VIIPER main change, including the Windows shared-library/header/export checks already defined by the repository CI.

Because this PR must not change the public canonical ABI, verify that no intentional public export, generated C struct, enum, callback signature, or calling convention changed.

Do not create new ABI machinery solely for this verification; use the existing repository checks.

---

## 10. Review criteria

The PR is correct when:

- the generic USB/IP server discards `respData` for OUT completions;
- OUT `ActualLength` remains the original consumed request length;
- regression coverage proves no OUT response body reaches the wire even if a device returns non-empty bytes;
- existing OUT tracing behavior remains unchanged;
- existing Steam Deck/Xbox360 behavior remains unchanged;
- no public ABI or lifecycle contract changes;
- no unrelated upstream 0.8 changes are imported.

---

## 11. Scope summary

Expected production diff:

```text
internal/server/usb/server.go
    + one explicit OUT respData discard
```

Expected test diff:

```text
internal/server/usb/usbip_out_trace_test.go
    + one focused transport regression
    + minimal local fake/helper if needed
```

No additional architecture is required.

The objective is protocol correctness at one existing transport boundary, not a broader VIIPER update.
