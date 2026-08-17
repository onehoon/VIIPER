Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $PSScriptRoot
$script = Join-Path $root 'inject-version.ps1'
$fixture = Join-Path $root '..\lib\viiper\versioninfo.json'
$work = Join-Path ([System.IO.Path]::GetTempPath()) ('viiper-version-test-' + [guid]::NewGuid())
New-Item -ItemType Directory -Path $work | Out-Null

function Check([bool]$condition, [string]$message) {
    if (-not $condition) { throw "FAIL: $message" }
}

function Invoke-VersionCase([string]$version, [int[]]$numeric, [string]$productVersion) {
    $input = Join-Path $work 'input.json'
    $output = Join-Path $work 'output.json'
    Copy-Item -LiteralPath $fixture -Destination $input -Force
    & pwsh -NoProfile -NonInteractive -File $script $version $input $output
    Check ($LASTEXITCODE -eq 0) "script failed for '$version'"
    $json = Get-Content -LiteralPath $output -Raw | ConvertFrom-Json
    $fixed = $json.FixedFileInfo.FileVersion
    Check ($fixed.Major -eq $numeric[0] -and $fixed.Minor -eq $numeric[1] -and $fixed.Patch -eq $numeric[2] -and $fixed.Build -eq $numeric[3]) "numeric version mismatch for '$version'"
    $fixedProduct = $json.FixedFileInfo.ProductVersion
    Check ($fixedProduct.Major -eq $numeric[0] -and $fixedProduct.Minor -eq $numeric[1] -and $fixedProduct.Patch -eq $numeric[2] -and $fixedProduct.Build -eq $numeric[3]) "numeric ProductVersion mismatch for '$version'"
    Check ($json.StringFileInfo.FileVersion -eq ("{0}.{1}.{2}.{3}" -f $numeric[0], $numeric[1], $numeric[2], $numeric[3])) "string FileVersion mismatch for '$version'"
    Check ($json.StringFileInfo.ProductVersion -eq $productVersion) "ProductVersion mismatch for '$version'"
}

try {
    Invoke-VersionCase 'v1.2.3' @(1, 2, 3, 0) '1.2.3'
    Invoke-VersionCase 'v1.2.3-5-gabcdef0' @(1, 2, 3, 5) '1.2.3-5-gabcdef0'
    Invoke-VersionCase '1.2.3.4' @(1, 2, 3, 4) '1.2.3.4'
    Invoke-VersionCase 'ba63b99' @(0, 0, 0, 0) 'ba63b99'
    Invoke-VersionCase 'ba63b9909f84bcabeddd4b1299beffe76ba04b4f' @(0, 0, 0, 0) 'ba63b9909f84bcabeddd4b1299beffe76ba04b4f'

    $invalidInput = Join-Path $work 'invalid-input.json'
    $invalidOutput = Join-Path $work 'invalid-output.json'
    Copy-Item -LiteralPath $fixture -Destination $invalidInput -Force
    & pwsh -NoProfile -NonInteractive -File $script 'not-a-version' $invalidInput $invalidOutput 2>$null
    Check ($LASTEXITCODE -ne 0) 'malformed version unexpectedly succeeded'
    Check (-not (Test-Path -LiteralPath $invalidOutput)) 'malformed version produced output'

    $rangeOutput = Join-Path $work 'range-output.json'
    Copy-Item -LiteralPath $fixture -Destination $invalidInput -Force
    & pwsh -NoProfile -NonInteractive -File $script '1.2.3.65536' $invalidInput $rangeOutput 2>$null
    Check ($LASTEXITCODE -ne 0) 'out-of-range version unexpectedly succeeded'
    Check (-not (Test-Path -LiteralPath $rangeOutput)) 'out-of-range version produced output'
    Write-Host 'All inject-version checks passed'
}
finally {
    if (Test-Path -LiteralPath $work) {
        Remove-Item -LiteralPath $work -Recurse -Force
    }
}

exit 0
