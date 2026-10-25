param([Parameter(Mandatory = $true)][string]$InstallerPath)

Set-StrictMode -Version 2.0
$ErrorActionPreference = 'Stop'

# Isolate the type-resolution collision from Pester and other tests.
$shadow = Add-Type -TypeDefinition @'
namespace InstallerArchitectureFixture {
    public static class RuntimeInformation {
        public static string OSDescription { get { return "shadow runtime"; } }
    }
}
'@ -PassThru
$accelerators = [psobject].Assembly.GetType('System.Management.Automation.TypeAccelerators')
$accelerators::Add('System.Runtime.InteropServices.RuntimeInformation', $shadow)
$unqualified = 'System.Runtime.InteropServices.RuntimeInformation' -as [type]
if ($null -ne $unqualified.GetProperty('OSArchitecture')) {
    throw 'The fixture did not shadow the unqualified runtime type.'
}

. $InstallerPath
$expected = [System.Runtime.InteropServices.RuntimeInformation, mscorlib]::OSArchitecture.ToString()
$actual = Get-DbAgentNativeArchitecture
if ($actual -cne $expected) {
    throw "Expected native architecture $expected, got $actual."
}
Write-Output 'Architecture collision regression passed.'
