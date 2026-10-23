[CmdletBinding()]
param([string]$ModulePath)

$ErrorActionPreference = 'Stop'
if ($ModulePath) { $env:PSModulePath = $ModulePath + [IO.Path]::PathSeparator + $env:PSModulePath }
Import-Module Pester -RequiredVersion 5.7.1 -ErrorAction Stop
Import-Module PSScriptAnalyzer -RequiredVersion 1.24.0 -ErrorAction Stop
$root = Split-Path $PSScriptRoot -Parent
$script = Join-Path $PSScriptRoot 'install-windows.ps1'
$results = @(Invoke-ScriptAnalyzer -Path $script -Severity Error, Warning)
if ($results.Count) {
    $results | Format-Table RuleName, Line, Message -AutoSize
    throw 'Windows installer lint failed.'
}
$result = Invoke-Pester -Path (Join-Path $root 'test/windowsinstaller/Installer.Tests.ps1') -Output Detailed -PassThru
if ($result.FailedCount -gt 0 -or $result.PassedCount -eq 0) { throw 'Windows installer tests failed.' }
