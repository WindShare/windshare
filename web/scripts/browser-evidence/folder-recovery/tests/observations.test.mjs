import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtemp, mkdir, rm, writeFile } from 'node:fs/promises'
import { join } from 'node:path'
import { tmpdir } from 'node:os'
import { summarizeEvents, timingComparison } from '../observations.mjs'
import { sampleHostTree, summarizeHostSamples } from '../host-sampler.mjs'
import { MAX_CASE_BYTES, validateFiles, workloads } from '../workload.mjs'

test('reports cumulative failed and retried copy writes separately from simultaneous file occupancy', () => {
  const summary = summarizeEvents([
    { kind: 'source-read', bytes: 8 },
    { kind: 'inventory', stageBytes: 8, targetBytes: 0 },
    { kind: 'target-write', origin: 'staging', bytes: 4 },
    { kind: 'copy-failed' },
    { kind: 'target-write', origin: 'staging', bytes: 8 },
    { kind: 'inventory', stageBytes: 8, targetBytes: 8 },
    { kind: 'stage-removed' },
    { kind: 'inventory', stageBytes: 0, targetBytes: 12 },
    { kind: 'target-write', origin: 'source', bytes: 4 },
  ])
  assert.equal(summary.cumulativeLocalCopyBytes, 12)
  assert.equal(summary.cumulativeTargetWriteBytes, 16)
  assert.equal(summary.sampledPeakCombinedFileBytes, 16)
  assert.equal(summary.sampledPeakStageFileBytes, 8)
  assert.equal(summary.receivedSourceBytes, 8)
  assert.equal(summary.copyFailures, 1)
})

test('keeps missing allocation evidence unknown and reports incomplete scans', () => {
  const summary = summarizeHostSamples([
    { logicalFileBytes: 10, allocatedFileBytes: 16, inaccessible: [] },
    { logicalFileBytes: 20, allocatedFileBytes: null, inaccessible: [{ code: 'EACCES' }] },
  ])
  assert.equal(summary.sampledPeakLogicalFileBytes, 20)
  assert.equal(summary.sampledPeakAllocatedFileBytes, null)
  assert.equal(summary.incompleteSamples, 1)
  assert.throws(() => summarizeHostSamples([]), /required/u)
})

test('native per-root peaks remain separate from the observed combined peak', () => {
  const summary = summarizeHostSamples([
    { logicalFileBytes: 40, allocatedFileBytes: 48, profileLogicalFileBytes: 10, targetLogicalFileBytes: 30, inaccessible: [] },
    { logicalFileBytes: 40, allocatedFileBytes: 48, profileLogicalFileBytes: 30, targetLogicalFileBytes: 10, inaccessible: [] },
  ])
  assert.equal(summary.sampledPeakLogicalFileBytes, 40)
  assert.equal(summary.sampledPeakProfileLogicalFileBytes, 30)
  assert.equal(summary.sampledPeakNativeTargetLogicalFileBytes, 30)
  assert.match(summary.scope, /external FSA target/u)
})

test('samples real host file lengths without substituting the logical workload model', async () => {
  const root = await mkdtemp(join(tmpdir(), 'windshare-folder-evidence-test-'))
  try {
    await mkdir(join(root, 'nested'))
    await writeFile(join(root, 'first'), new Uint8Array(3))
    await writeFile(join(root, 'nested', 'second'), new Uint8Array(7))
    const sample = await sampleHostTree(root, () => 5)
    assert.equal(sample.atMilliseconds, 5)
    assert.equal(sample.logicalFileBytes, 10)
    assert.equal(sample.fileCount, 2)
    assert.equal(sample.inaccessible.length, 0)
  } finally { await rm(root, { recursive: true, force: true }) }
})

test('bounds representative workloads and rejects unsafe identities before browser writes', () => {
  const fixture = workloads()
  assert.ok(fixture.mixed.reduce((sum, item) => sum + item.sizeBytes, 0) < MAX_CASE_BYTES)
  const entry = fixture.small[0]
  assert.throws(() => validateFiles([{ ...entry, path: '../escaped' }]), /Unsafe/u)
  assert.throws(() => validateFiles([{ ...entry, sizeBytes: MAX_CASE_BYTES + 1 }]), /bounded/u)
  assert.throws(() => validateFiles([entry, entry]), /Duplicate/u)
})

test('rejects malformed byte evidence instead of silently reporting unknown totals as zero', () => {
  assert.throws(() => summarizeEvents([{ kind: 'source-read' }]), /Byte/u)
  assert.throws(() => summarizeEvents([{ kind: 'target-write', bytes: -1 }]), /Byte/u)
  assert.throws(() => summarizeEvents([{ kind: 'inventory', stageBytes: Number.NaN, targetBytes: 0 }]), /Inventory/u)
})

test('timing comparison uses medians and rejects non-measurements', () => {
  assert.equal(timingComparison([30, 10, 20], [25, 15, 40]).candidateToBaselineRatio, 1.25)
  assert.equal(timingComparison([10, 20], [20, 40]).candidateToBaselineRatio, 2)
  assert.throws(() => timingComparison([0], [1]), /Positive/u)
})
