import { isAbsolute, join, resolve } from 'node:path'

import { freezeRecord } from '../../contract/json.ts'
import { requirePortableRelativePath } from '../../filesystem/portable-path.ts'
import {
  GUARD_UPLOAD_ATTACHMENTS_DIRECTORY,
  GUARD_UPLOAD_GUARD_FILENAME,
  GUARD_UPLOAD_RESULT_FILENAME,
  GUARD_UPLOAD_SAMPLES_DIRECTORY,
  type GuardUploadSampleContractPaths,
  type GuardUploadSampleManifest,
} from './contract.ts'

export function guardUploadSampleContractPaths(
  uploadDirectory: string,
  sample: Pick<GuardUploadSampleManifest, 'browser' | 'sampleIndex'>,
): GuardUploadSampleContractPaths {
  const sampleDirectory = sampleUploadRoot(
    requireCanonicalAbsolutePath(uploadDirectory, 'sealed guard upload directory'),
    sample,
  )
  return freezeRecord({
    sampleDirectory,
    resultPath: join(sampleDirectory, GUARD_UPLOAD_RESULT_FILENAME),
    guardPath: join(sampleDirectory, GUARD_UPLOAD_GUARD_FILENAME),
    attachmentsDirectory: join(sampleDirectory, GUARD_UPLOAD_ATTACHMENTS_DIRECTORY),
  })
}

export function relativeSampleUploadRoot(
  sample: Pick<GuardUploadSampleManifest, 'browser' | 'sampleIndex'>,
): string {
  return `${GUARD_UPLOAD_SAMPLES_DIRECTORY}/${sample.browser}/sample-${sample.sampleIndex}`
}

export function sampleUploadRoot(
  root: string,
  sample: Pick<GuardUploadSampleManifest, 'browser' | 'sampleIndex'>,
): string {
  return join(root, ...relativeSampleUploadRoot(sample).split('/'))
}

export function artifactPathSegments(relativePath: string): readonly string[] {
  return requirePortableRelativePath(relativePath, 'guard upload artifact path').split('/')
}

export function requireCanonicalAbsolutePath(path: string, label: string): string {
  if (!isAbsolute(path) || resolve(path) !== path || path.includes('\0')) {
    throw new Error(`${label} must be canonical and absolute`)
  }
  return path
}
