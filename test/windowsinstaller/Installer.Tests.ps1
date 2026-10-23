BeforeAll {
    . "$PSScriptRoot/../../scripts/install-windows.ps1"
    $script:OnWindows = [Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT
}

Describe 'Windows installer trust and publication' {
    BeforeEach {
        $script:FixtureBytes = [Text.Encoding]::UTF8.GetBytes('approved release fixture')
        $fixture = Join-Path $TestDrive ('approved-' + [guid]::NewGuid().ToString('N') + '.bin')
        [IO.File]::WriteAllBytes($fixture, $script:FixtureBytes)
        $script:FixtureHash = (Get-FileHash -LiteralPath $fixture -Algorithm SHA256).Hash.ToLowerInvariant()
        $script:DbAgentReleaseVersion = '1.2.3'
        $script:DbAgentReleaseUrl = 'https://releases.example.com/1.2.3/db-agent.exe'
        $script:DbAgentReleaseSHA256 = $script:FixtureHash
        $script:ProgramRoot = Join-Path $TestDrive ([guid]::NewGuid().ToString('N') + '/Program Files')
        [void][IO.Directory]::CreateDirectory($script:ProgramRoot)
        Mock Assert-DbAgentWindows {}
        Mock Get-DbAgentProgramDirectory { return $script:ProgramRoot }
        Mock Assert-DbAgentDirectory {}
        Mock New-DbAgentDirectory { param($Path); [void][IO.Directory]::CreateDirectory($Path) }
        Mock Receive-DbAgentFile { param($Uri, $Path); [IO.File]::WriteAllBytes($Path, $script:FixtureBytes) }
        Mock Invoke-DbAgentSetup {}
    }

    It 'installs only the approved bytes and launches the existing setup with literal arguments' {
        Invoke-DbAgentInstall -ServiceOnly -Config 'C:\private data\config.yaml'
        $installed = Join-Path $script:ProgramRoot 'db-agent/db-agent.exe'
        (Get-FileHash -LiteralPath $installed -Algorithm SHA256).Hash.ToLowerInvariant() | Should -BeExactly $script:FixtureHash
        Should -Invoke Invoke-DbAgentSetup -Times 1 -Exactly -ParameterFilter {
            $Path -eq $installed -and $ServiceOnly -and $Config -eq 'C:\private data\config.yaml'
        }
        @(Get-ChildItem (Split-Path $installed) -Directory -Filter '.install-*').Count | Should -Be 0
    }

    It 'rejects a bad checksum before running ANY downloaded code' {
        $script:DbAgentReleaseSHA256 = '0' * 64
        { Invoke-DbAgentInstall -ServiceOnly } | Should -Throw '*checksum mismatch*'
        Should -Invoke Invoke-DbAgentSetup -Times 0
        Test-Path (Join-Path $script:ProgramRoot 'db-agent/db-agent.exe') | Should -BeFalse
    }

    It 'never overwrites an existing installation or runs it from the bootstrap' {
        $directory = Join-Path $script:ProgramRoot 'db-agent'
        [void][IO.Directory]::CreateDirectory($directory)
        $path = Join-Path $directory 'db-agent.exe'
        [IO.File]::WriteAllText($path, 'existing binary')
        { Invoke-DbAgentInstall -ServiceOnly } | Should -Throw '*already exists*'
        [IO.File]::ReadAllText($path) | Should -BeExactly 'existing binary'
        Should -Invoke Receive-DbAgentFile -Times 0
    }

    It 'retains the verified binary when setup fails and preserves the error' {
        Mock Invoke-DbAgentSetup { throw 'setup fixture failed (exit code 9)' }
        { Invoke-DbAgentInstall -ServiceOnly } | Should -Throw '*exit code 9*'
        (Get-FileHash -LiteralPath (Join-Path $script:ProgramRoot 'db-agent/db-agent.exe') -Algorithm SHA256).Hash.ToLowerInvariant() | Should -BeExactly $script:FixtureHash
    }

    It 'fails before network or file writes without elevation' {
        Mock Assert-DbAgentWindows { throw 'Run as administrator' }
        { Invoke-DbAgentInstall -ServiceOnly } | Should -Throw '*administrator*'
        Should -Invoke Receive-DbAgentFile -Times 0
        Should -Invoke New-DbAgentDirectory -Times 0
    }

    It 'fails before downloads on unsupported architectures' {
        Mock Assert-DbAgentWindows { throw 'Unsupported Windows architecture' }
        { Invoke-DbAgentInstall -ServiceOnly } | Should -Throw '*Unsupported Windows architecture*'
        Should -Invoke Receive-DbAgentFile -Times 0
    }

    It 'requires a provisioned published installer, not customer-provided environment values' {
        $script:DbAgentReleaseSHA256 = ''
        { Invoke-DbAgentInstall -ServiceOnly } | Should -Throw '*not been provisioned*'
        Should -Invoke Receive-DbAgentFile -Times 0
    }

    It 'rejects insecure initial URLs' {
        $script:DbAgentReleaseUrl = 'http://releases.example.com/agent.exe'
        { Invoke-DbAgentInstall -ServiceOnly } | Should -Throw '*HTTPS*'
        Should -Invoke Receive-DbAgentFile -Times 0
    }

    It 'rejects unsafe existing installation directories without modifying them' {
        Mock Assert-DbAgentDirectory { throw 'unsafe owner' }
        { Invoke-DbAgentInstall -ServiceOnly } | Should -Throw '*unsafe owner*'
        Should -Invoke New-DbAgentDirectory -Times 0
        Should -Invoke Receive-DbAgentFile -Times 0
    }
}

Describe 'Bounded HTTPS transfer' {
    BeforeEach {
        $script:DownloadPath = Join-Path $TestDrive ([guid]::NewGuid().ToString('N') + '.download')
        $script:ResponseBody = [byte[]](1, 2, 3, 4)
        $script:ResponseLength = 4L
        $script:ResponseStatus = 200
        $script:ResponseLocation = ''
        Mock New-DbAgentFile { param($Path); return [IO.File]::Open($Path, [IO.FileMode]::CreateNew, [IO.FileAccess]::ReadWrite) }
        Mock New-DbAgentRequest {
            $object = [pscustomobject]@{}
            $object | Add-Member ScriptMethod GetResponse {
                $response = [pscustomobject]@{
                    StatusCode = $script:ResponseStatus
                    Headers = @{ Location = $script:ResponseLocation }
                    ContentEncoding = ''
                    ContentLength = $script:ResponseLength
                }
                $response | Add-Member ScriptMethod GetResponseStream { return [IO.MemoryStream]::new($script:ResponseBody, $false) }
                $response | Add-Member ScriptMethod Close {}
                return $response
            }
            return $object
        }
    }

    It 'streams a complete response and restores the host TLS policy' {
        $prior = [Net.ServicePointManager]::SecurityProtocol
        $path = $script:DownloadPath
        Receive-DbAgentFile 'https://example.com/binary' $path -Limit 8
        [IO.File]::ReadAllBytes($path).Length | Should -Be 4
        [Net.ServicePointManager]::SecurityProtocol | Should -Be $prior
    }

    It 'rejects oversized declared downloads without creating output' {
        $script:ResponseLength = 9L
        { Receive-DbAgentFile 'https://example.com/binary' $script:DownloadPath -Limit 8 } | Should -Throw '*size limit*'
        Should -Invoke New-DbAgentFile -Times 0
    }

    It 'bounds downloads even without a Content-Length header' {
        $script:ResponseLength = -1L
        $script:ResponseBody = [byte[]](1, 2, 3, 4, 5)
        { Receive-DbAgentFile 'https://example.com/binary' $script:DownloadPath -Limit 4 } | Should -Throw '*size limit*'
    }

    It 'rejects an incomplete response' {
        $script:ResponseLength = 8L
        { Receive-DbAgentFile 'https://example.com/binary' $script:DownloadPath -Limit 8 } | Should -Throw '*incomplete*'
    }

    It 'refuses HTTP redirects before making the next request' {
        $script:ResponseStatus = 302
        $script:ResponseLocation = 'http://example.com/redirect'
        { Receive-DbAgentFile 'https://example.com/binary' (Join-Path $TestDrive 'download') } | Should -Throw '*HTTPS*'
        Should -Invoke New-DbAgentRequest -Times 1 -Exactly
    }

    It 'bounds redirect loops' {
        $script:ResponseStatus = 302
        $script:ResponseLocation = 'https://example.com/redirect'
        { Receive-DbAgentFile 'https://example.com/binary' (Join-Path $TestDrive 'download') } | Should -Throw '*too many redirects*'
    }

    It 'reports download/proxy failures without echoing raw errors or URLs' {
        Mock New-DbAgentRequest { throw 'proxy-password-secret' }
        { Receive-DbAgentFile 'https://example.com/binary' (Join-Path $TestDrive 'download') } | Should -Throw '*Check HTTPS certificates*'
    }
}

Describe 'Native Windows protected publication' -Skip:([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) {
    It 'rejects a non-amd64 native OS before installation' {
        Mock Get-DbAgentNativeArchitecture { return 'Arm64' }
        { Assert-DbAgentWindows } | Should -Throw '*Unsupported Windows architecture*'
    }

    It 'creates directories and files with Administrators ownership and private DACLs at creation' {
        $directory = Join-Path $TestDrive 'private'
        New-DbAgentDirectory $directory
        $path = Join-Path $directory 'artifact.exe'
        $stream = New-DbAgentFile $path
        $stream.Dispose()
        foreach ($item in @($directory, $path)) {
            $acl = Get-Acl -LiteralPath $item
            $acl.GetOwner([Security.Principal.SecurityIdentifier]).Value | Should -Be 'S-1-5-32-544'
            $acl.AreAccessRulesProtected | Should -BeTrue
            foreach ($rule in $acl.GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier])) {
                $rule.IdentityReference.Value | Should -BeIn @('S-1-5-18', 'S-1-5-32-544')
            }
        }
    }
}
