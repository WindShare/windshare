import type {
  FSAMutationSchedulerDiagnosticsSnapshot,
  FSAMutationSchedulerState,
} from './model'

export class FSAMutationSchedulerDiagnostics {
  #state: FSAMutationSchedulerState = 'accepting'
  #activeWriters = 0
  #queuedWriters = 0
  #peakActiveWriters = 0
  #acquiredWriterLeases = 0
  #releasedWriterLeases = 0
  #activeNamespaceMutations = 0
  #queuedNamespaceMutations = 0
  #peakActiveNamespaceMutations = 0
  #startedNamespaceMutations = 0
  #completedNamespaceMutations = 0
  #failedNamespaceMutations = 0
  readonly maximumActiveWriters: number
  #terminalExclusiveRuns = 0
  #failedTerminalExclusiveRuns = 0

  constructor(maximumActiveWriters: number) {
    this.maximumActiveWriters = maximumActiveWriters
  }

  transition(state: FSAMutationSchedulerState): void {
    this.#state = state
  }

  writerQueued(): void {
    this.#queuedWriters += 1
  }

  writerAcquired(): void {
    this.#queuedWriters -= 1
    this.#activeWriters += 1
    this.#acquiredWriterLeases += 1
    this.#peakActiveWriters = Math.max(this.#peakActiveWriters, this.#activeWriters)
  }

  writerReleased(): void {
    this.#activeWriters -= 1
    this.#releasedWriterLeases += 1
  }

  namespaceQueued(): void {
    this.#queuedNamespaceMutations += 1
  }

  namespaceStarted(): void {
    this.#queuedNamespaceMutations -= 1
    this.#activeNamespaceMutations += 1
    this.#startedNamespaceMutations += 1
    this.#peakActiveNamespaceMutations = Math.max(
      this.#peakActiveNamespaceMutations,
      this.#activeNamespaceMutations,
    )
  }

  namespaceSettled(succeeded: boolean): void {
    this.#activeNamespaceMutations -= 1
    if (succeeded) {
      this.#completedNamespaceMutations += 1
    } else {
      this.#failedNamespaceMutations += 1
    }
  }

  terminalSettled(succeeded: boolean): void {
    this.#terminalExclusiveRuns += 1
    if (!succeeded) this.#failedTerminalExclusiveRuns += 1
  }

  snapshot(): FSAMutationSchedulerDiagnosticsSnapshot {
    return Object.freeze({
      state: this.#state,
      maximumActiveWriters: this.maximumActiveWriters,
      activeWriters: this.#activeWriters,
      queuedWriters: this.#queuedWriters,
      peakActiveWriters: this.#peakActiveWriters,
      acquiredWriterLeases: this.#acquiredWriterLeases,
      releasedWriterLeases: this.#releasedWriterLeases,
      activeNamespaceMutations: this.#activeNamespaceMutations,
      queuedNamespaceMutations: this.#queuedNamespaceMutations,
      peakActiveNamespaceMutations: this.#peakActiveNamespaceMutations,
      startedNamespaceMutations: this.#startedNamespaceMutations,
      completedNamespaceMutations: this.#completedNamespaceMutations,
      failedNamespaceMutations: this.#failedNamespaceMutations,
      terminalExclusiveRuns: this.#terminalExclusiveRuns,
      failedTerminalExclusiveRuns: this.#failedTerminalExclusiveRuns,
    })
  }
}
