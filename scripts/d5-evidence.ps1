Set-StrictMode -Version Latest

function Get-D5SHA256([byte[]]$Bytes) {
    return [Convert]::ToHexString(
        [Security.Cryptography.SHA256]::HashData($Bytes)
    ).ToLowerInvariant()
}

function Get-D5TextSHA256([string]$Text) {
    return Get-D5SHA256 ([Text.Encoding]::UTF8.GetBytes($Text))
}

function Invoke-D5Git(
    [string]$RepositoryRoot,
    [string[]]$Arguments
) {
    $output = @(& git -C $RepositoryRoot @Arguments 2>&1)
    if ($LASTEXITCODE -ne 0) {
        throw "git $($Arguments -join ' ') failed: $($output -join [Environment]::NewLine)"
    }
    return $output
}

function Get-D5SourceIdentity([string]$RepositoryRoot) {
    $root = [IO.Path]::GetFullPath($RepositoryRoot)
    $commit = [string](Invoke-D5Git $root @('rev-parse', 'HEAD'))
    $status = @(
        Invoke-D5Git $root @(
            '-c',
            'core.quotepath=false',
            'status',
            '--porcelain=v1',
            '--untracked-files=all'
        )
    )
    $paths = @(
        Invoke-D5Git $root @(
            '-c',
            'core.quotepath=false',
            'ls-files',
            '--cached',
            '--others',
            '--exclude-standard'
        ) | Sort-Object -Unique
    )
    $files = @(
        foreach ($path in $paths) {
            $fullPath = Join-Path $root $path
            if (Test-Path -LiteralPath $fullPath -PathType Leaf) {
                $item = Get-Item -LiteralPath $fullPath
                [ordered]@{
                    Path = $path.Replace('\', '/')
                    Bytes = [long]$item.Length
                    SHA256 = (Get-FileHash -LiteralPath $fullPath -Algorithm SHA256).Hash.ToLowerInvariant()
                    Present = $true
                }
            } else {
                [ordered]@{
                    Path = $path.Replace('\', '/')
                    Bytes = $null
                    SHA256 = $null
                    Present = $false
                }
            }
        }
    )
    $digestInput = @(
        foreach ($file in $files) {
            '{0}`0{1}`0{2}`0{3}' -f
                $file.Path,
                $file.Present,
                $file.Bytes,
                $file.SHA256
        }
    ) -join "`n"
    $clean = $status.Count -eq 0
    return [ordered]@{
        IdentityKind = if ($clean) { 'git-commit' } else { 'workspace-manifest' }
        Commit = $commit.Trim()
        CommitStatus = if ($clean) { 'committed-clean' } else { 'commit-pending-dirty-workspace' }
        WorktreeClean = $clean
        SourceDigest = Get-D5TextSHA256 $digestInput
        GitStatus = @($status)
        Files = @($files)
    }
}

function Test-D5SourceIdentityEqual([object]$Expected, [object]$Actual) {
    return [string]$Expected.IdentityKind -eq [string]$Actual.IdentityKind -and
        [string]$Expected.Commit -eq [string]$Actual.Commit -and
        [bool]$Expected.WorktreeClean -eq [bool]$Actual.WorktreeClean -and
        [string]$Expected.SourceDigest -eq [string]$Actual.SourceDigest
}

function Get-D5SourceIdentitySummary([object]$Source) {
    return [ordered]@{
        IdentityKind = [string]$Source.IdentityKind
        Commit = [string]$Source.Commit
        CommitStatus = [string]$Source.CommitStatus
        WorktreeClean = [bool]$Source.WorktreeClean
        SourceDigest = [string]$Source.SourceDigest
    }
}

function Add-D5SourceCheckpoint(
    [object]$Run,
    [string]$Name,
    [switch]$NoThrow
) {
    $source = Get-D5SourceIdentity $Run.RepositoryRoot
    $unchanged = Test-D5SourceIdentityEqual $Run.SourceAtStart $source
    $Run.SourceCheckpoints.Add([pscustomobject][ordered]@{
        Name = $Name
        UnchangedFromStart = $unchanged
        Source = Get-D5SourceIdentitySummary $source
    })
    if (-not $unchanged) {
        $Run.SourceStable = $false
        $message = "Workspace source changed at checkpoint '$Name': start $($Run.SourceAtStart.SourceDigest), observed $($source.SourceDigest)"
        $Run.SourceFailures.Add($message)
        if (-not $NoThrow) {
            throw $message
        }
    }
    return $source
}

function Get-D5CommandVersion([string]$Name, [string[]]$Arguments) {
    if ($null -eq (Get-Command $Name -ErrorAction SilentlyContinue)) {
        return $null
    }
    try {
        return ((@(& $Name @Arguments 2>&1) | ForEach-Object { [string]$_ }) -join "`n").Trim()
    } catch {
        return $null
    }
}

function Get-D5EvidenceEnvironment([string]$RepositoryRoot) {
    $cpu = $null
    $physicalMemoryBytes = $null
    if ($IsWindows) {
        # Hardware labels are descriptive only. Environment-provided metadata
        # avoids turning an unhealthy Windows management provider into a test
        # prerequisite; exact semantic/resource measurements remain in-run.
        $cpu = [string]$env:PROCESSOR_IDENTIFIER
    } elseif ($null -ne (Get-Command 'lscpu' -ErrorAction SilentlyContinue)) {
        $modelLine = @(& lscpu 2>$null | Where-Object { $_ -match '^Model name\s*:' } | Select-Object -First 1)
        if ($modelLine.Count -eq 1) {
            $cpu = ([string]$modelLine[0] -replace '^Model name\s*:\s*', '').Trim()
        }
    }
    return [ordered]@{
        OSDescription = [Runtime.InteropServices.RuntimeInformation]::OSDescription
        OSArchitecture = [Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
        ProcessArchitecture = [Runtime.InteropServices.RuntimeInformation]::ProcessArchitecture.ToString()
        Processor = $cpu
        LogicalProcessorCount = [Environment]::ProcessorCount
        PhysicalMemoryBytes = $physicalMemoryBytes
        PowerShell = $PSVersionTable.PSVersion.ToString()
        Go = Get-D5CommandVersion 'go' @('version')
        Node = Get-D5CommandVersion 'node' @('--version')
        Pnpm = Get-D5CommandVersion 'pnpm' @('--version')
        Playwright = Get-D5CommandVersion 'pnpm' @('-C', (Join-Path $RepositoryRoot 'web'), 'exec', 'playwright', '--version')
    }
}

function New-D5EvidenceRun(
    [string]$RepositoryRoot,
    [string]$EvidenceRoot,
    [string]$Mode,
    [string]$Command,
    [datetimeoffset]$StartedAt = [datetimeoffset]::Now
) {
    $root = [IO.Path]::GetFullPath($EvidenceRoot)
    New-Item -ItemType Directory -Force -Path $root | Out-Null
    $stage = Join-Path $root ('.staging-' + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $stage | Out-Null
    $source = Get-D5SourceIdentity $RepositoryRoot
    return [pscustomobject]@{
        RepositoryRoot = [IO.Path]::GetFullPath($RepositoryRoot)
        EvidenceRoot = $root
        StagePath = $stage
        Mode = $Mode
        Command = $Command
        StartedAt = $StartedAt
        SourceAtStart = $source
        SourceCheckpoints = [Collections.Generic.List[object]]::new()
        SourceFailures = [Collections.Generic.List[string]]::new()
        SourceStable = $true
        CompletionStatus = $null
        CompletionError = $null
    }
}

function Test-D5EvidenceDirectory(
    [string]$Path,
    [string]$ExpectedEvidenceID = ''
) {
    $root = [IO.Path]::GetFullPath($Path)
    $manifestPath = Join-Path $root 'manifest.json'
    $payloadPath = Join-Path $root 'payload.json'
    if (-not (Test-Path -LiteralPath $manifestPath -PathType Leaf)) {
        throw "Evidence manifest is missing: $manifestPath"
    }
    if (-not (Test-Path -LiteralPath $payloadPath -PathType Leaf)) {
        throw "Evidence canonical payload is missing: $payloadPath"
    }
    $manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json
    $payloadJSON = Get-Content -LiteralPath $payloadPath -Raw
    $payload = $payloadJSON | ConvertFrom-Json
    $computedID = Get-D5TextSHA256 $payloadJSON
    $directoryID = Split-Path -Leaf $root
    $requiredID = if ([string]::IsNullOrWhiteSpace($ExpectedEvidenceID)) {
        $directoryID
    } else {
        $ExpectedEvidenceID.ToLowerInvariant()
    }
    if ([string]$manifest.EvidenceID -ne $computedID -or
        [string]$manifest.PayloadSHA256 -ne $computedID -or
        [string]$manifest.PayloadFile -ne 'payload.json' -or
        $requiredID -ne $computedID) {
        throw "Evidence content address is invalid: recorded $($manifest.EvidenceID), computed $computedID, required $requiredID"
    }
    $expected = @{}
    foreach ($artifact in @($payload.Artifacts)) {
        $relative = [string]$artifact.Path
        if ($expected.ContainsKey($relative)) {
            throw "Evidence manifest repeats artifact $relative"
        }
        $expected[$relative] = $artifact
    }
    $actual = @(
        Get-ChildItem -LiteralPath $root -File -Recurse |
            Where-Object { $_.FullName -ne $manifestPath -and $_.FullName -ne $payloadPath } |
            ForEach-Object { [IO.Path]::GetRelativePath($root, $_.FullName).Replace('\', '/') } |
            Sort-Object
    )
    if ($actual.Count -ne $expected.Count -or
        @(Compare-Object -ReferenceObject @($expected.Keys | Sort-Object) -DifferenceObject $actual).Count -ne 0) {
        throw 'Evidence artifact file set differs from its manifest'
    }
    foreach ($relative in $actual) {
        $file = Get-Item -LiteralPath (Join-Path $root $relative)
        $record = $expected[$relative]
        $hash = (Get-FileHash -LiteralPath $file.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
        if ([long]$record.Bytes -ne [long]$file.Length -or [string]$record.SHA256 -ne $hash) {
            throw "Evidence artifact differs from its manifest: $relative"
        }
    }
    return $true
}

function Get-D5EvidencePublicationOperation(
    [object]$Operations,
    [string]$Name,
    [scriptblock]$Default
) {
    if ($null -eq $Operations) {
        return $Default
    }
    $property = $Operations.PSObject.Properties[$Name]
    if ($null -eq $property -or $null -eq $property.Value) {
        return $Default
    }
    if ($property.Value -isnot [scriptblock]) {
        throw "Evidence publication operation $Name is not a script block"
    }
    return [scriptblock]$property.Value
}

function Seal-D5EvidenceStaging([string]$Path) {
    $files = @(Get-ChildItem -LiteralPath $Path -File -Recurse)
    if ($files.Count -lt 2) {
        throw 'Evidence staging is incomplete before read-only sealing'
    }
    foreach ($file in $files) {
        $file.IsReadOnly = $true
    }
    foreach ($file in Get-ChildItem -LiteralPath $Path -File -Recurse) {
        if (-not $file.IsReadOnly) {
            throw "Evidence staging file was not sealed read-only: $($file.FullName)"
        }
    }
}

function Complete-D5EvidenceRun(
    [object]$Run,
    [ValidateSet('Success', 'Failed')]
    [string]$Status,
    [string]$ErrorMessage = '',
    [datetimeoffset]$CompletedAt = [datetimeoffset]::Now,
    [object]$PublicationOperations = $null
) {
    $payloadPath = Join-Path $Run.StagePath 'payload.json'
    $manifestPath = Join-Path $Run.StagePath 'manifest.json'
    if ((Test-Path -LiteralPath $payloadPath) -or (Test-Path -LiteralPath $manifestPath)) {
        throw 'Evidence staging already contains publication metadata'
    }
    $captureEnvironment = Get-D5EvidencePublicationOperation $PublicationOperations 'Environment' {
        param([object]$EvidenceRun)
        Get-D5EvidenceEnvironment $EvidenceRun.RepositoryRoot
    }
    $captureSource = Get-D5EvidencePublicationOperation $PublicationOperations 'SourceCheckpoint' {
        param([object]$EvidenceRun)
        Add-D5SourceCheckpoint $EvidenceRun 'completion-after-artifact-capture' -NoThrow
    }
    $environment = & $captureEnvironment $Run
    $artifacts = @(
        Get-ChildItem -LiteralPath $Run.StagePath -File -Recurse |
            Sort-Object FullName |
            ForEach-Object {
                [ordered]@{
                    Path = [IO.Path]::GetRelativePath($Run.StagePath, $_.FullName).Replace('\', '/')
                    Bytes = [long]$_.Length
                    SHA256 = (Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
                }
            }
    )
    $sourceAtEnd = & $captureSource $Run
    $sourceUnchanged = [bool]$Run.SourceStable -and
        (Test-D5SourceIdentityEqual $Run.SourceAtStart $sourceAtEnd)
    $effectiveStatus = $Status
    $errors = [Collections.Generic.List[string]]::new()
    if (-not [string]::IsNullOrWhiteSpace($ErrorMessage)) {
        $errors.Add($ErrorMessage)
    }
    foreach ($sourceFailure in $Run.SourceFailures) {
        if (-not $errors.Contains($sourceFailure)) {
            $errors.Add($sourceFailure)
        }
    }
    if (-not $sourceUnchanged) {
        $effectiveStatus = 'Failed'
    }
    $effectiveError = if ($errors.Count -eq 0) { $null } else { $errors -join '; ' }
    $payload = [ordered]@{
        SchemaVersion = 2
        Status = $effectiveStatus
        Error = $effectiveError
        Mode = $Run.Mode
        Command = $Run.Command
        StartedAt = $Run.StartedAt.ToUniversalTime().ToString('o')
        CompletedAt = $CompletedAt.ToUniversalTime().ToString('o')
        Source = [ordered]@{
            UnchangedForWholeRun = $sourceUnchanged
            Start = $Run.SourceAtStart
            End = $sourceAtEnd
            Checkpoints = @($Run.SourceCheckpoints)
        }
        Environment = $environment
        Artifacts = @($artifacts)
    }
    $payloadJSON = $payload | ConvertTo-Json -Depth 24 -Compress
    $evidenceID = Get-D5TextSHA256 $payloadJSON
    $destination = Join-Path $Run.EvidenceRoot $evidenceID
    if (Test-Path -LiteralPath $destination) {
        throw "Evidence $evidenceID already exists; refusing to overwrite it"
    }
    $manifest = [ordered]@{
        EvidenceID = $evidenceID
        PayloadSHA256 = $evidenceID
        PayloadFile = 'payload.json'
    }
    [IO.File]::WriteAllText(
        $payloadPath,
        $payloadJSON,
        [Text.UTF8Encoding]::new($false)
    )
    [IO.File]::WriteAllText(
        $manifestPath,
        ($manifest | ConvertTo-Json -Depth 24),
        [Text.UTF8Encoding]::new($false)
    )
    $verify = Get-D5EvidencePublicationOperation $PublicationOperations 'Verify' {
        param([string]$Path, [string]$EvidenceID)
        Test-D5EvidenceDirectory $Path $EvidenceID
    }
    $seal = Get-D5EvidencePublicationOperation $PublicationOperations 'Seal' {
        param([string]$Path)
        Seal-D5EvidenceStaging $Path
    }
    $move = Get-D5EvidencePublicationOperation $PublicationOperations 'Move' {
        param([string]$Source, [string]$Destination)
        [IO.Directory]::Move($Source, $Destination)
    }

    # Validation and sealing happen while the directory is hidden under its
    # staging name. The only operation that makes Success discoverable is the
    # final same-volume atomic directory rename.
    [void](& $verify $Run.StagePath $evidenceID)
    [void](& $seal $Run.StagePath)
    [void](& $verify $Run.StagePath $evidenceID)
    $Run.CompletionStatus = $effectiveStatus
    $Run.CompletionError = $effectiveError
    $result = [pscustomobject]@{
        Path = $destination
        EvidenceID = $evidenceID
        Status = $effectiveStatus
        Error = $effectiveError
        SourceUnchanged = $sourceUnchanged
    }
    [void](& $move $Run.StagePath $destination)
    return $result
}

function Complete-D5EvidenceTransaction(
    [AllowNull()] [object]$Run,
    [ValidateSet('Success', 'Failed')]
    [string]$RequestedStatus,
    [string]$ErrorMessage = '',
    [AllowEmptyCollection()] [object[]]$FinalizationSteps = @(),
    [datetimeoffset]$CompletedAt = [datetimeoffset]::MinValue,
    [object]$PublicationOperations = $null
) {
    $errors = [Collections.Generic.List[string]]::new()
    if (-not [string]::IsNullOrWhiteSpace($ErrorMessage)) {
        $errors.Add($ErrorMessage)
    }
    foreach ($step in $FinalizationSteps) {
        $name = [string]$step.Name
        $action = $step.Action
        if ([string]::IsNullOrWhiteSpace($name) -or $action -isnot [scriptblock]) {
            $errors.Add('evidence finalization step is malformed')
            continue
        }
        try {
            [void](& $action)
        } catch {
            $errors.Add("$name failed: $_")
        }
    }
    $effectiveStatus = if ($RequestedStatus -eq 'Success' -and $errors.Count -eq 0) {
        'Success'
    } else {
        'Failed'
    }
    $effectiveError = if ($errors.Count -eq 0) { '' } else { $errors -join '; ' }
    # A default timestamp belongs to the completed transaction, not merely to
    # function entry. Explicit timestamps remain injectable for deterministic evidence tests.
    $publicationCompletedAt = if ($PSBoundParameters.ContainsKey('CompletedAt')) {
        [datetimeoffset]$CompletedAt
    } else {
        [datetimeoffset]::Now
    }
    if ($null -eq $Run) {
        return [pscustomobject][ordered]@{
            Result = $null
            Status = $effectiveStatus
            Error = $effectiveError
            FinalizationFailures = @($errors)
        }
    }
    $result = Complete-D5EvidenceRun `
        $Run `
        $effectiveStatus `
        $effectiveError `
        $publicationCompletedAt `
        $PublicationOperations
    return [pscustomobject][ordered]@{
        Result = $result
        Status = $result.Status
        Error = $result.Error
        FinalizationFailures = @($errors)
    }
}
