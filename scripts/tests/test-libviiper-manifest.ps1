<#
Focused self-checking test for generate-libviiper-manifest.ps1 and
verify-libviiper-manifest.ps1. Does not depend on Pester so it runs
identically in any pwsh environment (local or CI).

Exercises:
  - happy path: generate then verify succeeds
  - verify fails on wrong commit
  - verify fails on malformed/missing full SHA
  - verify fails on wrong DLL hash
  - verify fails on wrong header hash
  - verify fails on missing DLL
  - verify fails on missing header
  - verify fails on unsupported schema_version
#>
$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
$generate = Join-Path $root "generate-libviiper-manifest.ps1"
$verify = Join-Path $root "verify-libviiper-manifest.ps1"

$work = Join-Path ([System.IO.Path]::GetTempPath()) ("viiper-manifest-test-" + [guid]::NewGuid())
New-Item -ItemType Directory -Force -Path $work | Out-Null

$failures = 0
function Check([string]$Name, [bool]$Condition) {
    if ($Condition) {
        Write-Host "[PASS] $Name"
    }
    else {
        Write-Host "[FAIL] $Name"
        $script:failures++
    }
}

function Run-Verify([string]$ManifestPath, [string]$DllPath, [string]$HeaderPath, [string]$ExpectedCommit) {
    $args = @(
        "-NoProfile", "-NonInteractive", "-File", $verify,
        "-ManifestPath", $ManifestPath,
        "-DllPath", $DllPath,
        "-HeaderPath", $HeaderPath
    )
    if ($ExpectedCommit) {
        $args += @("-ExpectedCommit", $ExpectedCommit)
    }
    & pwsh @args *> $null
    return $LASTEXITCODE
}

try {
    $dll = Join-Path $work "libVIIPER.dll"
    $header = Join-Path $work "libVIIPER.h"
    Set-Content -LiteralPath $dll -Value "fake-dll-content" -NoNewline
    Set-Content -LiteralPath $header -Value "fake-header-content" -NoNewline

    $commit = "0123456789abcdef0123456789abcdef01234567"
    $manifest = Join-Path $work "viiper-artifact.json"

    & pwsh -NoProfile -NonInteractive -File $generate `
        -DllPath $dll -HeaderPath $header -OutputPath $manifest -Commit $commit
    Check "generate succeeds and writes manifest" (Test-Path $manifest)

    $exit = Run-Verify $manifest $dll $header $commit
    Check "verify succeeds on matching manifest" ($exit -eq 0)

    $exit = Run-Verify $manifest $dll $header "ffffffffffffffffffffffffffffffffffffff"
    Check "verify fails on wrong expected commit" ($exit -ne 0)

    $badCommitManifest = Join-Path $work "bad-commit.json"
    (Get-Content $manifest -Raw) -replace $commit, "deadbeef" | Set-Content $badCommitManifest
    $exit = Run-Verify $badCommitManifest $dll $header $commit
    Check "verify fails on malformed/short commit" ($exit -ne 0)

    $json = Get-Content $manifest -Raw | ConvertFrom-Json
    $json.dll.sha256 = "0" * 64
    $badDllManifest = Join-Path $work "bad-dll.json"
    $json | ConvertTo-Json -Depth 10 | Set-Content $badDllManifest
    $exit = Run-Verify $badDllManifest $dll $header $commit
    Check "verify fails on wrong dll hash" ($exit -ne 0)

    $json = Get-Content $manifest -Raw | ConvertFrom-Json
    $json.header.sha256 = "0" * 64
    $badHeaderManifest = Join-Path $work "bad-header.json"
    $json | ConvertTo-Json -Depth 10 | Set-Content $badHeaderManifest
    $exit = Run-Verify $badHeaderManifest $dll $header $commit
    Check "verify fails on wrong header hash" ($exit -ne 0)

    $missingDll = Join-Path $work "missing.dll"
    $exit = Run-Verify $manifest $missingDll $header $commit
    Check "verify fails on missing dll" ($exit -ne 0)

    $missingHeader = Join-Path $work "missing.h"
    $exit = Run-Verify $manifest $dll $missingHeader $commit
    Check "verify fails on missing header" ($exit -ne 0)

    $json = Get-Content $manifest -Raw | ConvertFrom-Json
    $json.schema_version = 999
    $badSchemaManifest = Join-Path $work "bad-schema.json"
    $json | ConvertTo-Json -Depth 10 | Set-Content $badSchemaManifest
    $exit = Run-Verify $badSchemaManifest $dll $header $commit
    Check "verify fails on unsupported schema_version" ($exit -ne 0)
}
finally {
    Remove-Item -Recurse -Force $work -ErrorAction SilentlyContinue
}

if ($failures -gt 0) {
    Write-Host "$failures check(s) failed"
    exit 1
}
Write-Host "All manifest script checks passed"
