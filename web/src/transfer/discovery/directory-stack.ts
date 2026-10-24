import { FaultScope } from '../failure/fault'
import type { DirectoryCursor, DirectoryWork } from '../job/contract'
import { DirectoryOutputError, normalizeFileTransferFailure } from '../job/failures'

export interface DirectoryStackFrame {
  readonly work: DirectoryWork
  readonly discovery: AsyncGenerator<DirectoryWork, void>
}

/** Converts only child-scoped discovery failures into an isolated stack pop. */
export async function advanceDirectoryFrame(
  frame: DirectoryStackFrame,
  isolate: (directoryId: string, error: unknown) => void,
): Promise<IteratorResult<DirectoryWork, void> | undefined> {
  try {
    return await frame.discovery.next()
  } catch (error) {
    isolateDirectoryFailure(frame.work.cursor, error, isolate)
    return undefined
  }
}

export function isolateDirectoryFailure(
  cursor: DirectoryCursor,
  error: unknown,
  isolate: (directoryId: string, error: unknown) => void,
): void {
  const directoryId = error instanceof DirectoryOutputError && error.directoryId !== undefined
    ? error.directoryId
    : cursor.idText
  const normalized = normalizeFileTransferFailure(error)
  if (cursor.path.length === 0 || normalized.kind === 'canceled' ||
      normalized.fault.scope !== FaultScope.DirectoryLocal) {
    throw normalized.diagnostic
  }
  isolate(directoryId, normalized.diagnostic)
}
