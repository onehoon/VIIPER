param(
    [Parameter(Mandatory = $true)]
    [string]$Version,
    [Parameter(Mandatory = $true)]
    [string]$InputJson,
    [Parameter(Mandatory = $true)]
    [string]$OutputJson
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$inputVersion = $Version.Trim()
if ([string]::IsNullOrWhiteSpace($inputVersion)) {
    throw "Version must not be empty."
}

$displayVersion = $inputVersion.TrimStart('v')
$major = 0
$minor = 0
$patch = 0
$build = 0
$parsed = $false

if ($inputVersion -match '^v?(\d+)\.(\d+)\.(\d+)-(\d+)-g[0-9a-fA-F]+$') {
    $major = [int]$Matches[1]
    $minor = [int]$Matches[2]
    $patch = [int]$Matches[3]
    $build = [int]$Matches[4]
    $parsed = $true
}
elseif ($inputVersion -match '^v?(\d+)\.(\d+)\.(\d+)\.(\d+)$') {
    $major = [int]$Matches[1]
    $minor = [int]$Matches[2]
    $patch = [int]$Matches[3]
    $build = [int]$Matches[4]
    $parsed = $true
}
elseif ($inputVersion -match '^v?(\d+)\.(\d+)\.(\d+)$') {
    $major = [int]$Matches[1]
    $minor = [int]$Matches[2]
    $patch = [int]$Matches[3]
    $parsed = $true
}
elseif ($inputVersion -match '^[0-9a-fA-F]{7,40}$') {
    $displayVersion = $inputVersion
    $parsed = $true
}

if (-not $parsed) {
    throw "Unsupported version format '$Version'. Expected semver, git-describe semver, four-component numeric version, or a 7-40 character Git SHA."
}

foreach ($component in @($major, $minor, $patch, $build)) {
    if ($component -lt 0 -or $component -gt 65535) {
        throw "Version component '$component' is outside the Windows version-resource range 0..65535."
    }
}

$json = Get-Content $InputJson -Raw | ConvertFrom-Json
$json.FixedFileInfo.FileVersion.Major = $major
$json.FixedFileInfo.FileVersion.Minor = $minor
$json.FixedFileInfo.FileVersion.Patch = $patch
$json.FixedFileInfo.FileVersion.Build = $build
$json.FixedFileInfo.ProductVersion.Major = $major
$json.FixedFileInfo.ProductVersion.Minor = $minor
$json.FixedFileInfo.ProductVersion.Patch = $patch
$json.FixedFileInfo.ProductVersion.Build = $build
$json.StringFileInfo.FileVersion = "$major.$minor.$patch.$build"
$json.StringFileInfo.ProductVersion = $displayVersion

$json | ConvertTo-Json -Depth 10 | Set-Content -LiteralPath $OutputJson -Encoding utf8NoBOM
