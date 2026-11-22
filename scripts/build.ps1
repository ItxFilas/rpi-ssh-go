param(
    [string]$Target = "dist/rpi-ssh.exe",
    [switch]$PackageZip,
    [string]$ZipPath = "dist/rpi-ssh-go.zip"
)

$ErrorActionPreference = "Stop"

# Determine repository root (one level up from scripts/)
$repoRoot = Split-Path -Parent $PSScriptRoot
Set-Location $repoRoot

# Ensure output directory exists
$targetPath = [System.IO.Path]::GetFullPath((Join-Path $repoRoot $Target))
$targetDir = Split-Path -Parent $targetPath
if (-not (Test-Path $targetDir)) {
    New-Item -ItemType Directory -Path $targetDir -Force | Out-Null
}

Write-Host "Building production binary: $targetPath"
& go build -ldflags="-s -w" -o $targetPath .

if ($LASTEXITCODE -ne 0) {
    Write-Error "Build failed with exit code $LASTEXITCODE"
    exit $LASTEXITCODE
}

if (-not (Test-Path $targetPath)) {
    Write-Error "Target executable not found at $targetPath"
    exit 1
}

$fileInfo = Get-Item $targetPath
$sha256 = (Get-FileHash -Algorithm SHA256 -Path $targetPath).Hash
$sizeBytes = $fileInfo.Length
$sizeMB = [math]::Round($sizeBytes / 1MB, 2)

Write-Host "================ Build Summary ================"
Write-Host "Target:  $targetPath"
Write-Host "Size:    $sizeBytes bytes ($sizeMB MB)"
Write-Host "SHA-256: $sha256"
Write-Host "==============================================="

if ($PackageZip) {
    $zipFullPath = [System.IO.Path]::GetFullPath((Join-Path $repoRoot $ZipPath))
    $zipDir = Split-Path -Parent $zipFullPath
    if (-not (Test-Path $zipDir)) {
        New-Item -ItemType Directory -Path $zipDir -Force | Out-Null
    }
    if (Test-Path $zipFullPath) {
        Remove-Item -Force $zipFullPath
    }

    $stageDir = Join-Path $targetDir "_staging"
    if (Test-Path $stageDir) {
        Remove-Item -Recurse -Force $stageDir
    }
    New-Item -ItemType Directory -Path $stageDir -Force | Out-Null

    try {
        # Copy binary and user documentation
        Copy-Item -Path $targetPath -Destination (Join-Path $stageDir "rpi-ssh.exe") -Force
        foreach ($doc in @("README.md", "LICENSE", "CHANGELOG.md")) {
            if (Test-Path (Join-Path $repoRoot $doc)) {
                Copy-Item -Path (Join-Path $repoRoot $doc) -Destination (Join-Path $stageDir $doc) -Force
            }
        }

        # Audit packaging: strictly ensure no forbidden files (.env, cookies, .git, backup executables)
        $stagedFiles = Get-ChildItem -Recurse $stageDir
        $prohibitedPatterns = @("*.env*", "*cookie*", "*.git*", "*~*", "*pre-firewall*")
        foreach ($item in $stagedFiles) {
            foreach ($pat in $prohibitedPatterns) {
                if ($item.Name -like $pat) {
                    throw "Packaging audit failure: forbidden file '$($item.Name)' staged for release package!"
                }
            }
        }

        Compress-Archive -Path "$stageDir\*" -DestinationPath $zipFullPath -Force
        $zipHash = (Get-FileHash -Algorithm SHA256 -Path $zipFullPath).Hash
        Write-Host "================ Release Package =============="
        Write-Host "ZIP Archive: $zipFullPath"
        Write-Host "ZIP SHA-256: $zipHash"
        Write-Host "Audited contents (excluded .env, cookies, .git, and backup executables)"
        Write-Host "==============================================="
    }
    finally {
        if (Test-Path $stageDir) {
            Remove-Item -Recurse -Force $stageDir
        }
    }
}

[pscustomobject]@{
    Path      = $targetPath
    SizeBytes = $sizeBytes
    SizeMB    = $sizeMB
    SHA256    = $sha256
}
