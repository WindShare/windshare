function Get-WindShareGoPackageSet {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]
        [ValidateSet('all', 'core', 'non-core')]
        [string]$Set
    )

    $global:LASTEXITCODE = 0
    $packages = @(go run ./scripts/ci/_gopackages "-set=$Set")
    if ($LASTEXITCODE -ne 0) {
        throw "Loading the $Set package set exited with code $LASTEXITCODE"
    }
    if ($packages.Count -eq 0) {
        throw "The $Set package set is empty"
    }
    return $packages
}
