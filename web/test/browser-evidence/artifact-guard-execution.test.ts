import { readFile, writeFile } from 'node:fs/promises'
import { join } from 'node:path'

import { afterEach, beforeAll, describe, expect, it } from 'vitest'

import { sha256Bytes } from '../../scripts/browser-evidence/artifact/manifest.ts'
import { validateArtifactGuardForSample } from '../../scripts/browser-evidence/artifact/guard-result.ts'
import {
  artifactEnvironment,
  artifactRootForOutcome,
  createFrameworkWorkspace,
  createZip,
  guardSyntheticSample,
  loadFrameworkTopology,
  removeFrameworkWorkspace,
  runSyntheticSample,
  type FrameworkTopology,
} from './framework-fixtures.ts'

let topology: FrameworkTopology
const workspaces: string[] = []

beforeAll(async () => { topology = await loadFrameworkTopology() })
afterEach(async () => {
  await Promise.all(workspaces.splice(0).map(removeFrameworkWorkspace))
})

describe('fail-closed artifact and ZIP guard', () => {
  it('quarantines explicit secrets without rewriting independent sample outcomes', async () => {
    const workspace = await trackedWorkspace()
    const secret = 'runner-secret-value-123'
    const source = join(workspace, 'diagnostic.txt')
    await writeFile(source, `diagnostic before ${secret} after`, 'utf8')
    const outcome = await runSyntheticSample({
      workspace,
      topology,
      suite: 'main',
      mode: 'main-unavailable',
      environment: artifactEnvironment(source, 'playwright/diagnostic.txt', 'process-log', 'text/plain'),
    })
    const resultBefore = await readFile(outcome.resultPath, 'utf8')
    const guard = await guardSyntheticSample(outcome, [secret])
    expect(guard).toMatchObject({ guardOutcome: 'quarantined' })
    expect(guard.matches).toContainEqual(expect.objectContaining({
      location: 'file',
      detector: 'explicit-secret',
    }))
    expect(await readFile(outcome.resultPath, 'utf8')).toBe(resultBefore)
    // Quarantine is a visibility decision, not a path mutation: the retained
    // source may contain secrets but the suite transaction returns no upload authority.
    await expect(readFile(join(artifactRootForOutcome(outcome), 'playwright', 'diagnostic.txt'), 'utf8'))
      .resolves.toContain(secret)
  })

  it('detects GitHub token formats but permits synthetic capability, SDP, ICE, and digest diagnostics', async () => {
    const workspace = await trackedWorkspace()
    const safeSource = join(workspace, 'safe.txt')
    await writeFile(
      safeSource,
      'windshare://fixture/capability#key=synthetic\na=candidate:1 1 udp 1 192.0.2.10 40000 typ host\nsha256=' + 'b'.repeat(64),
      'utf8',
    )
    const safe = await runSyntheticSample({
      workspace,
      topology,
      suite: 'main',
      sampleIndex: 1,
      mode: 'main-unavailable',
      environment: artifactEnvironment(safeSource, 'playwright/safe.txt', 'process-log', 'text/plain'),
    })
    expect((await guardSyntheticSample(safe)).guardOutcome).toBe('passed')

    const tokenSource = join(workspace, 'token.txt')
    await writeFile(tokenSource, `diagnostic ghp_${'A'.repeat(36)} terminator`, 'utf8')
    const token = await runSyntheticSample({
      workspace,
      topology,
      suite: 'main',
      sampleIndex: 2,
      mode: 'main-unavailable',
      environment: artifactEnvironment(tokenSource, 'playwright/token.txt', 'process-log', 'text/plain'),
    })
    const tokenGuard = await guardSyntheticSample(token)
    expect(tokenGuard.guardOutcome).toBe('quarantined')
    expect(tokenGuard.matches).toContainEqual(expect.objectContaining({
      detector: 'github-token-pattern',
    }))
  })

  it('unpacks ZIP entries for scanning and attributes matches to normalized member paths', async () => {
    const workspace = await trackedWorkspace()
    const secret = 'zip-entry-secret-456'
    const source = join(workspace, 'trace.zip')
    await writeFile(source, await createZip([
      { name: 'logs/diagnostic.txt', data: `before ${secret} after` },
    ]))
    const outcome = await runSyntheticSample({
      workspace,
      topology,
      suite: 'main',
      mode: 'main-unavailable',
      environment: artifactEnvironment(source, 'playwright/trace.zip', 'trace', 'application/zip'),
    })
    const guard = await guardSyntheticSample(outcome, [secret])
    expect(guard.guardOutcome).toBe('quarantined')
    expect(guard.matches).toContainEqual(expect.objectContaining({
      location: 'archive-entry',
      archiveEntryPath: 'logs/diagnostic.txt',
      detector: 'explicit-secret',
    }))
    expect(guard.scanEvidence).toMatchObject({
      scannedArchiveEntryCount: 1,
      observedMaximumArchiveDepth: 1,
    })
  })

  it('detects a preamble-bearing ZIP by its central-directory authority without relying on its name', async () => {
    const workspace = await trackedWorkspace()
    const secret = 'preamble-archive-secret'
    const source = join(workspace, 'self-extracting.bin')
    const archive = await createZip([{ name: 'logs/secret.txt', data: secret }])
    await writeFile(source, Buffer.concat([Buffer.from('MZ-synthetic-preamble'), Buffer.from(archive)]))
    const outcome = await runSyntheticSample({
      workspace,
      topology,
      suite: 'main',
      mode: 'main-unavailable',
      environment: artifactEnvironment(
        source,
        'playwright/self-extracting.bin',
        'trace',
        'application/octet-stream',
      ),
    })
    const guard = await guardSyntheticSample(outcome, [secret])
    expect(guard.guardOutcome).toBe('quarantined')
    expect(guard.scanEvidence.scannedArchiveEntryCount).toBe(1)
    expect(guard.matches).toContainEqual(expect.objectContaining({
      location: 'archive-entry',
      archiveEntryPath: 'logs/secret.txt',
    }))
  })

  it('rejects traversal, malformed archives, and extensionless nested archive bombs', async () => {
    const workspace = await trackedWorkspace()
    const traversalSource = join(workspace, 'traversal.zip')
    await writeFile(traversalSource, await createZip([{ name: '../secret.txt', data: 'synthetic' }]))
    const traversal = await runArchiveSample(workspace, 1, traversalSource, 'traversal.zip')
    expect((await guardSyntheticSample(traversal)).failureCode).toBe('archive-path')

    const malformedSource = join(workspace, 'malformed.zip')
    await writeFile(malformedSource, Buffer.from('504b030462726f6b656e', 'hex'))
    const malformed = await runArchiveSample(workspace, 2, malformedSource, 'malformed.zip')
    expect((await guardSyntheticSample(malformed)).failureCode).toBe('invalid-archive')

    const inner = await createZip([{ name: 'payload.txt', data: 'synthetic nested payload' }])
    const nestedSource = join(workspace, 'outer.zip')
    await writeFile(nestedSource, await createZip([{ name: 'opaque.bin', data: inner }]))
    const nested = await runArchiveSample(workspace, 3, nestedSource, 'outer.zip')
    const nestedGuard = await guardSyntheticSample(nested)
    expect(nestedGuard.failureCode).toBe('archive-nesting-limit')
    expect(nestedGuard.scanEvidence.observedMaximumArchiveDepth).toBe(2)
    expect(nestedGuard.uploadableArtifactIds).toEqual([])
  })

  it('classifies malformed result snapshot bytes as a contract failure', async () => {
    const workspace = await trackedWorkspace()
    const outcome = await runSyntheticSample({
      workspace,
      topology,
      suite: 'main',
      mode: 'main-unavailable',
    })
    await writeFile(outcome.resultPath, '{"schemaVersion":1,"schemaVersion":1}', 'utf8')
    const guard = await guardSyntheticSample(outcome)
    expect(guard).toMatchObject({
      guardOutcome: 'failed',
      failureCode: 'contract',
      uploadableArtifactIds: [],
    })
  })

  it('turns scanner crashes into zero-upload failed guard authority', async () => {
    const workspace = await trackedWorkspace()
    const outcome = await runSyntheticSample({
      workspace,
      topology,
      suite: 'main',
      mode: 'main-unavailable',
    })
    const guard = await guardSyntheticSample(outcome, [], {
      action: 'fail-before-artifact-scan',
      relativePath: outcome.result.artifacts[0]!.relativePath,
    })
    expect(guard).toMatchObject({
      guardOutcome: 'failed',
      failureCode: 'scanner-crashed',
      uploadableArtifactIds: [],
    })
    expect(guard.quarantinedArtifactIds).toEqual(guard.checkedArtifactIds)
  })

  it('fails closed when indexed bytes change between workspace verification and scanning', async () => {
    const workspace = await trackedWorkspace()
    const source = join(workspace, 'mutable.txt')
    await writeFile(source, 'indexed-safe-bytes', 'utf8')
    const relativePath = 'playwright/mutable.txt'
    const outcome = await runSyntheticSample({
      workspace,
      topology,
      suite: 'main',
      mode: 'main-unavailable',
      environment: artifactEnvironment(source, relativePath, 'process-log', 'text/plain'),
    })
    const guard = await guardSyntheticSample(outcome, ['replacement-secret'], {
      action: 'replace-artifact-before-scan',
      relativePath,
      replacementUtf8: 'replacement-secret',
    })
    expect(guard).toMatchObject({
      guardOutcome: 'failed',
      failureCode: 'contract',
      uploadableArtifactIds: [],
    })
  })

  it('binds guard authorization to the exact artifact bytes and metadata, not only paths', async () => {
    const firstWorkspace = await trackedWorkspace()
    const secondWorkspace = await trackedWorkspace()
    const firstSource = join(firstWorkspace, 'diagnostic.txt')
    const secondSource = join(secondWorkspace, 'diagnostic.txt')
    await writeFile(firstSource, 'first artifact bytes', 'utf8')
    await writeFile(secondSource, 'second artifact bytes', 'utf8')
    const relativePath = 'playwright/diagnostic.txt'
    const first = await runSyntheticSample({
      workspace: firstWorkspace,
      topology,
      suite: 'main',
      mode: 'main-unavailable',
      environment: artifactEnvironment(firstSource, relativePath, 'process-log', 'text/plain'),
    })
    const second = await runSyntheticSample({
      workspace: secondWorkspace,
      topology,
      suite: 'main',
      mode: 'main-unavailable',
      environment: artifactEnvironment(secondSource, relativePath, 'process-log', 'text/plain'),
    })
    const firstGuard = await guardSyntheticSample(first)
    const secondResultSha256 = sha256Bytes(await readFile(second.resultPath))
    expect(() => validateArtifactGuardForSample(
      firstGuard,
      second.result,
      secondResultSha256,
    )).toThrow(/exact browser sample result bytes/u)
    expect(() => validateArtifactGuardForSample(
      { ...firstGuard, sampleResultSha256: secondResultSha256 },
      second.result,
      secondResultSha256,
    )).toThrow(/canonical full sample artifact manifest/u)
  })

  it('blocks mandatory result and manifest metadata that contain a protected token', async () => {
    const workspace = await trackedWorkspace()
    const source = join(workspace, 'safe-control-plane-source.txt')
    await writeFile(source, 'safe attachment bytes', 'utf8')
    const token = `ghp_${'A'.repeat(36)}`
    const outcome = await runSyntheticSample({
      workspace,
      topology,
      suite: 'main',
      mode: 'main-unavailable',
      environment: artifactEnvironment(
        source,
        `playwright/${token}.txt`,
        'process-log',
        'text/plain',
      ),
    })
    const guard = await guardSyntheticSample(outcome)
    expect(guard).toMatchObject({
      guardOutcome: 'failed',
      failureCode: 'contract',
      failureMessage: 'browser evidence control-plane bytes contain a protected secret',
      uploadableArtifactIds: [],
    })
  })
})

async function runArchiveSample(
  workspace: string,
  sampleIndex: number,
  source: string,
  name: string,
) {
  return runSyntheticSample({
    workspace,
    topology,
    suite: 'main',
    sampleIndex,
    mode: 'main-unavailable',
    environment: artifactEnvironment(source, `playwright/${name}`, 'trace', 'application/zip'),
  })
}

async function trackedWorkspace(): Promise<string> {
  const workspace = await createFrameworkWorkspace()
  workspaces.push(workspace)
  return workspace
}
