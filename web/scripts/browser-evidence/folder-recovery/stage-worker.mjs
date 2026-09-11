let handle
let queue = Promise.resolve()

self.onmessage = ({ data }) => {
  queue = queue.then(async () => {
    try {
      if (data.kind === 'open') {
        const root = await navigator.storage.getDirectory()
        const run = await root.getDirectoryHandle(data.caseId)
        const stages = await run.getDirectoryHandle('stages')
        const file = await stages.getFileHandle(data.name, { create: true })
        handle = await file.createSyncAccessHandle()
        handle.truncate(0)
      } else if (data.kind === 'write') {
        let written = 0
        while (written < data.bytes.length) {
          const count = handle.write(data.bytes.subarray(written), { at: data.offset + written })
          if (!Number.isSafeInteger(count) || count <= 0) throw new Error('Native stage made no progress')
          written += count
        }
        handle.flush()
      } else if (data.kind === 'close') {
        handle?.close()
        handle = undefined
      } else throw new Error('Unknown stage command')
      self.postMessage({ id: data.id, ok: true })
    } catch (error) {
      self.postMessage({ id: data.id, ok: false, error: { name: error.name, message: error.message } })
    }
  })
}
