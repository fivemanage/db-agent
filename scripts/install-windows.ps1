#Requires -Version 5.1
[CmdletBinding()]
param(
    [switch]$ServiceOnly,
    [string]$Config
)

# The publisher verifies the Ed25519 manifest before pinning these values. No
# downloaded checksum, metadata, or environment variable can replace this trust.
$script:DbAgentReleaseVersion = ''
$script:DbAgentReleaseUrl = ''
$script:DbAgentReleaseSHA256 = ''

function Assert-DbAgentUri {
    param([uri]$Uri, [switch]$AllowQuery)
    if (-not $Uri.IsAbsoluteUri -or $Uri.Scheme -cne 'https' -or -not $Uri.Host -or
        $Uri.UserInfo -or $Uri.Fragment -or (-not $AllowQuery -and $Uri.Query)) {
        throw 'The installer requires HTTPS URLs without credentials or fragments.'
    }
}

function Get-DbAgentRelease {
    if (-not $script:DbAgentReleaseVersion -or $script:DbAgentReleaseSHA256 -cnotmatch '^[0-9a-f]{64}$') {
        throw 'This installer has not been provisioned. Obtain the published FiveManage installer.'
    }
    $uri = $null
    if (-not [uri]::TryCreate($script:DbAgentReleaseUrl, [UriKind]::Absolute, [ref]$uri)) {
        throw 'The published installer has an invalid release URL.'
    }
    Assert-DbAgentUri $uri
    [pscustomobject]@{ Version = $script:DbAgentReleaseVersion; Uri = $uri; SHA256 = $script:DbAgentReleaseSHA256 }
}

function Get-DbAgentNativeArchitecture {
    $runtime = 'System.Runtime.InteropServices.RuntimeInformation' -as [type]
    if ($null -ne $runtime) { return $runtime::OSArchitecture.ToString() }
    $processors = @(Get-CimInstance -ClassName Win32_Processor -Property Architecture -ErrorAction Stop)
    if ($processors.Count -gt 0 -and @($processors | Where-Object { $_.Architecture -ne 9 }).Count -eq 0) { return 'X64' }
    return 'Unsupported'
}

function Assert-DbAgentWindows {
    if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) { throw 'This installer supports Windows only.' }
    if (-not [Environment]::Is64BitProcess -or -not [Environment]::Is64BitOperatingSystem) {
        throw 'Use 64-bit PowerShell, not Windows PowerShell (x86). Only Windows amd64 is supported.'
    }
    if ((Get-DbAgentNativeArchitecture) -ne 'X64') { throw 'Unsupported Windows architecture; only amd64 is supported.' }
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = [Security.Principal.WindowsPrincipal]::new($identity)
    if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw 'Open PowerShell with Run as administrator, then rerun the installer. Nothing was installed.'
    }
}

function Get-DbAgentProgramDirectory {
    [Environment]::GetFolderPath([Environment+SpecialFolder]::ProgramFiles)
}

function Assert-DbAgentDirectory {
    param([string]$Path, [switch]$Ancestor)
    $item = Get-Item -LiteralPath $Path -Force -ErrorAction Stop
    if (-not $item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
        throw 'Installation paths must be ordinary directories, not junctions or symbolic links.'
    }
    $acl = Get-Acl -LiteralPath $Path -ErrorAction Stop
    $trusted = @('S-1-5-18', 'S-1-5-32-544', 'S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464')
    if ($acl.GetOwner([Security.Principal.SecurityIdentifier]).Value -notin $trusted) {
        throw 'The installation path has an unsafe owner. Inspect its permissions before retrying.'
    }
    $rights = [Security.AccessControl.FileSystemRights]'ChangePermissions, TakeOwnership, DeleteSubdirectoriesAndFiles'
    if (-not $Ancestor) { $rights = $rights -bor [Security.AccessControl.FileSystemRights]'Write, Delete' }
    $rules = @($acl.GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier]))
    if ($rules.Count -eq 0) { throw 'The installation path has an empty or missing access policy.' }
    foreach ($rule in $rules) {
        if ($rule.AccessControlType -eq [Security.AccessControl.AccessControlType]::Allow -and
            -not ($rule.PropagationFlags -band [Security.AccessControl.PropagationFlags]::InheritOnly) -and
            ($rule.FileSystemRights -band $rights) -and $rule.IdentityReference.Value -notin $trusted) {
            throw 'The installation path permits non-administrator modification. Inspect its permissions before retrying.'
        }
    }
}

function New-DbAgentDirectory {
    [Diagnostics.CodeAnalysis.SuppressMessageAttribute('PSUseShouldProcessForStateChangingFunctions', '', Justification = 'An explicit installer; a partial WhatIf would not safely simulate verification and publication.')]
    [CmdletBinding()]
    param([string]$Path)
    # An individual user's ownership would let their unelevated token change
    # the DACL. Apply Administrators ownership and the DACL at creation time.
    $security = [Security.AccessControl.DirectorySecurity]::new()
    $security.SetSecurityDescriptorSddlForm('O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)')
    if ($PSVersionTable.PSEdition -eq 'Desktop') {
        [void][IO.Directory]::CreateDirectory($Path, $security)
    } else {
        [void][IO.FileSystemAclExtensions]::CreateDirectory($security, $Path)
    }
    Assert-DbAgentDirectory $Path
}

function New-DbAgentFile {
    [Diagnostics.CodeAnalysis.SuppressMessageAttribute('PSUseShouldProcessForStateChangingFunctions', '', Justification = 'Private file creation is an internal step of the explicitly invoked installer.')]
    [CmdletBinding()]
    param([string]$Path)
    $security = [Security.AccessControl.FileSecurity]::new()
    $security.SetSecurityDescriptorSddlForm('O:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)')
    if ($PSVersionTable.PSEdition -eq 'Desktop') {
        return [IO.FileStream]::new($Path, [IO.FileMode]::CreateNew, [Security.AccessControl.FileSystemRights]::FullControl,
            [IO.FileShare]::None, 65536, [IO.FileOptions]::WriteThrough, $security)
    }
    return [IO.FileSystemAclExtensions]::Create([IO.FileInfo]::new($Path), [IO.FileMode]::CreateNew,
        [Security.AccessControl.FileSystemRights]::FullControl, [IO.FileShare]::None, 65536, [IO.FileOptions]::WriteThrough, $security)
}

function New-DbAgentRequest {
    [Diagnostics.CodeAnalysis.SuppressMessageAttribute('PSUseShouldProcessForStateChangingFunctions', '', Justification = 'Constructs a request object without sending it or changing system state.')]
    [CmdletBinding()]
    param([uri]$Uri)
    $request = [Net.HttpWebRequest]::Create($Uri)
    $request.AllowAutoRedirect = $false
    $request.UseDefaultCredentials = $false
    $request.Timeout = 30000
    $request.ReadWriteTimeout = 30000
    $request.UserAgent = 'FiveManage-db-agent-installer'
    $request.Headers['Accept-Encoding'] = 'identity'
    return $request
}

function Receive-DbAgentFile {
    param([uri]$Uri, [string]$Path, [long]$Limit = 268435456)
    $response = $null
    $inputStream = $null
    $outputStream = $null
    $priorProtocol = [Net.ServicePointManager]::SecurityProtocol
    $timer = [Diagnostics.Stopwatch]::StartNew()
    try {
        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
        for ($redirect = 0; $redirect -le 5; $redirect++) {
            Assert-DbAgentUri $Uri -AllowQuery
            try {
                $request = New-DbAgentRequest $Uri
                $response = $request.GetResponse()
            } catch {
                throw 'Release download failed. Check HTTPS certificates, proxy/network access, and GitHub release availability.'
            }
            $status = [int]$response.StatusCode
            if ($status -ge 300 -and $status -lt 400 -and $response.Headers['Location']) {
                $Uri = [uri]::new($Uri, $response.Headers['Location'])
                $response.Close()
                $response = $null
                continue
            }
            if ($status -ne 200) { throw 'The release server returned an unexpected HTTP status.' }
            break
        }
        if ($null -eq $response) { throw 'The release server returned too many redirects.' }
        if ($response.ContentEncoding -and $response.ContentEncoding -ne 'identity') {
            throw 'The release server returned an unsupported content encoding.'
        }
        if ($response.ContentLength -gt $Limit) { throw 'The release exceeds the installer download size limit.' }
        $inputStream = $response.GetResponseStream()
        $outputStream = New-DbAgentFile $Path
        $buffer = [byte[]]::new(65536)
        [long]$count = 0
        while ($true) {
            try { $length = $inputStream.Read($buffer, 0, $buffer.Length) } catch {
                throw 'Release transfer failed; check the network/proxy and retry. Nothing was installed.'
            }
            if ($length -eq 0) { break }
            $count += $length
            if ($count -gt $Limit) { throw 'The release exceeds the installer download size limit.' }
            if ($timer.Elapsed.TotalMinutes -gt 5) { throw 'The release download timed out; check the network/proxy and retry.' }
            $outputStream.Write($buffer, 0, $length)
        }
        if ($response.ContentLength -ge 0 -and $count -ne $response.ContentLength) {
            throw 'The release download was incomplete. Nothing was installed.'
        }
        $outputStream.Flush($true)
    } finally {
        if ($null -ne $outputStream) { $outputStream.Dispose() }
        if ($null -ne $inputStream) { $inputStream.Dispose() }
        if ($null -ne $response) { $response.Close() }
        [Net.ServicePointManager]::SecurityProtocol = $priorProtocol
        $timer.Stop()
    }
}

function Invoke-DbAgentSetup {
    param([string]$Path, [switch]$ServiceOnly, [string]$Config)
    $arguments = @('install')
    if ($ServiceOnly) { $arguments += '--service-only' }
    if ($Config) { $arguments += @('--config', $Config) }
    & $Path @arguments
    if ($LASTEXITCODE -ne 0) {
        throw "Setup did not finish (exit code $LASTEXITCODE). The verified binary and saved config/key remain. Resume with db-agent.exe install."
    }
}

function Invoke-DbAgentInstall {
    [CmdletBinding()]
    param([switch]$ServiceOnly, [string]$Config)
    Set-StrictMode -Version 2.0
    $ErrorActionPreference = 'Stop'
    Assert-DbAgentWindows
    $release = Get-DbAgentRelease
    if (-not $ServiceOnly -and ([Console]::IsInputRedirected -or [Console]::IsOutputRedirected -or [Console]::IsErrorRedirected)) {
        throw 'Run interactive setup in a PowerShell terminal with redirection and recording disabled.'
    }
    $programFiles = Get-DbAgentProgramDirectory
    if (-not $programFiles -or $programFiles.StartsWith('\\')) { throw 'A local Program Files directory is required.' }
    $parent = [IO.DirectoryInfo]::new($programFiles).Parent
    while ($null -ne $parent) { Assert-DbAgentDirectory $parent.FullName -Ancestor; $parent = $parent.Parent }
    Assert-DbAgentDirectory $programFiles
    $installDirectory = Join-Path $programFiles 'db-agent'
    $executable = Join-Path $installDirectory 'db-agent.exe'
    if (Test-Path -LiteralPath $executable) {
        throw "An installation already exists at $executable. Resume its setup or use its signed update command; nothing was overwritten."
    }
    if (Test-Path -LiteralPath $installDirectory) { Assert-DbAgentDirectory $installDirectory }
    else { New-DbAgentDirectory $installDirectory }
    $work = Join-Path $installDirectory ('.install-' + [guid]::NewGuid().ToString('N'))
    $artifact = Join-Path $work 'db-agent.exe'
    New-DbAgentDirectory $work
    try {
        Receive-DbAgentFile $release.Uri $artifact
        $actualHash = (Get-FileHash -LiteralPath $artifact -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($actualHash -cne $release.SHA256) {
            throw 'Release checksum mismatch. No downloaded code was executed or installed.'
        }
        # File.Move does not overwrite an existing destination, including a
        # concurrent install. The protected staging and final paths share a volume.
        [IO.File]::Move($artifact, $executable)
    } finally {
        if ([IO.File]::Exists($artifact)) { [IO.File]::Delete($artifact) }
        [IO.Directory]::Delete($work, $false)
    }
    Write-Information "Verified db-agent $($release.Version) installed at $executable. Starting setup; service startup is not proof of a completed backup." -InformationAction Continue
    Invoke-DbAgentSetup $executable -ServiceOnly:$ServiceOnly -Config $Config
}

if ($MyInvocation.InvocationName -ne '.') {
    Invoke-DbAgentInstall -ServiceOnly:$ServiceOnly -Config $Config
}
