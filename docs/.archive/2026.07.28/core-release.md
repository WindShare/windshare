# Core module release

Core is released before the root module because the root's `GOWORK=off` build
must consume a published core version without a `replace` directive.

> The `core/v0.3.0` and `core-candidate/v0.3.0/*` namespaces are closed. Never create, move, force-update, or push those refs again, and do not trigger another tag-based v0.3.0 release verification. A future release must first choose a new version and exact commit; ordinary local `make core-release` remains only an artifact check and creates no tag. Its reserved `v0.0.0-ci` version is rejected by release-ref resolution.

## Candidate verification

After maintainers choose `<next-version>`, create a new lightweight candidate tag
at the exact commit, with that same lowercase 40-character commit SHA in the tag:

```text
git tag --no-sign core-candidate/<next-version>/<commit-sha> <commit-sha>
git cat-file -t refs/tags/core-candidate/<next-version>/<commit-sha>  # must print: commit
git push origin refs/tags/core-candidate/<next-version>/<commit-sha>
```

The dedicated workflow accepts only a new, non-forced lightweight tag whose raw
ref object, ref suffix, event SHA, and checked-out HEAD are the same commit. Its
external actions are pinned to reviewed full commit SHAs. It checks out that SHA
directly on Linux and Windows and installs the toolchain declared by
`core/go.mod`. After testing the helpers it re-proves the clean checkout, rejects
hidden index state, and creates a private exact-commit checkout whose verifier
inputs are raw-checked against their Git blobs. The archive builder, vulnerability
scanner, and late Windows worker run only from that checkout; every module byte is
read directly from the commit objects. It then runs tidy, verify, list, vet, build,
the internally fixed
`golang.org/x/vuln/cmd/govulncheck@v1.6.0` source/symbol scan, ordinary tests,
race tests, and the coverage gate. Every Go command uses new private module,
build, and GOPATH caches, `GOWORK=off`, `GOENV=off`, the local host toolchain,
default target and experiment selection, the public proxy and checksum database,
and no private-module bypass. The scanner
also fixes the public vulnerability database; a finding or operational failure
fails closed. The workflow requires these native tests to report top-level PASS
with no skip anywhere in the selected test trees:

- Linux/ext4: `TestLinuxExt4RestartIdentityRejectsForcedInodeReuse`,
  `TestLinuxExt4NativeCertification`, and `TestLinuxExt4ProcessRestartRecovery`;
- Windows/local-NTFS: `TestWindowsNTFSNativeCertification` and
  `TestWindowsNTFSProcessRestartRecovery`.

These tests certify process-restart recovery on the same running kernel and
mounted volume only. They make no unmount/remount, reboot, OS-crash, or
power-loss durability claim. The workflow has read-only repository permission
and never creates or pushes a tag or release.

The Linux gate builds a 128 MiB ext4 image with 1024 inodes, mounts it only in a
private namespace, and runs the static test binary in a chroot as the receiver
UID with zero effective capabilities. It exhausts the inode pool and uses
separate processes to prove 32 delete/recreate cycles reuse the same inode while
the production root binding rejects every replacement. A skip, missing pass, or
fixture/lifecycle failure blocks release.

The Windows native tests run as a temporary local standard user, not as the
elevated hosted-runner account. The worker proves its SID, non-administrator
token, and inability to create a file in the extracted artifact. It uses isolated
writable NTFS test and Go-cache directories and is removed with any profile after
its JSON test result has been checked. Process-restart children inherit the same
standard-user token and required native profile.

For ordinary local artifact checks, pass both the version and exact checked-out
commit. Use a standalone clone whose `.git` is a real directory, not a linked
worktree, and keep it clean; uncommitted bytes are never release evidence:

```text
bash scripts/ci/core-release.sh <next-version> <commit-sha>
pwsh -NoProfile -File scripts/ci/core-release.ps1 -Version <next-version> -CommitSHA <commit-sha>
```

The `linux-ext4` profile constructs its own loop-ext4 fixture; the
`windows-ntfs` profile requires a local NTFS runner. A wrong filesystem, missing
test, or skip anywhere in either required test tree is a failure. Manual workflow
dispatch remains diagnostic only and does not replace the retained candidate tag
required for publication.

## Public proxy verification

After pushing the final tag, verify the published module from fresh caches in a
disposable PowerShell session. Replace `commit-sha` with the exact lowercase
40-character SHA certified by the retained candidate tag:

```powershell
$modulePath = 'github.com/windshare/windshare/core'
$version = '<next-version>'
$commitSHA = '<commit-sha>'
if ($commitSHA -cnotmatch '^[0-9a-f]{40}$') { throw 'invalid certified commit SHA' }

$verificationRoot = Join-Path ([IO.Path]::GetTempPath()) (
    'windshare-core-proxy-{0}' -f [Guid]::NewGuid().ToString('N')
)
try {
    $env:GOMODCACHE = Join-Path $verificationRoot 'modcache'
    $env:GOCACHE = Join-Path $verificationRoot 'buildcache'
    $env:GOPATH = Join-Path $verificationRoot 'gopath'
    New-Item -ItemType Directory -Path @(
        $verificationRoot, $env:GOMODCACHE, $env:GOCACHE, $env:GOPATH
    ) | Out-Null

    $env:GOENV = 'off'
    $env:GOFLAGS = ''
    $env:GOTOOLCHAIN = 'local'
    $env:GOWORK = 'off'
    $env:GOPROXY = 'https://proxy.golang.org'
    $env:GOSUMDB = 'sum.golang.org'
    $env:GOPRIVATE = ''
    $env:GONOSUMDB = ''
    $env:GONOPROXY = ''
    $env:GOINSECURE = ''
    $env:GOTELEMETRY = 'off'

    $jsonLines = @(& go mod download -json "${modulePath}@${version}")
    if ($LASTEXITCODE -ne 0) { throw 'public module download failed' }
    $download = ConvertFrom-Json -InputObject ($jsonLines -join [Environment]::NewLine)
    $expectedRef = "refs/tags/core/$version"
    if ($download.Path -cne $modulePath -or $download.Version -cne $version) {
        throw 'public proxy returned a different module version'
    }
    if ([string]::IsNullOrWhiteSpace([string]$download.Sum) -or
        [string]::IsNullOrWhiteSpace([string]$download.GoModSum)) {
        throw 'public proxy response lacks module or go.mod sums'
    }
    if ($null -eq $download.Origin -or
        [string]$download.Origin.Ref -cne $expectedRef -or
        [string]$download.Origin.Hash -cne $commitSHA) {
        throw 'public proxy provenance does not match the certified final tag'
    }
} finally {
    if (Test-Path -LiteralPath $verificationRoot) {
        Remove-Item -Recurse -Force -LiteralPath $verificationRoot
    }
}
```

Do not update the root module until this exact public-proxy and public-sumdb
check succeeds. A local repository, direct VCS fallback, or pre-existing module
cache is not publication evidence.

## Publication order

1. Push `core-candidate/<next-version>/<commit-sha>` and wait for both native jobs to
   pass. Record the successful workflow run URL and retain the tag; ref existence
   alone is not evidence that candidate verification passed.
2. Create and push a lightweight `core/<next-version>` tag at that same commit. Its
   verification fails unless both the final and retained candidate refs directly
   contain that commit; annotated or indirect tags are rejected. Ordinary CI
   ignores both dedicated core tag namespaces.
3. Run the fresh-cache public proxy verification above against the certified
   commit SHA.
4. Only after that succeeds, update the root module's core requirement to
   `<next-version>` and its sums.
5. Run ordinary CI, including the root `GOWORK=off` build, before creating the
   root release tag.

The successful candidate run is the operator gate before the irreversible final
tag push. Tag creation, push, and publication remain deliberate maintainer
actions; the verification workflow performs none of them.
