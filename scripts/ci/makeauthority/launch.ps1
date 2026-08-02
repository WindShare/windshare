[CmdletBinding()]
param(
    [Parameter(Mandatory, Position = 0)][string]$CanonicalMakefile,
    [Parameter(Mandatory, Position = 1)][string]$ExpectedMakefileSHA256,
    [Parameter(Mandatory, Position = 2)][string]$RepositoryRoot,
    [Parameter(ValueFromRemainingArguments)][string[]]$MakeArguments
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

Import-Module (Join-Path $PSScriptRoot 'authority.psm1') -Force
Import-Module (Join-Path $PSScriptRoot 'protected-path-authority.psm1') -Force
$null = Enter-WindShareMakeAuthority
try {
    $makefileAuthority = Enter-WindShareMakefileAuthority `
        -CanonicalMakefile $CanonicalMakefile `
        -ExpectedSHA256 $ExpectedMakefileSHA256
    $null = Enter-WindShareGitAuthority
    $recipeShellAuthority = Enter-WindShareRecipeShellAuthority
    $pwshAuthority = Enter-WindSharePwshAuthority
    $checkoutCommit = Get-WindShareGitHeadCommit -RepositoryRoot $RepositoryRoot
    $completion = $null
    foreach ($argument in $MakeArguments) {
        if ($argument.StartsWith('BROWSER_NETWORK_COMPLETION=', [StringComparison]::Ordinal)) {
            if ($null -ne $completion) { throw 'duplicate browser-network completion authority' }
            $completion = $argument.Substring('BROWSER_NETWORK_COMPLETION='.Length)
        }
    }
    if ($null -ne $completion) {
        $null = Enter-WindShareProtectedPathAuthority `
            -Completion $completion
    }
    $invocationArguments = @(
        'WINDSHARE_HOST_GOOS=windows'
        "WINDSHARE_CORE_ARTIFACT_COMMIT_SHA=$checkoutCommit"
        "WINDSHARE_RETAINED_MAKEFILE=$($makefileAuthority.Path)"
        "WINDSHARE_RECIPE_SHELL=$($recipeShellAuthority.Executable)"
        "WINDSHARE_PWSH_EXECUTABLE=$($pwshAuthority.Executable)"
    ) + $MakeArguments
    Invoke-WindShareMake -MakeArguments $invocationArguments
    $exitCode = $LASTEXITCODE
} finally {
    Exit-WindShareProtectedPathAuthority
    Exit-WindShareMakeAuthority
}
exit $exitCode
