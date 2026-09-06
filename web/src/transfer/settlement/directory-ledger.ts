import type { IncidentScopeHandle } from '../../diagnostics/incident'
import {
  DirectorySettlementKind,
  type DirectoryAdmission,
} from '../directory-admission'
import { V2OutputPausedError } from '../job/contract'
import { finalizeV2Directories } from '../job/directory-transfer'
import { isolatedDirectoryOutputFailure } from '../job/failures'
import type {
  DirectTreeExecution,
  IncrementalDirectoryOutput,
  PlanExecution,
} from '../output-session'

export interface DirectorySettlementLedgerOptions {
  readonly retention: 'durable-at-admission' | 'after-content'
  readonly maximumAdmissions: number
  readonly recordFailure: (directoryId: string, reason: unknown) => void
  readonly incidentScope?: IncidentScopeHandle
}

export class DirectorySettlementLedger {
  readonly #options: DirectorySettlementLedgerOptions
  readonly #finalizable: DirectoryAdmission[] = []
  readonly #materializedPaths = new Set<string>()
  #admissionClaims = 0n
  #durableDirectoryCount = 0n

  constructor(options: DirectorySettlementLedgerOptions) {
    this.#options = options
  }

  get admissionClaims(): bigint { return this.#admissionClaims }

  get directoryCount(): bigint {
    return this.#options.retention === 'durable-at-admission'
      ? this.#durableDirectoryCount
      : BigInt(this.#materializedPaths.size)
  }

  reserveAdmission(): void {
    if (this.#options.retention === 'after-content' &&
        this.#admissionClaims >= BigInt(this.#options.maximumAdmissions)) {
      throw new V2OutputPausedError('Directory admission budget was exhausted')
    }
    this.#admissionClaims += 1n
  }

  async retain(
    output: IncrementalDirectoryOutput,
    admission: DirectoryAdmission,
    signal: AbortSignal,
  ): Promise<void> {
    if (this.#options.retention === 'durable-at-admission') {
      // ZIP topology is already durable, so only active traversal ancestry needs
      // receipts. Keeping a second ledger here would grow with the whole tree.
      const settlement = await output.finalizeDirectory(admission, signal)
      if (settlement.kind === DirectorySettlementKind.IsolatedFailure) {
        this.#options.recordFailure(admission.directoryId, settlement.fault)
      }
      if (admission.path.length > 0) this.#durableDirectoryCount++
      return
    }
    this.#finalizable.push(admission)
    if (admission.path.length > 0) this.#materializedPaths.add(admission.path.join('/'))
  }

  async finalize(
    output: IncrementalDirectoryOutput,
    signal: AbortSignal,
    fileFailureIsolation: boolean,
  ): Promise<void> {
    await finalizeV2Directories({
      admissions: this.#finalizable,
      output,
      signal,
      settled: (admission, settlement) => {
        if (settlement.kind === DirectorySettlementKind.IsolatedFailure) {
          this.#options.recordFailure(admission.directoryId, settlement.fault)
        }
      },
      failed: (admission, error) => {
        const isolated = admission.path.length === 0
          ? undefined
          : isolatedDirectoryOutputFailure(
              error,
              fileFailureIsolation,
              admission.directoryId,
              this.#options.incidentScope,
            )
        if (isolated === undefined) throw error
        this.#options.recordFailure(admission.directoryId, isolated)
      },
    })
  }
}

export function stabilizeDirectorySettlementLifecycle(
  execution: PlanExecution,
  directories: {
    readonly externalCancellationRequested: () => boolean
    readonly finalize: (signal: AbortSignal) => Promise<void>
  },
): PlanExecution {
  if (execution.planKind !== 'direct-tree') return execution
  const directExecution: DirectTreeExecution = execution
  return Object.freeze({
    ...directExecution,
    pause: async (
      request: Parameters<DirectTreeExecution['pause']>[0],
      signal: AbortSignal,
    ) => {
      if (directories.externalCancellationRequested()) {
        directExecution.beginTerminal('pause')
        await directories.finalize(signal)
      }
      return directExecution.pause(request, signal)
    },
    ...(directExecution.stop === undefined ? {} : {
      stop: async (
        request: Parameters<NonNullable<DirectTreeExecution['stop']>>[0],
        signal: AbortSignal,
      ) => {
        if (directories.externalCancellationRequested()) {
          directExecution.beginTerminal('stop')
          await directories.finalize(signal)
        }
        return directExecution.stop!(request, signal)
      },
    }),
  })
}
