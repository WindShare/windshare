import { emitOutputTrace, type OutputTraceSource } from '../../output/diagnostics'
import { systemReconnectClock, type ReconnectClock } from '../../receiver/recovery-clock'
import type { TransferJob } from '../../transfer/job'
import type { JoinedBrowserShare } from '../browser-share/gateway'

export const DISCONNECT_CHECKPOINT_DELAY_MS = 30_000

interface DisconnectGuardOptions {
  readonly job: Pick<TransferJob, 'checkpointActive' | 'allReceivedSaved'>
  readonly taskId: string
  readonly changed: () => void
  readonly trace?: OutputTraceSource
  readonly clock?: Pick<ReconnectClock, 'sleep'>
}

export class DisconnectCheckpointGuard {
  readonly #options: DisconnectGuardOptions
  readonly #unsubscribe: () => void
  #round: AbortController | undefined
  #sequence = 0
  #checkpointed = false

  constructor(
    connection: Pick<JoinedBrowserShare, 'subscribeConnection'>,
    options: DisconnectGuardOptions,
  ) {
    this.#options = options
    this.#unsubscribe = connection.subscribeConnection(snapshot => {
      if (snapshot.kind !== 'reconnecting') {
        this.#endRound()
      } else if (this.#round === undefined) {
        this.#round = new AbortController()
        void this.#checkpoint(++this.#sequence, this.#round.signal)
      }
    })
  }

  get checkpointed(): boolean { return this.#checkpointed }

  invalidate(): void {
    if (!this.#checkpointed) return
    this.#checkpointed = false
    this.#options.changed()
  }

  close(): void {
    this.#unsubscribe()
    this.#endRound()
  }

  #endRound(): void {
    this.#round?.abort()
    this.#round = undefined
    this.invalidate()
  }

  async #checkpoint(round: number, signal: AbortSignal): Promise<void> {
    try { await (this.#options.clock ?? systemReconnectClock).sleep(DISCONNECT_CHECKPOINT_DELAY_MS, signal) } catch { return }
    if (signal.aborted) return
    const summary = await this.#options.job.checkpointActive(signal)
    // A write or reconnect can arrive between completing the traversal and publishing its result.
    const confirmed = !signal.aborted && summary.allReceivedSaved && this.#options.job.allReceivedSaved
    emitOutputTrace(this.#options.trace, 'checkpoint', {
      task_id: this.#options.taskId, round, trigger: 'disconnect_timeout', all_received_saved: confirmed,
      superseded: signal.aborted, saved_files: summary.savedFiles, skipped_files: summary.skippedFiles,
      failed_files: summary.failedFiles, saved_bytes: String(summary.savedBytes),
    })
    if (confirmed) {
      this.#checkpointed = true
      this.#options.changed()
    }
  }
}
