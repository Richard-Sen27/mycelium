<#
.SYNOPSIS
    Install mycel for the current Windows user. Never needs administrator rights.

.DESCRIPTION
    Downloads mycel_<version>_windows_<arch>.zip and checksums.txt from the
    GitHub release, verifies the SHA-256 checksum with Get-FileHash, installs
    mycel.exe to %LOCALAPPDATA%\mycel\bin (or $env:MYCEL_INSTALL_DIR) and adds
    that directory to the user PATH. -FromSource builds with Go 1.27 instead.

    irm https://raw.githubusercontent.com/Richard-Sen27/mycelium/main/scripts/install.ps1 -OutFile install.ps1
    powershell -ExecutionPolicy Bypass -File .\install.ps1 [-Version v1.2.3] [-FromSource [-Ref main]] [-DryRun]

    Environment: MYCEL_REPO, MYCEL_INSTALL_DIR, MYCEL_DOWNLOAD_BASE, GITHUB_TOKEN.
#>
[CmdletBinding()]
param(
    [string]$Version = '',
    [switch]$FromSource,
    [string]$Ref = '',
    [switch]$DryRun,
    [string]$Repo = '',
    [string]$InstallDir = ''
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue' # Invoke-WebRequest is much faster without the progress bar
# Windows PowerShell 5 may default to TLS 1.0/1.1, which GitHub refuses.
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

$Module = 'github.com/richard-sen27/mycelium'
$GoMinMinor = 27
if (-not $Repo) { $Repo = if ($env:MYCEL_REPO) { $env:MYCEL_REPO } else { 'Richard-Sen27/mycelium' } }
if (-not $InstallDir) {
    $InstallDir = if ($env:MYCEL_INSTALL_DIR) { $env:MYCEL_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'mycel\bin' }
}
if ($Version -and -not $Version.StartsWith('v')) { $Version = "v$Version" }

function Write-Step([string]$Message) { Write-Output "+ $Message" }

function Exit-Install([string]$Message) {
    Write-Error "install.ps1: $Message" -ErrorAction Continue
    exit 1
}

function Get-Download([string]$Url, [string]$OutFile) {
    Write-Step "download $Url -> $OutFile"
    if ($DryRun) { return }
    $headers = @{ 'User-Agent' = 'mycel-install' }
    if ($env:GITHUB_TOKEN -and $Url.StartsWith('https://api.github.com/')) {
        $headers['Authorization'] = "Bearer $($env:GITHUB_TOKEN)"
    }
    try {
        Invoke-WebRequest -UseBasicParsing -Uri $Url -OutFile $OutFile -Headers $headers
    } catch {
        Exit-Install "download failed: $Url ($($_.Exception.Message))"
    }
}

# A 32-bit PowerShell on 64-bit Windows reports x86 in PROCESSOR_ARCHITECTURE.
$cpu = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
switch ($cpu) {
    'AMD64' { $Arch = 'amd64' }
    'ARM64' { $Arch = 'amd64' } # x64 emulation; no native windows/arm64 build yet
    default { Exit-Install "unsupported CPU $cpu" }
}

if ($DryRun) { Write-Output 'dry run: nothing is downloaded, built or installed' }
Write-Output "platform: windows/$Arch; install dir: $InstallDir"

if ($DryRun) {
    $Tmp = '<tmp>'
} else {
    $Tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("mycel-install-" + [System.Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $Tmp | Out-Null
}

try {
    if (-not $DryRun) { New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null } else { Write-Step "mkdir $InstallDir" }
    $Target = Join-Path $InstallDir 'mycel.exe'

    if ($FromSource) {
        if (-not (Get-Command git -ErrorAction SilentlyContinue)) { Exit-Install 'git is required for -FromSource' }
        if ($DryRun) {
            Write-Step "check: go version is 1.$GoMinMinor or newer"
        } else {
            if (-not (Get-Command go -ErrorAction SilentlyContinue)) { Exit-Install "Go 1.$GoMinMinor or newer is required for -FromSource; see https://go.dev/dl/" }
            $gov = (& go env GOVERSION)
            if ($gov -notmatch '^go1\.(\d+)' -or [int]$Matches[1] -lt $GoMinMinor) {
                Exit-Install "Go 1.$GoMinMinor or newer is required for -FromSource (found $gov)"
            }
        }
        $gitRef = if ($Ref) { $Ref } elseif ($Version) { $Version } else { 'main' }
        $src = Join-Path $Tmp 'src'
        Write-Step "git clone --depth 1 --branch $gitRef https://github.com/$Repo.git $src"
        if (-not $DryRun) {
            & git clone --quiet --depth 1 --branch $gitRef "https://github.com/$Repo.git" $src
            if ($LASTEXITCODE -ne 0) { Exit-Install 'git clone failed' }
        }
        if (Get-Command pnpm -ErrorAction SilentlyContinue) {
            Write-Step "(cd $src\web; pnpm install --frozen-lockfile; pnpm build)"
            if (-not $DryRun) {
                Push-Location (Join-Path $src 'web')
                try {
                    & pnpm install --frozen-lockfile; if ($LASTEXITCODE -ne 0) { Exit-Install 'pnpm install failed' }
                    & pnpm build; if ($LASTEXITCODE -ne 0) { Exit-Install 'pnpm build failed' }
                } finally { Pop-Location }
            }
        } else {
            Write-Output 'pnpm not found: building without the web UI (install Node 22 and pnpm to embed it)'
        }
        Write-Step "(cd $src; go build -ldflags `"-X $Module/internal/version.Version=$gitRef`" -o $Target ./cmd/mycel)"
        if (-not $DryRun) {
            Push-Location $src
            try {
                $env:CGO_ENABLED = '0'
                & go build -ldflags "-s -w -X $Module/internal/version.Version=$gitRef" -o "$Target.new" ./cmd/mycel
                if ($LASTEXITCODE -ne 0) { Exit-Install 'go build failed' }
            } finally { Pop-Location }
            Move-Item -Force "$Target.new" $Target
        }
    } else {
        if (-not $Version) {
            $api = "https://api.github.com/repos/$Repo/releases/latest"
            Get-Download $api (Join-Path $Tmp 'latest.json')
            if ($DryRun) {
                $Version = '<latest>'
            } else {
                $Version = (Get-Content -Raw (Join-Path $Tmp 'latest.json') | ConvertFrom-Json).tag_name
                if (-not $Version) { Exit-Install "no release found at $api (try -Version or -FromSource)" }
            }
        }
        $num = $Version.TrimStart('v')
        $archive = "mycel_${num}_windows_$Arch.zip"
        $base = if ($env:MYCEL_DOWNLOAD_BASE) { $env:MYCEL_DOWNLOAD_BASE } else { "https://github.com/$Repo/releases/download" }
        $base = "$base/$Version"
        Write-Output "release: $Version ($archive)"
        Get-Download "$base/$archive" (Join-Path $Tmp $archive)
        Get-Download "$base/checksums.txt" (Join-Path $Tmp 'checksums.txt')

        Write-Step "verify sha256 of $archive against checksums.txt"
        if (-not $DryRun) {
            $want = $null
            foreach ($line in Get-Content (Join-Path $Tmp 'checksums.txt')) {
                $parts = $line -split '\s+', 2
                if ($parts.Count -eq 2 -and $parts[1].TrimStart('*') -eq $archive) { $want = $parts[0].ToLower() }
            }
            if (-not $want) { Exit-Install "$archive is not listed in checksums.txt" }
            $got = (Get-FileHash -Algorithm SHA256 -Path (Join-Path $Tmp $archive)).Hash.ToLower()
            if ($want -ne $got) { Exit-Install "checksum mismatch for ${archive}: expected $want, got $got" }
            Write-Output "checksum ok: $got"
        }
        Write-Step "Expand-Archive $archive -> $Tmp\x"
        Write-Step "copy mycel.exe -> $Target"
        if (-not $DryRun) {
            Expand-Archive -Path (Join-Path $Tmp $archive) -DestinationPath (Join-Path $Tmp 'x') -Force
            # A running daemon locks mycel.exe; renaming it out of the way works anyway.
            if (Test-Path "$Target.old") {
                try { Remove-Item -Force "$Target.old" -ErrorAction Stop }
                catch { Exit-Install "cannot remove $Target.old: a daemon from an earlier update still runs it; run 'mycel service stop' and try again" }
            }
            if (Test-Path $Target) {
                try { Rename-Item -Path $Target -NewName 'mycel.exe.old' -ErrorAction Stop }
                catch { Exit-Install "cannot replace ${Target}: $($_.Exception.Message)" }
            }
            Copy-Item -Force (Join-Path $Tmp 'x\mycel.exe') $Target
        }
    }

    # Add the install dir to the user PATH (no admin rights needed). Read and
    # write the raw registry value so %VAR% entries stay unexpanded. (Off
    # Windows, where only the dry-run tests run this, there is no registry.)
    $onWindows = [Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT
    $envKey = $null
    $entries = @()
    if ($onWindows) {
        $envKey = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
        if (-not $envKey) { $envKey = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey('Environment') }
        $userPath = [string]$envKey.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
        $entries = @($userPath -split ';' | Where-Object { $_ })
    }
    if ($entries -notcontains $InstallDir) {
        Write-Step "add $InstallDir to the user PATH"
        if (-not $DryRun -and $envKey) {
            $envKey.SetValue('Path', (($entries + $InstallDir) -join ';'), [Microsoft.Win32.RegistryValueKind]::ExpandString)
            # Setting any user variable through .NET broadcasts WM_SETTINGCHANGE,
            # so new terminals see the new PATH.
            [Environment]::SetEnvironmentVariable('MYCEL_INSTALL_REFRESH', '1', 'User')
            [Environment]::SetEnvironmentVariable('MYCEL_INSTALL_REFRESH', $null, 'User')
        }
        Write-Output 'Open a new terminal for the PATH change to take effect.'
    }
    if ($envKey) { $envKey.Close() }
    if ($env:Path -split ';' -notcontains $InstallDir) { $env:Path = "$env:Path;$InstallDir" }

    if ($DryRun) {
        Write-Output "would install: $Target"
    } else {
        $v = & $Target version
        if ($LASTEXITCODE -ne 0) { Exit-Install "the installed binary does not run: $Target" }
        Write-Output "installed: $Target ($v)"
    }
} finally {
    if (-not $DryRun -and (Test-Path $Tmp)) { Remove-Item -Recurse -Force $Tmp -ErrorAction SilentlyContinue }
}

Write-Output ''
Write-Output 'Next:'
Write-Output '  joining a network:   mycel join <token>   then   mycel service install'
Write-Output '  starting a network:  mycel hub init --advertise https://<this-host>:7777   then   mycel service install'
Write-Output "  quickstart:          https://github.com/$Repo/blob/main/docs/quickstart.md"
