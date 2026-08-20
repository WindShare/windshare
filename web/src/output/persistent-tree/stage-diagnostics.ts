import {
  PERSISTENT_OUTPUT_FAILURE_FACT_LIMITS,
  PersistentOutputFailureObservationBudget,
  mergePersistentOutputFailureFacts,
} from './stage-diagnostic-facts'
import type {
  PersistentOutputFailureFactProvider,
  PersistentOutputFailureFactProviderName,
  PersistentOutputFailureFacts,
  PersistentOutputFailureObservation,
  PersistentOutputRawException,
  PersistentOutputStage,
  PersistentOutputStageCorrelation,
  PersistentOutputStageDiagnostics,
  PersistentOutputStageMilestone,
  PersistentOutputStageTarget,
  PersistentOutputWriterFacts,
} from './stage-diagnostic-model'

export * from './stage-diagnostic-facts'
export * from './stage-diagnostic-model'
export * from './stage-diagnostic-projection'

const FAILURE_FACT_PROVIDER_LIMIT = 4

interface WriterEvidence {
  state: PersistentOutputWriterFacts['state']
  closeFailure?: unknown
}

interface FailureFactRegistry {
  readonly providers: Map<PersistentOutputFailureFactProviderName, PersistentOutputFailureFactProvider>
}

interface PersistentOutputStageRuntime {
  readonly diagnostics: PersistentOutputStageDiagnostics
  readonly fileEvidence: Map<number, WriterEvidence>
  sequence: number
  nextFileEvidenceId: number
}

export class PersistentOutputStageAuthority {
  readonly #runtime: PersistentOutputStageRuntime
  readonly #base: Readonly<{
    operationId: string
    outputSessionId: string
    artifactId: string
  }>

  constructor(
    diagnostics: PersistentOutputStageDiagnostics,
    correlation: Readonly<{ operationId: string; artifactId: string }>,
  ) {
    this.#runtime = {
      diagnostics,
      sequence: 0,
      nextFileEvidenceId: 0,
      fileEvidence: new Map(),
    }
    this.#base = Object.freeze({
      operationId: requireIdentity(correlation.operationId, 'operation'),
      outputSessionId: requireIdentity(diagnostics.outputSessionId, 'output session'),
      artifactId: requireIdentity(correlation.artifactId, 'artifact'),
    })
  }

  bindingScope(): PersistentOutputStageScope {
    return this.#scope('binding', this.#base.artifactId, [])
  }

  rootScope(): PersistentOutputStageScope {
    return this.#scope('root', this.#base.artifactId, [])
  }

  directoryScope(
    artifactPath: readonly string[],
    ownedObjectId?: string,
  ): PersistentOutputStageScope {
    return this.#scope('directory', this.#base.artifactId, artifactPath, ownedObjectId)
  }

  fileScope(
    artifactId: string,
    artifactPath: readonly string[],
  ): PersistentOutputStageScope {
    this.#runtime.nextFileEvidenceId += 1
    const evidenceId = this.#runtime.nextFileEvidenceId
    this.#runtime.fileEvidence.set(evidenceId, { state: 'not-created' })
    return this.#scope(
      'file',
      requireIdentity(artifactId, 'artifact'),
      artifactPath,
      undefined,
      evidenceId,
    )
  }

  retireAllFileEvidence(): void {
    this.#runtime.fileEvidence.clear()
  }

  #scope(
    target: PersistentOutputStageTarget,
    artifactId: string,
    artifactPath: readonly string[],
    ownedObjectId?: string,
    fileEvidenceId?: number,
  ): PersistentOutputStageScope {
    return new PersistentOutputStageScope(
      this.#runtime,
      {
        ...this.#base,
        target,
        artifactId,
        artifactPath,
        ...(ownedObjectId === undefined ? {} : { ownedObjectId }),
      },
      { providers: new Map() },
      fileEvidenceId,
    )
  }
}

/**
 * Passed only to deep persistence modules that own a native operation. Absent
 * diagnostics bypass this object entirely and invoke the native operation directly.
 */
export class PersistentOutputStageScope {
  readonly #runtime: PersistentOutputStageRuntime
  readonly #correlation: PersistentOutputStageCorrelation
  readonly #facts: FailureFactRegistry
  readonly #fileEvidenceId: number | undefined

  constructor(
    runtime: PersistentOutputStageRuntime,
    correlation: PersistentOutputStageCorrelation,
    facts: FailureFactRegistry,
    fileEvidenceId?: number,
  ) {
    this.#runtime = runtime
    this.#correlation = snapshotCorrelation(correlation)
    this.#facts = facts
    this.#fileEvidenceId = fileEvidenceId
  }

  withCorrelation(
    details: Partial<Pick<
      PersistentOutputStageCorrelation,
      'ownedObjectId' | 'checkpointRecordId' | 'checkpointGeneration'
    >>,
  ): PersistentOutputStageScope {
    return new PersistentOutputStageScope(
      this.#runtime,
      { ...this.#correlation, ...details },
      this.#facts,
      this.#fileEvidenceId,
    )
  }

  addFailureFacts(
    name: PersistentOutputFailureFactProviderName,
    provider: PersistentOutputFailureFactProvider,
  ): void {
    if (!this.#facts.providers.has(name) &&
        this.#facts.providers.size >= FAILURE_FACT_PROVIDER_LIMIT) {
      throw new TypeError('Persistent output failure-fact provider limit exceeded')
    }
    this.#facts.providers.set(name, provider)
  }

  recordWriterOpened(): void {
    const evidence = this.#writerEvidence()
    if (evidence !== undefined) evidence.state = 'open'
  }

  recordWriterClosed(): void {
    const evidence = this.#writerEvidence()
    if (evidence === undefined) return
    evidence.state = 'closed'
    evidence.closeFailure = undefined
  }

  recordWriterCloseFailure(error: unknown): void {
    const evidence = this.#writerEvidence()
    if (evidence === undefined) return
    evidence.state = 'close-failed'
    evidence.closeFailure = error
  }

  writerFacts(
    context: Parameters<PersistentOutputFailureFactProvider>[0],
  ): PersistentOutputWriterFacts | undefined {
    const evidence = this.#writerEvidence()
    if (evidence === undefined) return undefined
    return evidence.state === 'close-failed'
      ? Object.freeze({
          state: evidence.state,
          closeFailure: context.exception(evidence.closeFailure),
        })
      : Object.freeze({ state: evidence.state })
  }

  retireFileEvidence(): void {
    if (this.#fileEvidenceId !== undefined) {
      this.#runtime.fileEvidence.delete(this.#fileEvidenceId)
    }
  }

  async run<Value>(
    stage: PersistentOutputStage,
    operation: () => Promise<Value>,
  ): Promise<Value> {
    this.#emit(Object.freeze({
      sequence: this.#nextSequence(),
      transition: 'started',
      stage,
      correlation: this.#correlation,
    }))
    try {
      await this.#runtime.diagnostics.beforeStage?.(stage, this.#correlation)
      const value = await operation()
      this.#emit(Object.freeze({
        sequence: this.#nextSequence(),
        transition: 'completed',
        stage,
        correlation: this.#correlation,
      }))
      return value
    } catch (error) {
      const budget = new PersistentOutputFailureObservationBudget()
      const exception = budget.exception(error)
      const facts = await this.#advisoryFailureFacts(budget)
      this.#emit(Object.freeze({
        sequence: this.#nextSequence(),
        transition: 'failed',
        stage,
        correlation: this.#correlation,
        exception,
        facts,
      }))
      throw error
    }
  }

  async #advisoryFailureFacts(
    budget: PersistentOutputFailureObservationBudget,
  ): Promise<PersistentOutputFailureFacts> {
    try {
      return await this.#captureFailureFacts(budget)
    } catch (factFailure) {
      const probeFailure = budget.exception(factFailure)
      const provider = this.#facts.providers.keys().next().value ??
        ('binding' as PersistentOutputFailureFactProviderName)
      return Object.freeze({
        probeFailures: Object.freeze([probeFailure]),
        observation: budget.observation(
          this.#facts.providers.size,
          0,
          this.#runtime.fileEvidence.size,
          [Object.freeze({
            provider,
            reason: 'rejected' as const,
            exception: probeFailure,
          })],
        ),
      })
    }
  }

  async #captureFailureFacts(
    budget: PersistentOutputFailureObservationBudget,
  ): Promise<PersistentOutputFailureFacts> {
    const entries = [...this.#facts.providers.entries()]
    const outcomes = new Map<
      PersistentOutputFailureFactProviderName,
      | Readonly<{ kind: 'fulfilled'; facts: Omit<PersistentOutputFailureFacts, 'observation'> }>
      | Readonly<{ kind: 'rejected'; exception: PersistentOutputRawException }>
    >()
    let deadlineHandle: ReturnType<typeof setTimeout> | undefined
    const deadline = new Promise<'timeout'>((resolve) => {
      deadlineHandle = setTimeout(() => {
        budget.timeout()
        resolve('timeout')
      }, PERSISTENT_OUTPUT_FAILURE_FACT_LIMITS.deadlineMilliseconds)
    })
    const captures = entries.map(async ([name, provider]) => {
      try {
        outcomes.set(name, Object.freeze({
          kind: 'fulfilled',
          facts: await provider(budget),
        }))
      } catch (error) {
        outcomes.set(name, Object.freeze({
          kind: 'rejected',
          exception: budget.exception(error),
        }))
      }
    })
    const completed = Promise.all(captures).then(() => 'completed' as const)
    const cut = await Promise.race([completed, deadline])
    if (cut === 'completed' && deadlineHandle !== undefined) clearTimeout(deadlineHandle)

    let merged: Omit<PersistentOutputFailureFacts, 'observation'> = Object.freeze({})
    const probeFailures: PersistentOutputRawException[] = []
    const unavailable: Array<PersistentOutputFailureObservation['unavailableProviders'][number]> = []
    let completedProviderCount = 0
    for (const [name] of entries) {
      const outcome = outcomes.get(name)
      if (outcome === undefined) {
        unavailable.push(Object.freeze({ provider: name, reason: 'timeout' as const }))
        continue
      }
      completedProviderCount += 1
      if (outcome.kind === 'fulfilled') {
        merged = mergePersistentOutputFailureFacts(merged, outcome.facts)
      } else {
        probeFailures.push(outcome.exception)
        unavailable.push(Object.freeze({
          provider: name,
          reason: 'rejected' as const,
          exception: outcome.exception,
        }))
      }
    }
    return Object.freeze({
      ...merged,
      ...(probeFailures.length === 0
        ? {}
        : { probeFailures: Object.freeze(probeFailures) }),
      observation: budget.observation(
        entries.length,
        completedProviderCount,
        this.#runtime.fileEvidence.size,
        unavailable,
      ),
    })
  }

  #writerEvidence(): WriterEvidence | undefined {
    return this.#fileEvidenceId === undefined
      ? undefined
      : this.#runtime.fileEvidence.get(this.#fileEvidenceId)
  }

  #emit(milestone: PersistentOutputStageMilestone): void {
    try {
      this.#runtime.diagnostics.observe(milestone)
    } catch {
      // Native persistence and the original error never depend on local observation.
    }
  }

  #nextSequence(): number {
    this.#runtime.sequence += 1
    return this.#runtime.sequence
  }
}

export function createPersistentOutputStageAuthority(
  diagnostics: PersistentOutputStageDiagnostics | undefined,
  correlation: Readonly<{ operationId: string; artifactId: string }>,
): PersistentOutputStageAuthority | undefined {
  return diagnostics === undefined
    ? undefined
    : new PersistentOutputStageAuthority(diagnostics, correlation)
}

export function runPersistentOutputStage<Value>(
  scope: PersistentOutputStageScope | undefined,
  stage: PersistentOutputStage,
  operation: () => Promise<Value>,
): Promise<Value> {
  return scope === undefined ? operation() : scope.run(stage, operation)
}

function snapshotCorrelation(
  correlation: PersistentOutputStageCorrelation,
): PersistentOutputStageCorrelation {
  return Object.freeze({
    operationId: requireIdentity(correlation.operationId, 'operation'),
    outputSessionId: requireIdentity(correlation.outputSessionId, 'output session'),
    target: correlation.target,
    artifactId: requireIdentity(correlation.artifactId, 'artifact'),
    artifactPath: Object.freeze([...correlation.artifactPath]),
    ...(correlation.ownedObjectId === undefined
      ? {}
      : { ownedObjectId: requireIdentity(correlation.ownedObjectId, 'owned object') }),
    ...(correlation.checkpointRecordId === undefined
      ? {}
      : {
          checkpointRecordId: requireIdentity(
            correlation.checkpointRecordId,
            'checkpoint record',
          ),
        }),
    ...(correlation.checkpointGeneration === undefined
      ? {}
      : { checkpointGeneration: correlation.checkpointGeneration }),
  })
}

function requireIdentity(value: string, label: string): string {
  if (typeof value !== 'string' || value.length === 0) {
    throw new TypeError(`${label} diagnostic identity is empty`)
  }
  return value
}
