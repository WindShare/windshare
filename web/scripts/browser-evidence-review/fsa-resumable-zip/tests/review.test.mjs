import assert from 'node:assert/strict'
import { cpSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import test from 'node:test'
import { reviewFrozenSupportMatrix } from '../review.mjs'

const REPOSITORY_ROOT = resolve(import.meta.dirname, '..', '..', '..', '..', '..')
const ARTIFACT_PATH = 'testdata/browser-evidence/v1'
const REVIEWED_MATRIX_SHA256 = '33d68466931b6ecc594b3e264218acc669e3ca7601f2fc60b0f1620d5d42e867'

test('reviews only the compact frozen support artifacts', () => {
  const result = reviewFrozenSupportMatrix(REPOSITORY_ROOT)
  assert.equal(result.matrixSha256, REVIEWED_MATRIX_SHA256)
  assert.deepEqual(result.reviewedEntryIds, [
    'windows-11-pro-10.0.26200-x64-edge-151.0.4129.93-ntfs-4096',
  ])
  assert.equal(result.policyConstants.zipWorkspaceRecommendationMaximumPeakBytes, 1_073_744_986)
})

test('rejects matrix byte drift before considering reviewed claims', (context) => {
  const root = fixture(context)
  const path = join(root, ARTIFACT_PATH, 'fsa-resumable-zip-support-matrix.json')
  writeFileSync(path, readFileSync(path, 'utf8').replace('1073744986', '1073744987'))
  assert.throws(() => reviewFrozenSupportMatrix(root), /detached digest/u)
})

test('rejects frozen schema byte drift', (context) => {
  const root = fixture(context)
  const path = join(root, ARTIFACT_PATH, 'fsa-resumable-zip-support-candidate.schema.json')
  const schema = JSON.parse(readFileSync(path, 'utf8'))
  schema.$id = 'windshare/browser-fsa-resumable-zip-support-matrix-candidate/invalid'
  writeFileSync(path, `${JSON.stringify(schema, null, 2)}\n`)
  assert.throws(() => reviewFrozenSupportMatrix(root), /frozen schema digest changed/u)
})

function fixture(context) {
  const root = mkdtempSync(join(tmpdir(), 'windshare-fsa-support-review-'))
  context.after(() => rmSync(root, { recursive: true }))
  const target = join(root, ARTIFACT_PATH)
  cpSync(join(REPOSITORY_ROOT, ARTIFACT_PATH), target, { recursive: true })
  assert.equal(dirname(target), join(root, 'testdata', 'browser-evidence'))
  return root
}
