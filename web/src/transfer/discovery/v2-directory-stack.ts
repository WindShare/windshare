import type { DirectoryWork } from '../job/contract'
import { V2DirectoryOutputError, isDirectoryScopedFailure } from '../job/failures'

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
    if (frame.work.cursor.path.length === 0 || !isDirectoryScopedFailure(error)) throw error
    const directoryId = error instanceof V2DirectoryOutputError && error.directoryId !== undefined
      ? error.directoryId
      : frame.work.cursor.idText
    isolate(directoryId, error)
    return undefined
  }
}
