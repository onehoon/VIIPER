# libVIIPER Documentation

libVIIPER is a shared library (`libVIIPER.dll` on Windows, `libVIIPER.so` on Linux) that embeds the full VIIPER USB/USBIP stack directly into your application.

- Single shared library (`libVIIPER.dll` / `libVIIPER.so`)
- Pure C API callable from any language with C FFI support
- In-process + threadsafe  
  the USBIP server runs in a background thread inside your application
- Optional auto-attach to the local USBIP client on the same machine

!!! warning "License"
    libVIIPER is licensed under **GPL-3.0**.  
    Linking against it **requires your application to be GPL-3.0 compatible**.  
    If your project cannot comply with the GPL-3.0, use the standalone VIIPER executable and the [TCP API](../api/overview.md) instead. All TCP client libraries are **MIT licensed**.

!!! info "USBIP Required"
    libVIIPER uses USBIP internally. A USBIP client must be installed on the target machine.  
    See [Installation › Requirements](../getting-started/installation.md#usbip) for setup instructions.

!!! warning "Fork-specific contracts"
    This fork's canonical `lib/viiper` API has an explicit lifecycle and
    ownership contract that differs from upstream: typed device removal does
    not remove the caller-owned bus, tracked localhost attachment is Windows-only,
    and the tested native attachment baseline is usbip-win2 `v0.9.7.7`.
    Read the [fork API and lifecycle guide](fork-api.md) before embedding this
    library.

## API Overview

The libVIIPER C API is declared in the generated `libVIIPER.h`. Boolean
compatibility APIs coexist with classified `*Ex` APIs that return
operation-specific result enums. Exact return semantics belong in the
[fork-specific API guide](fork-api.md).
Handles (`USBServerHandle`, `Xbox360DeviceHandle`, …) are opaque `uintptr_t` values.

For the fork-specific typed API, ownership, attachment, failure-state, and
platform rules, see the [Fork-specific API guide](fork-api.md).

### Server lifecycle

| Function                               | Description                               |
| -------------------------------------- | ----------------------------------------- |
| `NewUSBServer(config, &handle, logCb)` | Start a USB server in a background thread |
| `CloseUSBServer(handle)`               | Close the server; check the result. A successful close finalizes it; a failed close may retain authoritative resources and leave it `close-failed`. Retry semantics are defined in [fork-api.md](fork-api.md). |

### Bus management

| Function                             | Description                                       |
| ------------------------------------ | ------------------------------------------------- |
| `CreateUSBBus(serverHandle, &busID)` | Create a new USB bus (pass `0` to auto-assign ID) |
| `RemoveUSBBus(serverHandle, busID)`  | Remove a bus and all its devices                  |

## Examples

Full working examples are in [`examples/libVIIPER/`](https://github.com/Alia5/VIIPER/tree/main/examples/libVIIPER).

=== "C"

    ```c
    USBServerConfig conf = { .addr = "localhost:3245" };
    USBServerHandle serverHandle = 0;
    if (!NewUSBServer(&conf, &serverHandle, logCallback)) return 1;

    uint32_t busID = 0;
    if (!CreateUSBBus(serverHandle, &busID)) {
        if (!CloseUSBServer(serverHandle)) return 1; // preserve failure evidence
        return 1;
    }

    Xbox360DeviceHandle deviceHandle = 0;
    if (!CreateXbox360Device(serverHandle, &deviceHandle, busID, /*autoAttach=*/true, 0, 0, 0)) {
        if (!CloseUSBServer(serverHandle)) return 1; // preserve failure evidence
        return 1;
    }

    SetXbox360RumbleCallback(deviceHandle, rumbleCallback);

    Xbox360DeviceState state = {0};
    while (running) {
        // only required when an actual change occurs
        state.Buttons = XBOX360_BUTTON_A;
        state.LT      = 128;
        state.LX      = 20000;
        SetXbox360DeviceState(deviceHandle, state);
        _sleep(16);
    }

    if (!CloseUSBServer(serverHandle)) return 1;
    ```

=== "C#"

    ```csharp
    USBServerConfig conf = new() { addr = "localhost:3245" };
    if (!LibVIIPER.NewUSBServer(ref conf, out nuint serverHandle, logCb)) return;

    uint busID = 0;
    if (!LibVIIPER.CreateUSBBus(serverHandle, ref busID)) {
        if (!LibVIIPER.CloseUSBServer(serverHandle)) return; // preserve failure evidence
        return;
    }

    if (!LibVIIPER.CreateXbox360Device(serverHandle, out nuint deviceHandle, busID, autoAttachLocalhost: true, 0, 0, 0)) {
        if (!LibVIIPER.CloseUSBServer(serverHandle)) return; // preserve failure evidence
        return;
    }

    Xbox360RumbleCallbackDelegate rumbleCb = RumbleCallback;
    LibVIIPER.SetXbox360RumbleCallback(deviceHandle, rumbleCb);

    Xbox360DeviceState state = new();
    while (running) {
        // only required when an actual change occurs
        state.Buttons = Xbox360Buttons.A;
        state.LT      = 128;
        state.LX      = 20000;
        LibVIIPER.SetXbox360DeviceState(deviceHandle, state);
        Thread.Sleep(16);
    }

    if (!LibVIIPER.CloseUSBServer(serverHandle)) return;
    ```

    See [`examples/libVIIPER/C#/`](https://github.com/Alia5/VIIPER/tree/main/examples/libVIIPER/C%23) for the full project including P/Invoke declarations.

## Devices

- [Xbox 360 Controller](../devices/xbox360.md)
- [DualShock 4](../devices/dualshock4.md)
- [DualSense (and Edge)](../devices/dualsense.md)
- [Switch 2 Pro Controller](../devices/ns2pro.md)
- [Keyboard](../devices/keyboard.md)
- [Mouse](../devices/mouse.md)
- Fork-specific typed Steam Controller and Steam Deck APIs are also exposed;
  see [fork-api.md](fork-api.md) for their contract.

### Logging

Pass an optional `VIIPERLogCallback` to `NewUSBServer` to observe synchronous
log messages. `NULL` disables that callback observer only; on Windows the
library-owned `libVIIPER.log` sink remains independently active when available.
Logging failures do not affect lifecycle results.

```c
typedef enum {
    VIIPER_LOG_DEBUG = -4,
    VIIPER_LOG_INFO  = 0,
    VIIPER_LOG_WARN  = 4,
    VIIPER_LOG_ERROR = 8,
} VIIPERLogLevel;

typedef void (*VIIPERLogCallback)(VIIPERLogLevel level, const char* message);
```


