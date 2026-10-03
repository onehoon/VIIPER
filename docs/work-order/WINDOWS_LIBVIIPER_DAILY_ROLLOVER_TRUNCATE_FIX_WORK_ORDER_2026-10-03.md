# Work Order — Fix Windows libVIIPER Daily-Rollover Truncation

Date: 2026-10-03

Repository: `onehoon/VIIPER`  
Target base: latest `main`

**Scope: one Windows embedded-log correctness fix plus focused real-filesystem regression coverage.**

## 1. Goal and observed production failure

Fix a real Windows logging failure in the canonical embedded `libVIIPER` file sink.

A downstream Windows consumer configured `libVIIPER.log` into its persistent application log directory. The file contains valid VIIPER records through 2026-09-27, but after subsequent successful VIIPER server/controller sessions the file still has the old 2026-09-27 contents and modification time.

The consumer-side evidence establishes all of the following at the same time:

- the process successfully reaches `SetDiagnosticLogDirectory`;
- `NewUSBServer` succeeds;
- the virtual controller attaches and operates normally;
- the expected `libVIIPER.log` path is the path being exported by the consumer;
- that `libVIIPER.log` is not reset or updated on later local-calendar days.

This is therefore not a controller-routing failure and not a downstream export-path problem. It is a file-persistence failure at VIIPER's daily rollover boundary.

The current architecture intentionally treats logging failure as diagnostic-only, so controller operation continuing normally is expected. The bug is that the owned file sink can permanently suppress the current day's persistence after rollover fails.

## 2. Read before editing

Preserve the existing fork contracts in:

- `FORK_ARCHITECTURE.md`;
- `docs/libviiper/fork-api.md`;
- `lib/viiper/embeddedlog.go`;
- `lib/viiper/dailyrollover.go`;
- `lib/viiper/asynclog.go`;
- `lib/viiper/embeddedlog_windows.go`;
- the existing embedded-log and daily-rollover tests.

Relevant downstream contract: the standalone Full 1902 Addon consumes the canonical `lib/viiper` artifact and must continue to receive the same ABI, lifecycle results, callback behavior, and attachment ownership semantics. No downstream consumer change belongs in this PR.

## 3. Root cause

The Windows owned-file sink currently opens the real destination as:

```go
f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
```

and wraps it as:

```go
type osFileDailyLogWriter struct{ f *os.File }

func (w *osFileDailyLogWriter) Write(p []byte) (int, error) { return w.f.Write(p) }
func (w *osFileDailyLogWriter) Reset() error                { return w.f.Truncate(0) }
```

The daily rollover path calls `Reset()` on the first write after the local date changes. If reset fails, `dailyRolloverWriter` deliberately suppresses persistence for the remainder of that day:

```go
case today != d.day:
    d.day = today
    d.suppressed = d.backing.Reset() != nil
```

That policy is correct; the concrete Windows reset mechanism is not.

This repository currently declares `go 1.26.2` in `go.mod`. Ground the Windows behavior in that exact toolchain version, not Go `master`.

In Go 1.26.2 on Windows, `syscall.Open` handling for `O_APPEND` removes `GENERIC_WRITE` when `O_TRUNC` is not present and replaces it with append-oriented rights including `FILE_APPEND_DATA`. See:

- https://raw.githubusercontent.com/golang/go/go1.26.2/src/syscall/syscall_windows.go

Go 1.26.2 `os.Truncate(path, size)` opens the named file separately with `O_WRONLY` before truncating it. See:

- https://raw.githubusercontent.com/golang/go/go1.26.2/src/os/file_windows.go

Windows `SetEndOfFile`, which underlies truncation of an already-open file handle, requires the handle to have been created with `GENERIC_WRITE`. See:

- https://learn.microsoft.com/windows/win32/api/fileapi/nf-fileapi-setendoffile

Therefore the existing long-lived append handle is suitable for append writes but is not a reliable Windows handle for the later `f.Truncate(0)` rollover operation.

Once that rollover reset fails, the existing fail-safe behavior explains the observed symptom exactly:

```text
old-day libVIIPER.log exists
        |
new process/server starts on a later local day
        |
first VIIPER file record reaches dailyRolloverWriter
        |
Reset() -> f.Truncate(0) fails on the append-only Windows handle
        |
suppressed = true
        |
all file persistence for that local day is discarded
        |
routing/controller operation remains normal
```

The optional `VIIPERLogCallback` is independent and remains unaffected by this file-sink failure. Do not change that contract.

## 4. Required production change

Keep the current architecture:

- one fixed `libVIIPER.log`;
- one process-wide file handler/writer;
- append writes during the same day;
- reset in place on the first write of a new local day;
- no dated archive;
- no size rotation;
- asynchronous/non-blocking producer path;
- reset failure remains diagnostic-only and suppresses stale-file appends for that day.

Change only how the real file writer identifies and truncates its destination on Windows-compatible Go semantics.

### Path-stability requirement

`SetDiagnosticLogDirectory` currently accepts a non-empty relative directory as well as an absolute directory. That API behavior must not be changed in this PR.

Do **not** implement rollover as `os.Truncate(w.f.Name(), 0)` while retaining a possibly relative `Name()`. If the process working directory changes after the file was opened, the same relative string could resolve to a different file at rollover time.

Instead, resolve the selected `libVIIPER.log` path to one absolute path **once when the real file sink is initialized**, use that same absolute path for stat/open, and retain it in the concrete writer for later reset.

Preferred shape:

```go
type osFileDailyLogWriter struct {
    f    *os.File
    path string
}

func (w *osFileDailyLogWriter) Write(p []byte) (int, error) {
    return w.f.Write(p)
}

func (w *osFileDailyLogWriter) Reset() error {
    return os.Truncate(w.path, 0)
}
```

The real sink setup should conceptually be:

```go
resolvedPath, ok := resolveEmbeddedLogPath(directory)
if !ok {
    // existing no-file-sink behavior
}

absolutePath, err := filepath.Abs(resolvedPath)
if err != nil {
    // diagnostic-only sink initialization failure
}

// Use absolutePath consistently for:
//   - initial stat/mod-time lookup
//   - os.OpenFile(... O_APPEND ...)
//   - osFileDailyLogWriter.path
```

Keep this path stabilization local to the real owned-file sink. Do not change the public `SetDiagnosticLogDirectory` contract to reject relative paths, and do not add a new path manager or process-working-directory watcher.

Why this shape is preferred:

- `os.Truncate(path, 0)` opens a separate writable handle for the truncate operation instead of trying to truncate through the long-lived append-only handle;
- Go 1.26.2's Windows `os.Truncate` implementation opens the named file with `O_WRONLY`, then truncates through that writable handle;
- freezing one absolute destination at sink initialization preserves the file that was actually selected even if the process working directory later changes;
- the existing append file handle remains the single long-lived writer;
- after the named file is truncated to zero, the next `O_APPEND` write lands at the new EOF;
- no close/reopen state machine is required.

Equivalent minimal code is acceptable if the Windows regression tests prove both the handle-right invariant and the path-stability invariant. Do not redesign the logger around this fix.

Update the nearby comment in `embeddedlog.go`: it currently states that the same append-opened `*os.File` can successfully `Truncate(0)` in place. That statement is not valid for the Windows append-handle semantics above.

## 5. Required Windows regression test

The existing fake `dailyLogWriter` tests correctly test policy but cannot detect this platform handle-rights bug.

Add one focused **real filesystem Windows test** using the same open flags as production.

Preferred location:

```text
lib/viiper/embeddedlog_windows_test.go
```

or another narrowly named `*_windows_test.go` in `lib/viiper`.

The test must exercise a real file, not a fake writer.

Minimum required behavior:

1. Create a temporary `libVIIPER.log` containing old-day/stale bytes.
2. Open it exactly like production:

   ```go
   os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
   ```

3. Construct the real `osFileDailyLogWriter` over that handle.
4. Call `Reset()`.
5. Assert reset succeeds on Windows.
6. Write a first new-day record through the original long-lived append handle.
7. Write a second same-day record through that same original append handle.
8. Close/flush as required by the test.
9. Read the file and assert it contains exactly those two new records in order, with no stale prefix.

The regression should fail with the current `w.f.Truncate(0)` implementation on Windows and pass with the fix.

A suitable test shape is:

```go
func TestOSFileDailyLogWriterResetWorksWithProductionAppendHandle(t *testing.T) {
    path := filepath.Join(t.TempDir(), "libVIIPER.log")
    if err := os.WriteFile(path, []byte("stale-old-day\n"), 0o644); err != nil {
        t.Fatal(err)
    }

    f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
    if err != nil {
        t.Fatal(err)
    }
    defer f.Close()

    w := &osFileDailyLogWriter{f: f}

    if err := w.Reset(); err != nil {
        t.Fatalf("Reset failed for production append handle: %v", err)
    }
    if _, err := w.Write([]byte("new-day-1\n")); err != nil {
        t.Fatal(err)
    }
    if _, err := w.Write([]byte("new-day-2\n")); err != nil {
        t.Fatal(err)
    }

    got, err := os.ReadFile(path)
    if err != nil {
        t.Fatal(err)
    }
    const want = "new-day-1\nnew-day-2\n"
    if string(got) != want {
        t.Fatalf("log contents = %q, want %q", got, want)
    }
}
```

Adjust only for test correctness/style. The concrete writer may now require the retained absolute path as well as the file handle; construct it exactly as production does. Do not add production-only hooks just to make the test convenient.

Also add one focused path-stability assertion for the existing relative-directory contract. The test may change the process working directory **inside the test only**, provided it restores it with `t.Cleanup` and does not run in parallel. The invariant to prove is:

```text
relative configured directory at sink initialization
    -> sink resolves/fixes one absolute destination
    -> process working directory changes
    -> Reset()
    -> original opened libVIIPER.log is truncated
    -> no second file is created/resolved under the new working directory
```

Keep this as a small Windows-only regression. No production observer, lock, retry, or working-directory tracking is needed.

## 6. Daily-rollover integration coverage

In addition to the direct writer test, preserve the existing fake-policy tests in `dailyrollover_test.go`.

If it can be done without a new framework, add or adapt one small Windows real-filesystem test that proves the actual daily policy:

```text
stale file from previous local day
    -> first current-day record
    -> reset succeeds
    -> stale bytes disappear
    -> triggering current-day record is preserved
    -> second current-day record appends normally
```

This is desirable but secondary to the required production-append-handle regression above.

Do not manipulate the real system clock. Use existing injectable `now` seams and/or explicit initial-day construction.

## 7. Preserve failure policy

Do not weaken this existing safety property:

```go
if d.suppressed {
    return len(p), nil
}
```

If reset genuinely fails because of filesystem/AV/filter-driver/permission failure, VIIPER must still avoid appending new-day records onto stale old-day content.

This PR fixes the Windows handle-right mismatch that causes a normal rollover to fail. It does not convert diagnostic logging into a controller-routing dependency.

Do not make `NewUSBServer`, attach/detach, typed removal, bus removal, or server close fail because log persistence fails.

## 8. Explicit exclusions

Do **not** include:

- CTW/ClawTweaks code changes;
- SteamAddonforClaw code changes;
- downstream artifact pin updates;
- changes to `SetDiagnosticLogDirectory` ABI or semantics;
- changes to `VIIPERLogCallback` ABI or callback filtering;
- new public exports for logger state;
- a logger health manager;
- file watchers;
- retry loops;
- reopening the log on every record;
- dated/numbered rotation;
- size-based rotation;
- extra background cleanup;
- changes to the async queue capacity or flush timeout;
- controller, USB/IP, attachment, rumble, haptics, report, HidHide, or device-lifecycle code;
- unrelated cleanup of the previous Xbox360 diagnostic trace code.

Do not introduce new locks, states, managers, wrappers, epochs, or abstractions for theoretical logging races. The writer goroutine already serializes the rollover/write path that matters here.

## 9. Documentation update

Update the diagnostic logging sections of:

- `FORK_ARCHITECTURE.md`;
- `docs/libviiper/fork-api.md`;

only as needed to keep the implementation statement accurate.

When touching those sections, state the destination rule accurately:

```text
SetDiagnosticLogDirectory configured before sink initialization
    -> libVIIPER.log in that configured directory

no configured directory
    -> Windows fallback beside the actually loaded libVIIPER.dll
```

Do not leave wording that implies the DLL-adjacent path is unconditional. A relative configured directory remains accepted, but the sink should freeze the resulting absolute file destination when it initializes.

The retention contract remains:

```text
same-day -> append
new local day -> reset same libVIIPER.log, then write
real reset failure -> suppress persistence for that day
callback observer -> unaffected
routing/lifecycle -> unaffected
```

Do not expand the documentation beyond the actual fix.

## 10. Validation

Run the normal repository checks:

```sh
go test ./lib/viiper/...
go test -race ./lib/viiper/...
go test ./...
go vet ./...
git diff --check
```

The Windows real-filesystem regression is mandatory evidence. If local development is not Windows, ensure the repository's Windows CI runs the test and report that result explicitly in the PR.

Also run the canonical Windows shared-library path:

```text
just build-libVIIPER Release
```

Verify:

- generated header/export checks still pass;
- no public ABI/export changes;
- no controller lifecycle behavior changes;
- the canonical Windows artifact is produced normally.

Do not commit downstream DLL/header pin changes in this PR.

## 11. Downstream adoption after merge

After this VIIPER PR is merged and the exact `main` commit's canonical Windows artifact passes CI:

1. adopt that exact artifact in ClawTweaks/CTW through its existing pinned-artifact process;
2. adopt the same exact artifact in SteamAddonforClaw through its existing dependency-update/review process;
3. verify a fresh current-day `libVIIPER.log` is actually written in each consumer;
4. for CTW, verify the existing Export Logs operation now exports the current-session/current-day VIIPER file rather than the previously stale file.

These are separate downstream reviewed changes. Do not modify either consumer from this VIIPER PR.

## 12. Acceptance criteria

The PR is complete when:

- the production Windows log file is still opened once for append writes;
- daily reset no longer calls `Truncate(0)` through the append-only long-lived handle;
- a real Windows test proves reset succeeds with the exact production append-open flags;
- the sink retains one absolute destination selected at initialization, so later working-directory changes cannot redirect rollover;
- stale old-day bytes are removed;
- the first new-day record is preserved;
- a second same-day record appended through the original long-lived handle is preserved in order;
- genuine reset failure still suppresses stale-file persistence rather than affecting routing;
- `VIIPERLogCallback` behavior is unchanged;
- async producer/non-blocking logging behavior is unchanged;
- no ABI, controller, USB/IP, or lifecycle change is introduced.

**Expected production diff:** a small, localized change in `lib/viiper/embeddedlog.go` to retain the initialized absolute file path and truncate by that path, plus the corresponding comment correction. If the absolute-path normalization is cleaner in the existing Windows/path helper seam, a similarly narrow edit there is acceptable.

The objective is not to redesign VIIPER logging. It is to make the existing one-file daily-retention contract actually work on Windows.
