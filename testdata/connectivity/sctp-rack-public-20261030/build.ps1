param(
    [Parameter(Mandatory)][string]$ForkPath,
    [switch]$Linux
)
$ErrorActionPreference = 'Stop'
Set-Location -LiteralPath $PSScriptRoot
$fork = (Resolve-Path -LiteralPath $ForkPath).Path.Replace('\','/')
Copy-Item -LiteralPath go.mod -Destination repair.mod
Copy-Item -LiteralPath go.sum -Destination repair.sum
go mod edit -modfile repair.mod "-replace=github.com/pion/sctp=$fork"
if ($LASTEXITCODE -ne 0) { throw 'Cannot configure candidate module' }
$savedGOOS = $env:GOOS
try {
    if ($Linux) { $env:GOOS = 'linux' }
    $suffix = if ($Linux) { '-linux' } elseif ($IsWindows) { '.exe' } else { '' }
    go build -trimpath -o ".build/upstream$suffix" .
    if ($LASTEXITCODE -ne 0) { throw 'Upstream build failed' }
    go build -trimpath -modfile repair.mod -ldflags '-X main.variant=repaired' -o ".build/repaired$suffix" .
    if ($LASTEXITCODE -ne 0) { throw 'Candidate build failed' }
} finally {
    $env:GOOS = $savedGOOS
}
