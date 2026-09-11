import { loadNativeTarget } from './native-target.mjs'

export const CHUNK_BYTES = 1024 * 1024
export const REFERENCE_IMPLEMENTATION = 'raw browser API reference; not WindShare delivery runtime'

export async function emit(kind, detail = {}) {
  await globalThis.__folderEvidenceEvent({ kind, atMilliseconds: performance.now(), ...detail })
}

export async function openCase(caseId, create) {
  if (!/^folder-evidence-[a-z0-9-]+$/u.test(caseId)) throw new Error('Invalid evidence case identity')
  const root = await navigator.storage.getDirectory()
  const run = await root.getDirectoryHandle(caseId, { create })
  const stages = await run.getDirectoryHandle('stages', { create })
  const nativeParent = await loadNativeTarget()
  const target = nativeParent
    ? await nativeParent.getDirectoryHandle(caseId, { create })
    : await run.getDirectoryHandle('target', { create })
  return { root, run, stages, target, nativeParent }
}

export async function targetFile(root, path) {
  const parts = path.split('/')
  let parent = root
  for (const part of parts.slice(0, -1)) parent = await parent.getDirectoryHandle(part, { create: true })
  return parent.getFileHandle(parts.at(-1), { create: true })
}

export async function inventory(tree, caseId, phase) {
  async function files(root, prefix = '') {
    const result = []
    for await (const entry of root.values()) {
      const path = prefix + entry.name
      if (entry.kind === 'directory') result.push(...await files(entry, path + '/'))
      else result.push({ path, bytes: (await entry.getFile()).size })
    }
    return result.sort((left, right) => left.path.localeCompare(right.path))
  }
  const stageFiles = await files(tree.stages)
  const targetFiles = await files(tree.target)
  await emit('inventory', {
    caseId, phase, stageFiles, targetFiles,
    stageBytes: stageFiles.reduce((sum, file) => sum + file.bytes, 0),
    targetBytes: targetFiles.reduce((sum, file) => sum + file.bytes, 0),
  })
}

export async function writeManifest(tree, manifest) {
  const file = await tree.run.getFileHandle('recovery.json', { create: true })
  const stream = await file.createWritable()
  await stream.write(JSON.stringify(manifest))
  await stream.close()
}

export async function verifyFile(root, entry) {
  const file = await (await targetFile(root, entry.path)).getFile()
  const digest = [...new Uint8Array(await crypto.subtle.digest('SHA-256', await file.arrayBuffer()))]
    .map(value => value.toString(16).padStart(2, '0')).join('')
  if (file.size !== entry.sizeBytes || digest !== entry.sha256) throw new Error(`Output mismatch: ${entry.path}`)
  return { path: entry.path, bytes: file.size, sha256: digest }
}

export function createStageWriter() {
  const worker = new Worker(new URL('./stage-worker.mjs', import.meta.url), { type: 'module' })
  let sequence = 0
  const pending = new Map()
  worker.onmessage = ({ data }) => {
    const request = pending.get(data.id)
    if (!request) return
    pending.delete(data.id)
    if (data.ok) request.resolve()
    else request.reject(new Error(`${data.error.name}: ${data.error.message}`))
  }
  worker.onerror = error => {
    for (const request of pending.values()) request.reject(new Error(error.message))
    pending.clear()
  }
  return {
    command(input) {
      const id = ++sequence
      return new Promise((resolve, reject) => {
        pending.set(id, { resolve, reject })
        worker.postMessage({ id, ...input })
      })
    },
    terminate() { worker.terminate() },
  }
}
