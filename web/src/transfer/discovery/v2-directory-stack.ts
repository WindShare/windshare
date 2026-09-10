import { FaultScope } from '../fault'
import type { DirectoryCursor, DirectoryWork } from '../job/contract'
import { V2DirectoryOutputError, normalizeV2FileTransferFailure } from '../job/failures'

export interface V2DirectoryStackFrame {
  readonly work: DirectoryWork
  readonly discovery: AsyncGenerator<DirectoryWork, void>
}

/** Converts only child-scoped discovery failures into an isolated stack pop. */
export async function advanceV2DirectoryFrame(
  frame: V2DirectoryStackFrame,
  isolate: (directoryId: string, error: unknown) => void,
): Promise<IteratorResult<DirectoryWork, void> | undefined> {
  try {
    return await frame.discovery.next()
  } catch (error) {
    isolateV2DirectoryFailure(frame.work.cursor, error, isolate)
    return undefined
  }
}

export function isolateV2DirectoryFailure(
  cursor: DirectoryCursor,
  error: unknown,
  isolate: (directoryId: string, error: unknown) => void,
): void {
  const directoryId = error instanceof V2DirectoryOutputError && error.directoryId !== undefined
    ? error.directoryId
    : cursor.idText
  const normalized = normalizeV2FileTransferFailure(error)
  if (cursor.path.length === 0 || normalized.kind === 'canceled' ||
      normalized.fault.scope !== FaultScope.DirectoryLocal) {
    throw normalized.diagnostic
  }
  isolate(directoryId, normalized.diagnostic)
}
