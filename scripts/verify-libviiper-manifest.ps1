<#
Verifies viiper-artifact.json against the exact DLL/header files it claims to
describe and against the expected commit SHA. Fails closed: any mismatch,
missing file, or malformed field is a non-zero exit.
#>
param(
    [Parameter(Mandatory = $true)][string]$ManifestPath,
    [Parameter(Mandatory = $true)][string]$DllPath,
    [Parameter(Mandatory = $true)][string]$HeaderPath,
    [string]$ExpectedCommit,
    [string]$ExpectedRepository = "onehoon/VIIPER",
    [string]$ExpectedBuildEntrypoint = "just build-libVIIPER Release",
    [string]$ExpectedPlatform = "windows",
    [string]$ExpectedArchitecture = "amd64"
)

$ErrorActionPreference = "Stop"

$SupportedSchemaVersions = @(1)

function Fail([string]$Message) {
    Write-Error $Message
    exit 1
}

if (-not (Test-Path -LiteralPath $ManifestPath)) {
    Fail "Manifest not found at path: $ManifestPath"
}
if (-not (Test-Path -LiteralPath $DllPath)) {
    Fail "DLL not found at path: $DllPath"
}
if (-not (Test-Path -LiteralPath $HeaderPath)) {
    Fail "Header not found at path: $HeaderPath"
}

try {
    $manifest = Get-Content -LiteralPath $ManifestPath -Raw | ConvertFrom-Json
}
catch {
    Fail "Manifest is not valid JSON: $($_.Exception.Message)"
}

if (-not $manifest.schema_version -or ($SupportedSchemaVersions -notcontains [int]$manifest.schema_version)) {
    Fail "Unsupported or missing schema_version: '$($manifest.schema_version)'"
}

if (-not $manifest.commit -or $manifest.commit -notmatch '^[0-9a-fA-F]{40}$') {
    Fail "Manifest commit is missing or not a full 40-character SHA: '$($manifest.commit)'"
}

if (-not $ExpectedCommit) {
    $ExpectedCommit = (git rev-parse HEAD).Trim()
}
if ($ExpectedCommit -notmatch '^[0-9a-fA-F]{40}$') {
    Fail "Expected commit is not a full 40-character SHA: '$ExpectedCommit'"
}
if ($manifest.commit.ToLowerInvariant() -ne $ExpectedCommit.ToLowerInvariant()) {
    Fail "Manifest commit '$($manifest.commit)' does not match expected checkout commit '$ExpectedCommit'"
}

if ($manifest.repository -ne $ExpectedRepository) {
    Fail "Manifest repository '$($manifest.repository)' does not match expected '$ExpectedRepository'"
}
if ($manifest.build_entrypoint -ne $ExpectedBuildEntrypoint) {
    Fail "Manifest build_entrypoint '$($manifest.build_entrypoint)' does not match expected '$ExpectedBuildEntrypoint'"
}
if ($manifest.platform -ne $ExpectedPlatform) {
    Fail "Manifest platform '$($manifest.platform)' does not match expected '$ExpectedPlatform'"
}
if ($manifest.architecture -ne $ExpectedArchitecture) {
    Fail "Manifest architecture '$($manifest.architecture)' does not match expected '$ExpectedArchitecture'"
}

if (-not $manifest.dll -or -not $manifest.dll.sha256) {
    Fail "Manifest is missing dll.sha256"
}
if (-not $manifest.header -or -not $manifest.header.sha256) {
    Fail "Manifest is missing header.sha256"
}
$expectedDllFile = Split-Path -Leaf $DllPath
$expectedHeaderFile = Split-Path -Leaf $HeaderPath
if ($manifest.dll.file -ne $expectedDllFile) {
    Fail "Manifest dll.file '$($manifest.dll.file)' does not match expected '$expectedDllFile'"
}
if ($manifest.header.file -ne $expectedHeaderFile) {
    Fail "Manifest header.file '$($manifest.header.file)' does not match expected '$expectedHeaderFile'"
}

$actualDllHash = (Get-FileHash -LiteralPath $DllPath -Algorithm SHA256).Hash.ToLowerInvariant()
$actualHeaderHash = (Get-FileHash -LiteralPath $HeaderPath -Algorithm SHA256).Hash.ToLowerInvariant()

if ($manifest.dll.sha256.ToLowerInvariant() -ne $actualDllHash) {
    Fail "DLL hash mismatch: manifest has '$($manifest.dll.sha256)', actual file hash is '$actualDllHash'"
}
if ($manifest.header.sha256.ToLowerInvariant() -ne $actualHeaderHash) {
    Fail "Header hash mismatch: manifest has '$($manifest.header.sha256)', actual file hash is '$actualHeaderHash'"
}

Write-Host "Manifest verified OK"
Write-Host "  commit: $($manifest.commit)"
Write-Host "  dll:    $actualDllHash"
Write-Host "  header: $actualHeaderHash"
exit 0
