function Get-WindShareInstallRoot([string]$Channel) {
    if ($Channel -notin @('development', 'release')) { throw 'Unknown installation channel' }
    Join-Path $env:LOCALAPPDATA "Programs/WindShare/$Channel"
}

function Wait-WindShareIdle([string]$Root, [int]$TimeoutSeconds) {
    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    $rootPrefix = [IO.Path]::GetFullPath($Root).TrimEnd([IO.Path]::DirectorySeparatorChar) + [IO.Path]::DirectorySeparatorChar
    $announced = $false
    while ($true) {
        $busy = $false
        foreach ($process in [Diagnostics.Process]::GetProcessesByName('windshare')) {
            try { if ($process.MainModule.FileName.StartsWith($rootPrefix, [StringComparison]::OrdinalIgnoreCase)) { $busy = $true } }
            catch { $busy = $true }
            finally { $process.Dispose() }
        }
        if (Test-Path -LiteralPath $Root) {
            foreach ($file in Get-ChildItem -LiteralPath $Root -Filter '*.dll' -Recurse -File) {
                try { $stream = [IO.File]::Open($file.FullName, 'Open', 'ReadWrite', 'None'); $stream.Dispose() }
                catch { $busy = $true }
            }
        }
        if (!$busy) { return }
        if (!$announced) {
            Write-Host 'Waiting for WindShare and the Explorer extension to exit. Active shares continue; close WindShare when finished.'
            $announced = $true
        }
        if ((Get-Date) -ge $deadline) { throw 'Installation deferred. Close WindShare and Explorer windows, then run this command again. The current installation remains usable.' }
        Start-Sleep -Milliseconds 500
    }
}

function Remove-WindShareDirectory([string]$Path, [string]$Root) {
    $resolvedRoot = [IO.Path]::GetFullPath($Root).TrimEnd([IO.Path]::DirectorySeparatorChar)
    $resolvedPath = [IO.Path]::GetFullPath($Path).TrimEnd([IO.Path]::DirectorySeparatorChar)
    if ($resolvedPath -ne $resolvedRoot -and !$resolvedPath.StartsWith($resolvedRoot + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
        throw 'Refusing to remove a path outside the WindShare installation'
    }
    if (Test-Path -LiteralPath $resolvedPath) { Remove-Item -LiteralPath $resolvedPath -Recurse -Force }
}
