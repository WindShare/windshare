import { contentBytes } from '../fsa-small-file/content.mjs'
import {
  CHUNK_BYTES, REFERENCE_IMPLEMENTATION, createStageWriter, emit, inventory,
  openCase, targetFile, verifyFile, writeManifest,
} from './browser-storage.mjs'

async function receiveStage(tree, caseId, entry, bytes, sample) {
  const writer = createStageWriter()
  const stageName = `stage-${entry.ordinal}.bin`
  try {
    await writer.command({ kind: 'open', caseId, name: stageName })
    for (let offset = 0; offset < bytes.length; offset += CHUNK_BYTES) {
      const chunk = bytes.subarray(offset, offset + CHUNK_BYTES)
      await writer.command({ kind: 'write', offset, bytes: chunk })
      await emit('stage-write', { caseId, path: entry.path, bytes: chunk.length })
      if (sample) await inventory(tree, caseId, 'receiving-stage')
    }
    await writer.command({ kind: 'close' })
    await writeManifest(tree, { entry, stageName, targetSaved: false })
    await emit('stage-complete', { caseId, path: entry.path })
    return stageName
  } finally { writer.terminate() }
}

async function copyToTarget(tree, caseId, entry, blob, { staged, failAfterBytes, sample }) {
  const file = await targetFile(tree.target, entry.path)
  const stream = await file.createWritable({ keepExistingData: false })
  let written = 0
  try {
    for (let offset = 0; offset < blob.size; offset += CHUNK_BYTES) {
      const chunk = await blob.slice(offset, offset + CHUNK_BYTES).arrayBuffer()
      await stream.write(chunk)
      written += chunk.byteLength
      await emit('target-write', { caseId, path: entry.path, origin: staged ? 'staging' : 'source', bytes: chunk.byteLength })
      if (sample) await inventory(tree, caseId, 'copying-target')
      if (failAfterBytes !== null && written >= failAfterBytes) throw new Error('Injected destination write failure')
    }
    await stream.close()
    await emit('target-committed', { caseId, path: entry.path, bytes: written })
  } catch (error) {
    await stream.abort().catch(() => undefined)
    await emit('copy-failed', { caseId, path: entry.path, writtenBytes: written, error: error.message })
    await inventory(tree, caseId, 'copy-failed-writer-aborted')
    throw error
  }
}

async function publishStage(tree, caseId, entry, stageName, options) {
  const stage = await (await tree.stages.getFileHandle(stageName)).getFile()
  await copyToTarget(tree, caseId, entry, stage, { ...options, staged: true })
  await writeManifest(tree, { entry, stageName, targetSaved: true })
  await inventory(tree, caseId, 'saved-before-stage-removal')
  await tree.stages.removeEntry(stageName)
  await emit('stage-removed', { caseId, path: entry.path, bytes: entry.sizeBytes })
  await inventory(tree, caseId, 'stage-released')
}

export async function runCase(input) {
  const { caseId, files, failAfterBytes = null, sample = true } = input
  const tree = await openCase(caseId, true)
  await inventory(tree, caseId, 'ready')
  const start = performance.now()
  try {
    for (const entry of files) {
      const bytes = contentBytes(entry.ordinal, entry.sizeBytes)
      await emit('source-read', { caseId, path: entry.path, bytes: bytes.length })
      if (entry.placement === 'staged' && input.mode !== 'direct') {
        const stageName = await receiveStage(tree, caseId, entry, bytes, sample)
        await publishStage(tree, caseId, entry, stageName, { failAfterBytes, sample })
      } else {
        await copyToTarget(tree, caseId, entry, new Blob([bytes]), { staged: false, failAfterBytes: null, sample })
      }
    }
  } catch (error) {
    if (error.message !== 'Injected destination write failure') throw error
    return { status: 'copy-failed', implementation: REFERENCE_IMPLEMENTATION, milliseconds: performance.now() - start }
  }
  const milliseconds = performance.now() - start
  const verified = []
  for (const entry of files) verified.push(await verifyFile(tree.target, entry))
  await inventory(tree, caseId, 'finished')
  return { status: 'saved', implementation: REFERENCE_IMPLEMENTATION, milliseconds, verified }
}

export async function resumeCase({ caseId }) {
  const tree = await openCase(caseId, false)
  const manifestFile = await (await tree.run.getFileHandle('recovery.json')).getFile()
  const manifest = JSON.parse(await manifestFile.text())
  if (manifest.targetSaved) throw new Error('Retry fixture is already saved')
  const start = performance.now()
  await inventory(tree, caseId, 'reopened-retained-stage')
  await publishStage(tree, caseId, manifest.entry, manifest.stageName, { failAfterBytes: null, sample: true })
  const verified = await verifyFile(tree.target, manifest.entry)
  return { status: 'saved', implementation: REFERENCE_IMPLEMENTATION, milliseconds: performance.now() - start, verified }
}

export async function cleanupCase({ caseId }) {
  const tree = await openCase(caseId, false)
  await tree.root.removeEntry(caseId, { recursive: true })
  await tree.nativeParent?.removeEntry(caseId, { recursive: true })
  await emit('case-cleaned', { caseId })
}
