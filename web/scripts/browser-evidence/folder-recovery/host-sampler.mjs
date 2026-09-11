import { lstat, readdir } from 'node:fs/promises'
import { join, resolve } from 'node:path'

const ALLOCATED_BLOCK_BYTES = 512
export const SAMPLE_INTERVAL_MILLISECONDS = 25

export async function sampleHostTree(root, now = () => Date.now()) {
  const sample = {
    atMilliseconds: now(), logicalFileBytes: 0, allocatedFileBytes: 0,
    fileCount: 0, missingDuringScan: 0, inaccessible: [], allocationAvailable: true,
  }
  async function visit(path) {
    let entries
    try { entries = await readdir(path, { withFileTypes: true }) }
    catch (error) {
      if (error.code === 'ENOENT') { sample.missingDuringScan += 1; return }
      sample.inaccessible.push({ path, code: error.code ?? error.name }); return
    }
    for (const entry of entries) {
      const entryPath = join(path, entry.name)
      if (entry.isSymbolicLink()) { sample.inaccessible.push({ path: entryPath, code: 'symlink-excluded' }); continue }
      if (entry.isDirectory()) { await visit(entryPath); continue }
      if (!entry.isFile()) continue
      try {
        const metadata = await lstat(entryPath)
        sample.logicalFileBytes += metadata.size
        sample.fileCount += 1
        if (Number.isSafeInteger(metadata.blocks) && metadata.blocks >= 0) {
          sample.allocatedFileBytes += metadata.blocks * ALLOCATED_BLOCK_BYTES
        } else sample.allocationAvailable = false
      } catch (error) {
        if (error.code === 'ENOENT') sample.missingDuringScan += 1
        else sample.inaccessible.push({ path: entryPath, code: error.code ?? error.name })
      }
    }
  }
  await visit(resolve(root))
  if (!sample.allocationAvailable) sample.allocatedFileBytes = null
  return sample
}

export function summarizeHostSamples(samples) {
  if (samples.length === 0) throw new Error('Host samples are required')
  const available = samples.filter(item => item.allocatedFileBytes !== null)
  const nativeTarget = samples.some(item => item.targetLogicalFileBytes !== undefined)
  return {
    scope: nativeTarget
      ? 'sum of independently scanned isolated browser profile and isolated external FSA target; per-root counts retained in each sample'
      : 'isolated browser profile, including browser databases/cache and OPFS; destination surrogate is within this profile',
    measurement: 'host stat file lengths and 512-byte allocated-block counts; non-atomic sampled peak estimate',
    sampledPeakLogicalFileBytes: Math.max(...samples.map(item => item.logicalFileBytes)),
    sampledPeakAllocatedFileBytes: available.length === samples.length
      ? Math.max(...available.map(item => item.allocatedFileBytes)) : null,
    sampledPeakProfileLogicalFileBytes: nativeTarget ? Math.max(...samples.map(item => item.profileLogicalFileBytes)) : null,
    sampledPeakNativeTargetLogicalFileBytes: nativeTarget ? Math.max(...samples.map(item => item.targetLogicalFileBytes)) : null,
    firstLogicalFileBytes: samples[0].logicalFileBytes,
    samples: samples.length,
    incompleteSamples: samples.filter(item => item.inaccessible.length > 0).length,
    limitations: [
      'Scanning is non-atomic: it can miss short-lived copies or combine file states from different instants, so it is not a strict bound on simultaneous peak.',
      'Allocated blocks are filesystem-reported allocation, not unique physical extents or volume free-space change.',
      'Profile totals include browser overhead; per-root maxima can occur at different times and must not be added to infer simultaneous peak.',
    ],
  }
}
