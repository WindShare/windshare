import { contentBytes } from '../content.mjs'

const TARGET_CONCURRENCY = 8
const PICKER_ID = 'windshare-fsa-evidence-v1'
const workloadUrl = new URL('/canonical-workload.json', location.origin)
const query = new URLSearchParams(location.search)
const runId = query.get('run') ?? crypto.randomUUID()
const runButton = document.querySelector('#run-evidence')
const statusNode = document.querySelector('#status')
const resultNode = document.querySelector('#result')

function setStatus(value) {
  statusNode.textContent = value
}

function serializeError(error) {
  return error instanceof Error
    ? { name: error.name, message: error.message, stack: error.stack ?? null }
    : { name: 'Error', message: String(error), stack: null }
}

async function loadWorkload() {
  const response = await fetch(workloadUrl, { cache: 'no-store' })
  if (!response.ok) throw new Error(`Canonical workload request failed: ${response.status}`)
  const workload = await response.json()
  if (workload?.facts?.fileCount !== 582 || workload?.facts?.directoryCount !== 105 ||
      workload?.facts?.totalBytes !== 6_762_858 || workload?.facts?.emptyFileCount !== 31) {
    throw new Error('Canonical workload aggregate contract does not match W7-A')
  }
  return workload
}

async function materialize(root, workload) {
  const directories = new Map([['', root]])
  for (const path of workload.directories) {
    const slash = path.lastIndexOf('/')
    const parentPath = slash < 0 ? '' : path.slice(0, slash)
    const name = slash < 0 ? path : path.slice(slash + 1)
    const parent = directories.get(parentPath)
    if (parent === undefined) throw new Error(`Canonical directory parent is unavailable: ${parentPath}`)
    directories.set(path, await parent.getDirectoryHandle(name, { create: true }))
  }

  const handles = await Promise.all(workload.files.map(async (file) => {
    const slash = file.path.lastIndexOf('/')
    const parent = directories.get(file.path.slice(0, slash))
    if (parent === undefined) throw new Error(`Canonical file parent is unavailable: ${file.path}`)
    return parent.getFileHandle(file.path.slice(slash + 1), { create: true })
  }))

  let next = 0
  let active = 0
  let peakActive = 0
  let firstWriteAt = null
  let lastByteAt = null
  async function worker() {
    while (true) {
      const index = next
      next += 1
      if (index >= workload.files.length) return
      const file = workload.files[index]
      if (file.sizeBytes === 0) continue
      active += 1
      peakActive = Math.max(peakActive, active)
      let writer
      let closed = false
      try {
        writer = await handles[index].createWritable()
        await writer.write(contentBytes(file.ordinal, file.sizeBytes))
        const writtenAt = performance.now()
        if (firstWriteAt === null) firstWriteAt = writtenAt
        lastByteAt = Math.max(lastByteAt ?? writtenAt, writtenAt)
        await writer.close()
        closed = true
      } finally {
        active -= 1
        if (writer !== undefined && !closed) {
          try { await writer.abort() } catch {}
        }
      }
    }
  }
  await Promise.all(Array.from({ length: TARGET_CONCURRENCY }, worker))
  if (firstWriteAt === null || lastByteAt === null) throw new Error('Canonical workload produced no non-empty writes')
  return { firstWriteAt, lastByteAt, peakActive }
}

async function runEvidence() {
  delete globalThis.__windShareFsaEvidenceResult
  runButton.disabled = true
  setStatus('Choose the prepared empty target directory.')
  try {
    const workload = await loadWorkload()
    const root = await globalThis.showDirectoryPicker({ id: PICKER_ID, mode: 'readwrite' })
    const authorityAcquiredAt = performance.now()
    const permission = await root.queryPermission({ mode: 'readwrite' })
    if (permission !== 'granted') throw new DOMException('Read/write permission was not granted', 'NotAllowedError')
    setStatus('Materializing the canonical workload with pure FSA at c8…')
    const run = await materialize(root, workload)
    const completedAt = performance.now()
    globalThis.__windShareFsaEvidenceResult = {
      schema: 'windshare/fsa-small-file-native-baseline-raw/v1',
      ok: true,
      runId,
      directoryName: root.name,
      permission,
      concurrency: TARGET_CONCURRENCY,
      timing: {
        durationMilliseconds: completedAt - authorityAcquiredAt,
        authorityToFirstWriteMilliseconds: run.firstWriteAt - authorityAcquiredAt,
        firstWriteToLastByteMilliseconds: run.lastByteAt - run.firstWriteAt,
        lastByteToCompletedMilliseconds: completedAt - run.lastByteAt,
      },
      peakActive: run.peakActive,
      facts: workload.facts,
    }
    setStatus('Pure FSA baseline completed.')
  } catch (error) {
    globalThis.__windShareFsaEvidenceResult = {
      schema: 'windshare/fsa-small-file-native-baseline-raw/v1',
      ok: false,
      runId,
      error: serializeError(error),
    }
    setStatus('Pure FSA baseline failed.')
  } finally {
    resultNode.textContent = JSON.stringify(globalThis.__windShareFsaEvidenceResult, null, 2)
  }
}

if (typeof globalThis.showDirectoryPicker !== 'function') {
  setStatus('File System Access API is unavailable.')
} else {
  runButton.disabled = false
  runButton.addEventListener('click', runEvidence)
}
globalThis.__windShareFsaEvidenceReady = true
