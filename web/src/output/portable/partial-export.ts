import { acquireArtifactReader } from '../origin-private/export-readers'
import { exportCompleteZipEntries } from '../progressive-zip/partial-export'
import type { RetainedZipPartialReader } from '../resume/reopen/partial-zip-continuation'
export { pickPartialExport, supportsPartialExport } from '../capability/partial-export'

export async function saveProgressivePartial(
  reader: RetainedZipPartialReader,
  destination: FileSystemFileHandle,
  signal: AbortSignal,
): Promise<void> {
  const lease = await acquireArtifactReader(reader.object.operationId)
  try {
    signal.throwIfAborted()
    const source = await reader.handle.getFile()
    const output = await destination.createWritable()
    await exportCompleteZipEntries({
      source, output, signal,
      entries: () => reader.completeEntries(),
    })
  } finally {
    lease.release()
  }
}
