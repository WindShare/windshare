import { createBrowserDirectZipComposition } from '/src/ui/browser-receive/direct-zip/production.ts'
import { createBrowserReceiveComposition } from '/src/ui/v2-browser-receive-composition.ts'
import { IndexedDbReceiveOperationRepository } from '/src/output/browser/indexeddb-repository.ts'
import { IndexedDbDirectZipJournalRepository } from '/src/output/direct-zip/journal/index.ts'
import { readEnvelope } from '/src/ui/browser-receive/direct-zip/resources.ts'
import { prepareProductionDirectZipActivation } from '/test/browser/direct-zip/production-activation.ts'

// The test picker chooses OPFS; production activation, journaling, transfer and
// publication still own all writes. The activation fixture is bound to the real share.
export async function bindProductionZip(joined) {
  const databaseName = 'allocation-zip-' + crypto.randomUUID()
  const root = await navigator.storage.getDirectory()
  const parent = await root.getDirectoryHandle(databaseName, { create: true })
  const picker = Object.getOwnPropertyDescriptor(window, 'showDirectoryPicker')
  Object.defineProperty(window, 'showDirectoryPicker', { configurable: true, value: async () => parent })
  const openRepository = () => IndexedDbReceiveOperationRepository.open(databaseName)
  const directZip = createBrowserDirectZipComposition(window, {
    openRepository, openJournal: () => IndexedDbDirectZipJournalRepository.open({ databaseName }),
  })
  const receiver = createBrowserReceiveComposition(window, { directZip })
  window.__zipProbeShare = joined.descriptor
  const activation = await prepareProductionDirectZipActivation(window, receiver, directZip, new AbortController().signal)
  const committed = await activation.commit()
  if (committed.kind !== 'bound-operation') throw new Error('ZIP activation failed: ' + committed.kind)
  const active = committed.operation
  const writes = []
  const prototype = FileSystemWritableFileStream.prototype
  const originalWrite = prototype.write
  const originalClose = prototype.close
  let closed
  prototype.write = async function (chunk) {
    const at = performance.now()
    await originalWrite.call(this, chunk)
    writes.push({ at, end: performance.now(), bytes: chunk?.data?.byteLength ?? chunk?.byteLength ?? 0 })
  }
  prototype.close = async function () {
    await originalClose.call(this)
    closed = performance.now()
  }
  return {
    intent: active.intent, plans: active.plans, transferJobId: active.transferJobId,
    selection: joined.selection.snapshot(),
    snapshot: () => ({ writes, closed }),
    read: async () => {
      const repository = await openRepository()
      let envelope
      try { envelope = await readEnvelope(repository, active.intent.operationId) } finally { repository.close() }
      const file = await (await parent.getFileHandle(envelope.candidate.stableName)).getFile()
      const archive = new Uint8Array(await file.arrayBuffer())
      return { archiveBytes: archive.length, file: new Blob([storedMember(archive, 'probe.bin')]) }
    },
    close: async () => {
      prototype.write = originalWrite
      prototype.close = originalClose
      await active.detach()
      if (picker) Object.defineProperty(window, 'showDirectoryPicker', picker)
      else Reflect.deleteProperty(window, 'showDirectoryPicker')
      await root.removeEntry(databaseName, { recursive: true })
    },
  }
}

// Read the central directory so a streaming local header's unknown sizes cannot
// be mistaken for an empty member. This probe accepts only the production STORE format.
function storedMember(bytes, suffix) {
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength)
  let end = bytes.length - 22
  while (end >= Math.max(0, bytes.length - 65557) &&
    (view.getUint32(end, true) !== 0x06054b50 || end + 22 + view.getUint16(end + 20, true) !== bytes.length)) end--
  if (end < Math.max(0, bytes.length - 65557)) throw new Error('Missing ZIP end record')
  let offset = view.getUint32(end + 16, true)
  if (offset === 0xffffffff) {
    const zip64 = Number(view.getBigUint64(end - 12, true))
    offset = Number(view.getBigUint64(zip64 + 48, true))
  }
  while (view.getUint32(offset, true) === 0x02014b50) {
    const nameLength = view.getUint16(offset + 28, true), extraLength = view.getUint16(offset + 30, true)
    const name = new TextDecoder().decode(bytes.subarray(offset + 46, offset + 46 + nameLength))
    let size = view.getUint32(offset + 24, true), local = view.getUint32(offset + 42, true)
    let extra = offset + 46 + nameLength
    while (extra < offset + 46 + nameLength + extraLength) {
      const tag = view.getUint16(extra, true), length = view.getUint16(extra + 2, true)
      if (tag === 1) {
        let cursor = extra + 4
        if (size === 0xffffffff) { size = Number(view.getBigUint64(cursor, true)); cursor += 8 }
        if (view.getUint32(offset + 20, true) === 0xffffffff) cursor += 8
        if (local === 0xffffffff) local = Number(view.getBigUint64(cursor, true))
      }
      extra += 4 + length
    }
    if (name.endsWith(suffix)) {
      if (view.getUint16(offset + 10, true) !== 0) throw new Error('Expected STORE member')
      const start = local + 30 + view.getUint16(local + 26, true) + view.getUint16(local + 28, true)
      return bytes.subarray(start, start + size)
    }
    offset += 46 + nameLength + extraLength + view.getUint16(offset + 32, true)
  }
  throw new Error('ZIP member missing')
}
