import type { AcquiredOutputCapability } from '../capability/acquisition'
import type { TransferIntent } from '../../transfer/intent'
import {
  type BeginOutputFileResult,
  type DirectoryAdmission,
  type DirectorySettlement,
  JobSettlementKind,
  type JobSettlement,
  type OutputCapabilities,
  type OutputDirectoryAdmission,
  type OutputFile,
  type OutputSession,
  type OutputSessionIdentity,
} from '../../transfer/output-session'
import type { JobOutcome } from '../../transfer/outcome'
import type {
  PausedTaskDescriptorRepository,
} from '../resume/authority'
import type { PausedTaskDescriptorV1 } from '../resume/descriptor'
import { IndexedDbPausedTaskState } from './indexeddb-resume-state'

interface PausedTaskRepositorySession extends PausedTaskDescriptorRepository {
  close(): void
}

export type PausedTaskRepositoryFactory = () => Promise<PausedTaskRepositorySession>

export interface BrowserPausedTaskLifecycle {
  track(
    intent: TransferIntent,
    capability: AcquiredOutputCapability,
    session: OutputSession,
  ): Promise<OutputSession>
}

export class IndexedDbBrowserPausedTaskLifecycle implements BrowserPausedTaskLifecycle {
  readonly #openRepository: PausedTaskRepositoryFactory

  constructor(
    openRepository: PausedTaskRepositoryFactory = () => IndexedDbPausedTaskState.open(),
  ) {
    this.#openRepository = openRepository
  }

  async track(
    intent: TransferIntent,
    capability: AcquiredOutputCapability,
    session: OutputSession,
  ): Promise<OutputSession> {
    if (capability.kind !== 'PersistentDirectory' &&
        capability.kind !== 'OriginPrivateStaging') {
      return session
    }
    let repository: PausedTaskRepositorySession | undefined
    let descriptor: PausedTaskDescriptorV1
    try {
      repository = await this.#openRepository()
      descriptor = await repository.persist(intent, capability.root)
    } catch (error) {
      await pauseAfterDescriptorFailure(session, error)
      throw error
    } finally {
      repository?.close()
    }
    return new DescriptorTrackedOutputSession(
      session,
      descriptor,
      this.#openRepository,
    )
  }
}

class DescriptorTrackedOutputSession implements OutputSession {
  readonly identity: OutputSessionIdentity
  readonly format: OutputSession['format']
  readonly capabilities: OutputCapabilities
  readonly #inner: OutputSession
  readonly #descriptor: PausedTaskDescriptorV1
  readonly #openRepository: PausedTaskRepositoryFactory

  constructor(
    inner: OutputSession,
    descriptor: PausedTaskDescriptorV1,
    openRepository: PausedTaskRepositoryFactory,
  ) {
    this.#inner = inner
    this.#descriptor = descriptor
    this.#openRepository = openRepository
    this.identity = inner.identity
    this.format = inner.format
    this.capabilities = inner.capabilities
  }

  admitDirectory(
    directory: OutputDirectoryAdmission,
    signal: AbortSignal,
  ): Promise<DirectoryAdmission> {
    return this.#inner.admitDirectory(directory, signal)
  }

  finalizeDirectory(
    admission: DirectoryAdmission,
    signal: AbortSignal,
  ): Promise<DirectorySettlement> {
    return this.#inner.finalizeDirectory(admission, signal)
  }

  beginFile(file: OutputFile, signal: AbortSignal): Promise<BeginOutputFileResult> {
    return this.#inner.beginFile(file, signal)
  }

  async completeJob(outcome: JobOutcome, signal: AbortSignal): Promise<JobSettlement> {
    const settlement = await this.#inner.completeJob(outcome, signal)
    if (settlement.kind !== JobSettlementKind.Completed) return settlement
    const repository = await this.#openRepository()
    try {
      await repository.removeCompleted(this.#descriptor)
    } finally {
      repository.close()
    }
    return settlement
  }

  pauseJob(reason: unknown): Promise<JobSettlement> {
    return this.#inner.pauseJob(reason)
  }
}

async function pauseAfterDescriptorFailure(
  session: OutputSession,
  descriptorFailure: unknown,
): Promise<void> {
  try {
    await session.pauseJob(descriptorFailure)
  } catch (pauseFailure) {
    throw new AggregateError(
      [descriptorFailure, pauseFailure],
      'Paused-task persistence and output pause failed',
      { cause: pauseFailure },
    )
  }
}
