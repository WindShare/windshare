$ErrorActionPreference = 'Stop'
$repo = (Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path
$research = Join-Path $repo '.tmp/webrtc-plan-revision-20261022'
$stage = Join-Path $research ('build-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $stage -Force | Out-Null
Push-Location $repo
try {
    # Keep the experimental dependency edits outside the production checkout.
    $manifest = Get-Content -Raw -LiteralPath (Join-Path $PSScriptRoot 'portable-manifest.json') | ConvertFrom-Json
    foreach ($module in @(@('sctp','github.com/pion/sctp@v1.11.1'), @('dtls','github.com/pion/dtls/v3@v3.1.8'))) {
        $download = go mod download -json $module[1] | ConvertFrom-Json
        if ($LASTEXITCODE -ne 0) { throw 'Dependency download failed' }
        Copy-Item -LiteralPath $download.Dir -Destination (Join-Path $stage $module[0]) -Recurse
    }
    foreach ($source in $manifest.sources) {
        $destination = Join-Path $stage $source.target
        if ($source.base -and $source.target -notmatch '^(sctp|dtls)/') {
            New-Item -ItemType Directory -Path (Split-Path $destination) -Force | Out-Null
            Copy-Item -LiteralPath (Join-Path $repo ('third_party/pion/' + $source.target)) -Destination $destination
        }
        if ($source.base -and (Get-FileHash -LiteralPath $destination -Algorithm SHA256).Hash.ToLowerInvariant() -ne $source.base) {
            throw ('Research source changed: ' + $source.target)
        }
    }
    Get-ChildItem -LiteralPath $stage -File -Recurse | ForEach-Object { $_.IsReadOnly = $false }
    $relativeStage = [IO.Path]::GetRelativePath($repo, $stage).Replace('\','/')
    git apply --check --directory=$relativeStage (Join-Path $PSScriptRoot 'portable.patch')
    if ($LASTEXITCODE -ne 0) { throw 'Prototype patch check failed' }
    git apply --directory=$relativeStage (Join-Path $PSScriptRoot 'portable.patch')
    if ($LASTEXITCODE -ne 0) { throw 'Prototype patch failed' }
    $overlay = @{}
    foreach ($source in $manifest.sources) {
        $destination = Join-Path $stage $source.target
        if ((Get-FileHash -LiteralPath $destination -Algorithm SHA256).Hash.ToLowerInvariant() -ne $source.result) {
            throw ('Patched source differs: ' + $source.target)
        }
        if ($source.target -notmatch '^(sctp|dtls)/') {
            $overlay[(Join-Path $repo ('third_party/pion/' + $source.target))] = $destination
        }
    }
    $overlayFile = Join-Path $stage 'overlay.json'
    @{ Replace = $overlay } | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath $overlayFile -Encoding utf8
    $moduleFile = Join-Path $stage 'product.mod'
    Copy-Item -LiteralPath (Join-Path $repo 'go.mod') -Destination $moduleFile
    Copy-Item -LiteralPath (Join-Path $repo 'go.sum') -Destination (Join-Path $stage 'product.sum')
    go mod edit "-modfile=$moduleFile" "-replace=github.com/pion/sctp=$(Join-Path $stage 'sctp')" "-replace=github.com/pion/dtls/v3=$(Join-Path $stage 'dtls')"
    if ($LASTEXITCODE -ne 0) { throw 'Research module setup failed' }
    foreach ($variant in @('baseline','batch')) { New-Item -ItemType Directory -Path (Join-Path $research $variant) -Force | Out-Null }
    go build -o (Join-Path $research 'baseline/wind.exe') ./cmd/wind
    if ($LASTEXITCODE -ne 0) { throw 'Baseline build failed' }
    go build "-modfile=$moduleFile" "-overlay=$overlayFile" -o (Join-Path $research 'batch/wind.exe') ./cmd/wind
    if ($LASTEXITCODE -ne 0) { throw 'Prototype build failed' }
    rustup run stable cargo build --manifest-path relay-rust/Cargo.toml --locked --release -p windshare-relay-native --bin relay-native
    if ($LASTEXITCODE -ne 0) { throw 'Relay build failed' }
    foreach ($variant in @('baseline','batch')) {
        Copy-Item -LiteralPath (Join-Path $repo 'relay-rust/target/release/relay-native.exe') -Destination (Join-Path $research ($variant + '/relay-native.exe'))
    }
    Write-Output ('Research binaries ready; isolated sources: ' + $stage)
} finally { Pop-Location }
