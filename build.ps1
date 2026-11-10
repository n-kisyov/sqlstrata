<#
.SYNOPSIS
    Builds SQLStrata and optional helper tools on Windows.

.DESCRIPTION
    Compiles sqlstrata.exe (pure Go, CGO_ENABLED=0) with stripped symbols and
    trimmed paths. Supports optional flags for running tests, building the
    synthetic database generator (gendb.exe), hiding the console window
    (-H=windowsgui), cleaning artifacts, and launching the app after build.

.EXAMPLE
    .\build.ps1
    Builds sqlstrata.exe in the repository root.

.EXAMPLE
    .\build.ps1 -Test -Tools
    Runs go vet & go test, then builds both sqlstrata.exe and gendb.exe.

.EXAMPLE
    .\build.ps1 -Gui -Run
    Builds sqlstrata.exe as a Windows GUI subsystem binary (no console window) and runs it.
#>
[CmdletBinding()]
param(
    [switch]$Test,
    [switch]$Tools,
    [switch]$Gui,
    [switch]$Clean,
    [switch]$Run,
    [string]$OutDir = $PSScriptRoot
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

Push-Location $PSScriptRoot
try {
    $env:CGO_ENABLED = '0'
    $env:GOOS = 'windows'

    if (!(Test-Path -Path $OutDir)) {
        New-Item -ItemType Directory -Path $OutDir | Out-Null
    }

    $appExe   = Join-Path $OutDir 'sqlstrata.exe'
    $gendbExe = Join-Path $OutDir 'gendb.exe'

    if ($Clean) {
        Write-Host '==> Cleaning build artifacts...' -ForegroundColor Cyan
        foreach ($f in @($appExe, $gendbExe)) {
            if (Test-Path $f) {
                Remove-Item -Force $f
                Write-Host "    Removed $f" -ForegroundColor DarkGray
            }
        }
    }

    if ($Test) {
        Write-Host '==> Running go vet...' -ForegroundColor Cyan
        & go vet ./...
        if ($LASTEXITCODE -ne 0) {
            throw "go vet failed with exit code $LASTEXITCODE"
        }

        Write-Host '==> Running go test...' -ForegroundColor Cyan
        & go test ./...
        if ($LASTEXITCODE -ne 0) {
            throw "go test failed with exit code $LASTEXITCODE"
        }
    }

    $ldflags = '-s -w'
    if ($Gui) {
        $ldflags = '-s -w -H=windowsgui'
    }

    Write-Host "==> Building sqlstrata.exe (ldflags: '$ldflags')..." -ForegroundColor Cyan
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    & go build -trimpath -ldflags $ldflags -o $appExe ./cmd/sqlstrata
    if ($LASTEXITCODE -ne 0) {
        throw "Failed to build sqlstrata.exe (exit code $LASTEXITCODE)"
    }
    $sw.Stop()

    $appSizeMB = [math]::Round((Get-Item $appExe).Length / 1MB, 2)
    Write-Host ("    Built {0} ({1} MB) in {2:N2}s" -f $appExe, $appSizeMB, $sw.Elapsed.TotalSeconds) -ForegroundColor Green

    if ($Tools) {
        Write-Host '==> Building gendb.exe...' -ForegroundColor Cyan
        $sw.Restart()
        & go build -trimpath -ldflags '-s -w' -o $gendbExe ./cmd/gendb
        if ($LASTEXITCODE -ne 0) {
            throw "Failed to build gendb.exe (exit code $LASTEXITCODE)"
        }
        $sw.Stop()

        $toolSizeMB = [math]::Round((Get-Item $gendbExe).Length / 1MB, 2)
        Write-Host ("    Built {0} ({1} MB) in {2:N2}s" -f $gendbExe, $toolSizeMB, $sw.Elapsed.TotalSeconds) -ForegroundColor Green
    }

    if ($Run) {
        Write-Host '==> Launching sqlstrata.exe...' -ForegroundColor Cyan
        & $appExe
    }
}
finally {
    Pop-Location
}

