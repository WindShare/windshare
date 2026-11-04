Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [Text.UTF8Encoding]::new($false)
$pipe = $null
$process = $null
$connection = $null
$writerTimeoutMilliseconds = 30000
$resultTimeoutMilliseconds = 5000
$pipeBufferBytes = 4096
try {
    $pipeName = 'windshare-network-' + [Guid]::NewGuid().ToString('N')
    $security = [IO.Pipes.PipeSecurity]::new()
    $security.SetAccessRuleProtection($true, $false)
    $user = [Security.Principal.WindowsIdentity]::GetCurrent()
    try { $userSid = $user.User } finally { $user.Dispose() }
    # A standard user may authorize with a different administrator account.
    $administratorSid = [Security.Principal.SecurityIdentifier]::new([Security.Principal.WellKnownSidType]::BuiltinAdministratorsSid, $null)
    foreach ($sid in @($userSid, $administratorSid)) {
        $security.AddAccessRule([IO.Pipes.PipeAccessRule]::new($sid, [IO.Pipes.PipeAccessRights]::ReadWrite, [Security.AccessControl.AccessControlType]::Allow))
    }
    $pipe = [IO.Pipes.NamedPipeServerStream]::new($pipeName, [IO.Pipes.PipeDirection]::In, 1,
        [IO.Pipes.PipeTransmissionMode]::Byte, [IO.Pipes.PipeOptions]::Asynchronous, $pipeBufferBytes, $pipeBufferBytes, $security)
    $connection = $pipe.BeginWaitForConnection($null, $null)
    $childScript = '$ResultPipe = ''' + $pipeName + "'`n" + $RulesScript
    $encoded = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($childScript))
    $process = Start-Process -FilePath (Join-Path $PSHOME 'powershell.exe') -Verb RunAs -WindowStyle Hidden -PassThru -ArgumentList @(
        '-NoLogo', '-NoProfile', '-NonInteractive', '-EncodedCommand', $encoded)
    if (-not $connection.AsyncWaitHandle.WaitOne($writerTimeoutMilliseconds)) { throw 'Timed out waiting for the firewall rule writer.' }
    $pipe.EndWaitForConnection($connection)
    $reader = [IO.StreamReader]::new($pipe, [Text.UTF8Encoding]::new($false))
    try {
        $read = $reader.ReadLineAsync()
        if (-not $read.Wait($resultTimeoutMilliseconds)) { throw 'Timed out reading the firewall rule result.' }
        $result = $read.Result | ConvertFrom-Json
        if (-not $process.WaitForExit($resultTimeoutMilliseconds)) { throw 'The firewall rule writer did not exit.' }
        if ($null -eq $result -or $result.Code -ne $process.ExitCode) { throw 'The firewall rule writer returned an inconsistent result.' }
        $result | ConvertTo-Json -Compress
    } finally { $reader.Dispose() }
} catch {
    $code = 1
    $exception = $_.Exception
    while ($null -ne $exception) {
        if ($exception -is [ComponentModel.Win32Exception]) { $code = $exception.NativeErrorCode; break }
        $exception = $exception.InnerException
    }
    @{ Code = $code; Detail = $_.Exception.Message } | ConvertTo-Json -Compress
} finally {
    if ($null -ne $process) { $process.Dispose() }
    if ($null -ne $pipe) { $pipe.Dispose() }
    if ($null -ne $connection) { $connection.AsyncWaitHandle.Dispose() }
}
