import type { CatalogEntry } from '../../catalog/records'
import type { SourceRevisionFailure } from '../../transfer/state'
import type { BrowseDirectory, BrowsePage, JoinedBrowserShare } from './gateway'
import type { RetainedTaskSnapshot } from '../receive/contracts'
import { ReceiveBlockedError } from '../receive/single-file'

type ReplacementShare = Pick<JoinedBrowserShare, 'descriptor' | 'rootDirectory' | 'childDirectory' | 'page'>

export class ReplacementFileNotFoundError extends Error {
  readonly path: string

  constructor(path: string) {
    super('The replacement file is no longer shared at this path')
    this.name = 'ReplacementFileNotFoundError'
    this.path = path
  }
}

export async function findReplacementFile(
  joined: ReplacementShare,
  task: RetainedTaskSnapshot,
  failure: SourceRevisionFailure,
  signal: AbortSignal,
): Promise<Readonly<{ entry: CatalogEntry; page: BrowsePage; directories: readonly BrowseDirectory[] }>> {
  const failures = task.sourceRevisionFailures
  if (failures === undefined || !failures.files.includes(failure)) {
    throw new ReceiveBlockedError('actionUnavailable')
  }
  if (failures.shareInstance !== joined.descriptor.shareInstanceId) {
    throw new ReceiveBlockedError('matchingShare')
  }
  const displayPath = failure.path.join('/')
  const directories = [joined.rootDirectory()]
  let directory = directories[0]!
  for (let index = 0; index < failure.sourcePath.length; index++) {
    const found = await findNamedEntry(joined, directory, failure.sourcePath[index]!, displayPath, signal)
    if (index === failure.sourcePath.length - 1) {
      if (found.entry.kind !== 'file') break
      return Object.freeze({ ...found, directories: Object.freeze(directories) })
    }
    if (found.entry.kind !== 'directory') break
    directory = joined.childDirectory(directory, found.entry)
    directories.push(directory)
  }
  throw new ReplacementFileNotFoundError(displayPath)
}

async function findNamedEntry(
  joined: ReplacementShare,
  directory: BrowseDirectory,
  name: string,
  displayPath: string,
  signal: AbortSignal,
): Promise<Readonly<{ entry: CatalogEntry; page: BrowsePage }>> {
  let pageIndex = 0
  for (;;) {
    signal.throwIfAborted()
    const page = await joined.page(directory, pageIndex, { signal, explicitRetry: pageIndex === 0 })
    signal.throwIfAborted()
    const entry = page.entries.find(candidate => candidate.name === name)
    if (entry !== undefined) return { entry, page }
    if (++pageIndex >= page.pageCount) {
      throw new ReplacementFileNotFoundError(displayPath)
    }
  }
}
