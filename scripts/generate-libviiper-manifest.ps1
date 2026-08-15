<#
Generates viiper-artifact.json for the canonical Windows libVIIPER build.
Source identity is the full 40-character Git commit SHA of the current checkout;
DLL/header hashes are computed from the exact files produced by
`just build-libVIIPER Release` in the same job.
#>
param(
    [Parameter(Mandatory = $true)][string]$DllPath,
    [Parameter(Mandatory = $true)][string]$HeaderPath,
    [Parameter(Mandatory = $true)][string]$OutputPath,
    [string]$Commit,
    [string]$Repository = "onehoon/VIIPER",
    [string]$BuildEntrypoint = "just build-libVIIPER Release",
    [string]$Platform = "windows",
    [string]$Architecture = "amd64"
)

$ErrorActionPreference = "Stop"

if (-not (Test-Path -LiteralPath $DllPath)) {
    throw "DLL not found at path: $DllPath"
}
if (-not (Test-Path -LiteralPath $HeaderPath)) {
    throw "Header not found at path: $HeaderPath"
}

if (-not $Commit) {
    $Commit = (git rev-parse HEAD).Trim()
}
if ($Commit -notmatch '^[0-9a-fA-F]{40}$') {
    throw "Commit '$Commit' is not a full 40-character Git SHA"
}

$dllHash = (Get-FileHash -LiteralPath $DllPath -Algorithm SHA256).Hash.ToLowerInvariant()
$headerHash = (Get-FileHash -LiteralPath $HeaderPath -Algorithm SHA256).Hash.ToLowerInvariant()

$manifest = [ordered]@{
    schema_version   = 1
    repository       = $Repository
    commit           = $Commit.ToLowerInvariant()
    build_entrypoint = $BuildEntrypoint
    platform         = $Platform
    architecture     = $Architecture
    dll              = [ordered]@{
        file   = (Split-Path -Leaf $DllPath)
        sha256 = $dllHash
    }
    header           = [ordered]@{
        file   = (Split-Path -Leaf $HeaderPath)
        sha256 = $headerHash
    }
}

$manifest | ConvertTo-Json -Depth 10 | Set-Content -LiteralPath $OutputPath -Encoding utf8NoBOM

Write-Host "Wrote manifest to $OutputPath"
Write-Host "  commit: $($manifest.commit)"
Write-Host "  dll:    $($manifest.dll.file) $($manifest.dll.sha256)"
Write-Host "  header: $($manifest.header.file) $($manifest.header.sha256)"
