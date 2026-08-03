import { writeFile } from 'node:fs/promises'
import { join } from 'node:path'

import {
  comparePortablePaths,
  requirePortableRelativePath,
} from '../../filesystem/portable-path.ts'
import type {
  GuardUploadFaultCut,
  GuardUploadSampleInput,
} from './contract.ts'

const MAXIMUM_UPLOAD_FAULT_CUTS = 32
const FOREIGN_STAGING_FAULT_FILENAME = '.windshare-foreign-fault'
const FOREIGN_STAGING_FAULT_BYTES = 'declarative foreign staging fault'

export function canonicalUploadFaultCuts(
  candidates: readonly GuardUploadFaultCut[] | undefined,
  samples: readonly GuardUploadSampleInput[],
): readonly GuardUploadFaultCut[] {
  if (candidates === undefined) return Object.freeze([])
  requireDenseDataArray(candidates, 'guard upload fault cuts')
  if (candidates.length > MAXIMUM_UPLOAD_FAULT_CUTS) {
    throw new Error('guard upload fault cuts exceed their bounded authority')
  }
  let foreignFileCutSeen = false
  const targets = new Set<string>()
  return Object.freeze(candidates.map((candidate) => {
    requirePlainDataRecord(candidate, 'guard upload fault cut')
    const keys = Object.keys(candidate).sort(comparePortablePaths)
    if (candidate.action === 'add-foreign-file-before-publication') {
      if (keys.join(',') !== 'action' || foreignFileCutSeen) {
        throw new Error('guard upload foreign-file fault cut shape is invalid or repeated')
      }
      foreignFileCutSeen = true
      return Object.freeze({ action: candidate.action })
    }
    if (candidate.action !== 'fail-before-artifact-copy' ||
        keys.join(',') !== 'action,browser,relativePath,sampleIndex') {
      throw new Error('guard upload artifact-copy fault cut shape is invalid')
    }
    const input = samples.find(({ sample }) =>
      sample.browser === candidate.browser && sample.sampleIndex === candidate.sampleIndex)
    if (input === undefined ||
        !input.sample.artifacts.some(({ relativePath }) => relativePath === candidate.relativePath)) {
      throw new Error('guard upload artifact-copy fault cut targets no indexed artifact')
    }
    const relativePath = requirePortableRelativePath(
      candidate.relativePath,
      'guard upload artifact-copy fault path',
    )
    const target = `${candidate.browser}/${candidate.sampleIndex}/${relativePath}`
    if (targets.has(target)) throw new Error('guard upload artifact-copy fault target is repeated')
    targets.add(target)
    return Object.freeze({
      action: candidate.action,
      browser: candidate.browser,
      sampleIndex: candidate.sampleIndex,
      relativePath,
    })
  }))
}

export async function applyBeforePublicationFaults(
  stagePath: string,
  faultCuts: readonly GuardUploadFaultCut[],
  signal: AbortSignal,
): Promise<void> {
  if (!faultCuts.some(({ action }) => action === 'add-foreign-file-before-publication')) return
  signal.throwIfAborted()
  await writeFile(
    join(stagePath, FOREIGN_STAGING_FAULT_FILENAME),
    FOREIGN_STAGING_FAULT_BYTES,
    { encoding: 'utf8', flag: 'wx' },
  )
  signal.throwIfAborted()
}

export function hasArtifactCopyFailureCut(
  faultCuts: readonly GuardUploadFaultCut[],
  sample: GuardUploadSampleInput['sample'],
  relativePath: string,
): boolean {
  return faultCuts.some((candidate) =>
    candidate.action === 'fail-before-artifact-copy' &&
    candidate.browser === sample.browser &&
    candidate.sampleIndex === sample.sampleIndex &&
    candidate.relativePath === relativePath)
}

function requireDenseDataArray(value: readonly unknown[], label: string): void {
  const keys = Reflect.ownKeys(value)
  if (keys.some((key) => typeof key !== 'string' ||
      (key !== 'length' && !/^(?:0|[1-9]\d*)$/u.test(key)))) {
    throw new Error(`${label} contains non-index properties`)
  }
  for (let index = 0; index < value.length; index += 1) {
    const descriptor = Object.getOwnPropertyDescriptor(value, String(index))
    if (descriptor === undefined || !descriptor.enumerable || !('value' in descriptor)) {
      throw new Error(`${label} must be a dense data array`)
    }
  }
}

function requirePlainDataRecord(value: object, label: string): void {
  if (Object.getPrototypeOf(value) !== Object.prototype) throw new Error(`${label} must be a plain object`)
  const descriptors = Object.getOwnPropertyDescriptors(value)
  if (Reflect.ownKeys(value).some((key) => typeof key !== 'string') ||
      Object.values(descriptors).some((descriptor) => !descriptor.enumerable || !('value' in descriptor))) {
    throw new Error(`${label} contains hidden or executable properties`)
  }
}
