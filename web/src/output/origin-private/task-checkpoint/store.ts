import type { TaskCheckpoint, TaskEntry, TaskDirectoryPin } from './model'

export interface TaskCheckpointCommit {
  readonly expectedGeneration: bigint | undefined
  readonly checkpoint: TaskCheckpoint
  readonly entries: readonly TaskEntry[]
  readonly directoryPins?: readonly TaskDirectoryPin[]
}

/** ZIP payload ranges and encoding coordinates have one atomic recovery authority. */
export interface TaskCheckpointStore {
  readCheckpoint(): Promise<TaskCheckpoint | undefined>
  readEntry(entryId: string): Promise<TaskEntry | undefined>
  readDirectoryPin(directoryId: string): Promise<TaskDirectoryPin | undefined>
  readPath(path: readonly string[]): Promise<TaskEntry | undefined>
  readEntries(input: Readonly<{ afterSequence?: bigint; limit: number }>): Promise<readonly TaskEntry[]>
  commit(input: TaskCheckpointCommit): Promise<void>
  close(): void
}
