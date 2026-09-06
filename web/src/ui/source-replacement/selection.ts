import type { V2CatalogEntry } from '../../catalog/v2-records'
import type { SourceRevisionFailure } from '../../output/resume/source-revision-failures'
import type { V2BrowseDirectory, V2BrowsePage, V2JoinedBrowserShare } from '../v2-gateway'
import type { V2RetainedReceiveOperation } from '../v2-receive-runtime'

type ReplacementShare = Pick<V2JoinedBrowserShare, 'descriptor' | 'rootDirectory' | 'childDirectory' | 'page'>

export async function findReplacementFile(
  joined: ReplacementShare, operation: V2RetainedReceiveOperation,
  failure: SourceRevisionFailure, signal: AbortSignal,
): Promise<Readonly<{ entry: V2CatalogEntry; page: V2BrowsePage; directories: readonly V2BrowseDirectory[] }>> {
  const failures = operation.sourceRevisionFailures
  if (failures === undefined || !failures.files.includes(failure) ||
      failures.shareInstance !== joined.descriptor.shareInstanceId) {
    throw new DOMException('Open the matching share before downloading the current version', 'InvalidStateError')
  }
  const directories = [joined.rootDirectory()]
  let directory = directories[0]!
  for (let index = 0; index < failure.sourcePath.length; index++) {
    const found = await findNamedEntry(joined, directory, failure.sourcePath[index]!, signal)
    if (index === failure.sourcePath.length - 1) {
      if (found.entry.kind !== 'file') break
      return Object.freeze({ ...found, directories: Object.freeze(directories) })
    }
    if (found.entry.kind !== 'directory') break
    directory = joined.childDirectory(directory, found.entry)
    directories.push(directory)
  }
  throw new DOMException('The replacement file is no longer shared at this path', 'NotFoundError')
}

async function findNamedEntry(
  joined: ReplacementShare, directory: V2BrowseDirectory, name: string, signal: AbortSignal,
): Promise<Readonly<{ entry: V2CatalogEntry; page: V2BrowsePage }>> {
  let pageIndex = 0
  for (;;) {
    signal.throwIfAborted()
    // Refresh each directory once, then page the authenticated generation consistently.
    const page = await joined.page(directory, pageIndex, { signal, explicitRetry: pageIndex === 0 })
    signal.throwIfAborted()
    const entry = page.entries.find(candidate => candidate.name === name)
    if (entry !== undefined) return { entry, page }
    if (++pageIndex >= page.pageCount) {
      throw new DOMException('The replacement file is no longer shared at this path', 'NotFoundError')
    }
  }
}
