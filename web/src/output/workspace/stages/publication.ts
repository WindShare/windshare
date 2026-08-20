import { BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS } from '../../../transfer/intent'
import {
  validateDestinationReservation,
  type AtomicTargetReservation,
} from '../../../transfer/intent'
import {
  createPublicationAttempt,
  type PackagedArtifactV1,
  type PublicationAttemptV1,
} from '../aggregate'
import {
  createExpiryReceipt,
  createHandoffReceipt,
  createManagedPublicationReceipt,
  persistedReceiptRecord,
  type HandoffReceiptV1,
  type ManagedPublicationReceiptV1,
} from '../receipts'
import {
  createPersistedReceiveRecord,
  RECEIVE_RECORD_PUBLICATION_ATTEMPT,
  RECEIVE_RECORD_RESERVATION,
  type ReceiveOperationHandleRecord,
} from '../records'
import type { ExternalAttemptReason, ReceiveLifecycleState } from '../state'
import {
  WORKSPACE_HANDLE_PUBLICATION_TARGET,
  checkedLeaseEnd,
} from './contracts'
import { WorkspaceStageRuntime } from './runtime'

export class WorkspacePublicationStages {
  readonly runtime: WorkspaceStageRuntime

  constructor(runtime: WorkspaceStageRuntime) {
    this.runtime = runtime
  }

  async startManagedPublication(input: {
    readonly package: PackagedArtifactV1
    readonly publicationAttemptId: string
    readonly reservation: AtomicTargetReservation
    readonly targetHandle: ReceiveOperationHandleRecord
  }): Promise<PublicationAttemptV1> {
    const state = await this.runtime.lifecycle()
    this.runtime.requireContinuationUnexpired(state)
    const reservation = await validateDestinationReservation(
      input.reservation,
      this.runtime.intent.artifact,
    )
    if (reservation.kind !== 'atomic-target' || reservation.operationId !== this.runtime.intent.operationId ||
        reservation.artifactDigest !== this.runtime.intent.artifact.digest ||
        reservation.guarantees.profile !== 'managed-atomic' ||
        input.targetHandle.kind !== WORKSPACE_HANDLE_PUBLICATION_TARGET ||
        input.targetHandle.authorityRef !== reservation.authorityRef) {
      throw new TypeError('managed publication target is not an atomic package authority')
    }
    this.#assertRetainedPackage(state, input.package)
    const attempt = await createPublicationAttempt({
      publicationAttemptId: input.publicationAttemptId,
      operationId: this.runtime.intent.operationId,
      receiveIntentDigest: this.runtime.intent.digest,
      packagedArtifactDigest: input.package.digest,
      route: Object.freeze({ kind: 'managed', reservationDigest: reservation.digest }),
    })
    const next = this.runtime.reduce(state, this.runtime.event({
      kind: 'save-requested',
      publicationAttemptId: attempt.publicationAttemptId,
    }, state))
    const records = await Promise.all([
      createPersistedReceiveRecord({
        operationId: this.runtime.intent.operationId,
        kind: RECEIVE_RECORD_RESERVATION,
        canonicalBytes: reservation.canonicalBytes,
      }),
      createPersistedReceiveRecord({
        operationId: this.runtime.intent.operationId,
        kind: RECEIVE_RECORD_PUBLICATION_ATTEMPT,
        canonicalBytes: attempt.canonicalBytes,
      }),
    ])
    await this.runtime.repository.commitTransition({
      operationId: this.runtime.intent.operationId,
      expectedLifecycleGeneration: state.generation,
      expectedLeaseId: this.runtime.leaseId,
      records,
      handles: [input.targetHandle],
      lifecycle: next,
    })
    this.runtime.emit({
      name: 'receive.publication.started',
      operation_id: this.runtime.intent.operationId,
      package_digest: input.package.digest,
      publication_attempt_id: attempt.publicationAttemptId,
      publication_route: 'managed',
    })
    return attempt
  }

  async recordManagedPublicationCommitted(input: {
    readonly package: PackagedArtifactV1
    readonly attempt: PublicationAttemptV1
    readonly targetAuthorityRef: string
  }): Promise<Readonly<{
    receipt: ManagedPublicationReceiptV1
    state: Extract<ReceiveLifecycleState, { kind: 'published' }>
  }>> {
    const state = await this.runtime.lifecycle()
    if (state.kind !== 'publishing-managed' ||
        state.publicationAttemptId !== input.attempt.publicationAttemptId ||
        state.packageDigest !== input.package.digest || input.attempt.route.kind !== 'managed') {
      throw new TypeError('publication commit proof escaped its active attempt')
    }
    const receipt = await createManagedPublicationReceipt({
      operationId: this.runtime.intent.operationId,
      receiveIntentDigest: this.runtime.intent.digest,
      publicationAttemptId: input.attempt.publicationAttemptId,
      packagedArtifactDigest: input.package.digest,
      reservationDigest: input.attempt.route.reservationDigest,
      targetAuthorityRef: input.targetAuthorityRef,
      exactBytes: input.package.exactBytes,
      commitConfirmed: true,
    })
    const next = this.runtime.reduce(state, this.runtime.event({
      kind: 'publication-committed',
      receiptDigest: receipt.digest,
    }, state))
    if (next.kind !== 'published') throw new TypeError('confirmed publication did not become Published')
    await this.runtime.repository.commitTransition({
      operationId: this.runtime.intent.operationId,
      expectedLifecycleGeneration: state.generation,
      expectedLeaseId: this.runtime.leaseId,
      records: [await persistedReceiptRecord(receipt)],
      lifecycle: next,
    })
    this.runtime.emit({
      name: 'receive.publication.committed',
      operation_id: this.runtime.intent.operationId,
      package_digest: input.package.digest,
      publication_attempt_id: input.attempt.publicationAttemptId,
      artifact_bytes: input.package.exactBytes,
    })
    return Object.freeze({ receipt, state: next })
  }

  async recordManagedPublicationNotCommitted(input: {
    readonly package: PackagedArtifactV1
    readonly attempt: PublicationAttemptV1
    readonly reason: ExternalAttemptReason
  }): Promise<Extract<ReceiveLifecycleState, { kind: 'waiting-to-save' }>> {
    const state = await this.runtime.lifecycle()
    this.#assertActiveManagedAttempt(state, input.package, input.attempt)
    const next = this.runtime.reduce(state, this.runtime.event({
      kind: 'publication-not-committed',
      reason: input.reason,
    }, state))
    if (next.kind !== 'waiting-to-save') throw new TypeError('non-commit did not restore WaitingToSave')
    await this.runtime.commitLifecycle(state, next)
    this.runtime.emit({
      name: 'receive.publication.not_committed',
      operation_id: this.runtime.intent.operationId,
      package_digest: input.package.digest,
      publication_attempt_id: input.attempt.publicationAttemptId,
      external_attempt_reason: input.reason,
    })
    return next
  }

  async recordManagedPublicationUnknown(input: {
    readonly package: PackagedArtifactV1
    readonly attempt: PublicationAttemptV1
    readonly lastVerifiedRecordDigest: string
  }): Promise<Extract<ReceiveLifecycleState, { kind: 'needs-attention' }>> {
    const state = await this.runtime.lifecycle()
    this.#assertActiveManagedAttempt(state, input.package, input.attempt)
    const next = this.runtime.reduce(state, this.runtime.event({
      kind: 'publication-unknown',
      lastVerifiedRecordDigest: input.lastVerifiedRecordDigest,
    }, state))
    if (next.kind !== 'needs-attention') throw new TypeError('unknown publication was retried')
    await this.runtime.commitLifecycle(state, next)
    this.runtime.emit({
      name: 'receive.publication.unknown',
      operation_id: this.runtime.intent.operationId,
      package_digest: input.package.digest,
      publication_attempt_id: input.attempt.publicationAttemptId,
      needs_attention_reason: 'publication-unknown',
    })
    this.runtime.emit({
      name: 'receive.operation.needs_attention',
      operation_id: this.runtime.intent.operationId,
      prior_state: state.kind,
      needs_attention_reason: 'publication-unknown',
    })
    return next
  }

  async recordTargetOwnershipUnknown(
    lastVerifiedRecordDigest: string,
  ): Promise<Extract<ReceiveLifecycleState, { kind: 'needs-attention' }>> {
    const state = await this.runtime.lifecycle()
    const next = this.runtime.reduce(state, this.runtime.event({
      kind: 'ownership-unknown',
      lastVerifiedRecordDigest,
    }, state))
    if (next.kind !== 'needs-attention') {
      throw new TypeError('unknown target ownership did not become NeedsAttention')
    }
    await this.runtime.commitLifecycle(state, next)
    this.runtime.emit({
      name: 'receive.operation.needs_attention',
      operation_id: this.runtime.intent.operationId,
      prior_state: state.kind,
      needs_attention_reason: 'target-ownership-unknown',
    })
    return next
  }

  async startHandoff(input: {
    readonly package: PackagedArtifactV1
    readonly publicationAttemptId: string
    readonly suggestedName: string
    readonly packagedFileSupported: boolean
  }): Promise<PublicationAttemptV1> {
    const state = await this.runtime.lifecycle()
    this.runtime.requireContinuationUnexpired(state)
    this.#assertRetainedPackage(state, input.package)
    const attempt = await createPublicationAttempt({
      publicationAttemptId: input.publicationAttemptId,
      operationId: this.runtime.intent.operationId,
      receiveIntentDigest: this.runtime.intent.digest,
      packagedArtifactDigest: input.package.digest,
      route: Object.freeze({
        kind: 'handoff',
        suggestedName: input.suggestedName,
        packagedFileSupported: input.packagedFileSupported,
        objectUrlLeaseMilliseconds: BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
      }),
    })
    const next = this.runtime.reduce(state, this.runtime.event({
      kind: 'handoff-requested',
      attemptKind: 'workspace',
      attemptId: attempt.publicationAttemptId,
    }, state))
    await this.runtime.repository.commitTransition({
      operationId: this.runtime.intent.operationId,
      expectedLifecycleGeneration: state.generation,
      expectedLeaseId: this.runtime.leaseId,
      records: [await createPersistedReceiveRecord({
        operationId: this.runtime.intent.operationId,
        kind: RECEIVE_RECORD_PUBLICATION_ATTEMPT,
        canonicalBytes: attempt.canonicalBytes,
      })],
      lifecycle: next,
    })
    this.runtime.emit({
      name: 'receive.publication.started',
      operation_id: this.runtime.intent.operationId,
      package_digest: input.package.digest,
      publication_attempt_id: attempt.publicationAttemptId,
      publication_route: 'handoff',
    })
    this.runtime.emit({
      name: 'receive.handoff.started',
      operation_id: this.runtime.intent.operationId,
      attempt_kind: 'workspace',
      attempt_id: attempt.publicationAttemptId,
      package_digest_present: true,
      package_digest: input.package.digest,
      object_url_lease_ms: BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
    })
    return attempt
  }

  async recordHandoffStarted(input: {
    readonly package: PackagedArtifactV1
    readonly attempt: PublicationAttemptV1
    readonly urlLeaseStartedAt: number
    readonly urlLeaseEndsAt: number
  }): Promise<Readonly<{
    receipt: HandoffReceiptV1
    state: Extract<ReceiveLifecycleState, { kind: 'download-started' }>
  }>> {
    const state = await this.runtime.lifecycle()
    if (state.kind !== 'handing-off' || state.attemptKind !== 'workspace' ||
        state.attemptId !== input.attempt.publicationAttemptId ||
        state.packageDigest !== input.package.digest || input.attempt.route.kind !== 'handoff') {
      throw new TypeError('handoff start proof escaped its active attempt')
    }
    const now = this.runtime.now()
    if (input.urlLeaseStartedAt > now ||
        input.urlLeaseEndsAt !== checkedLeaseEnd(input.urlLeaseStartedAt)) {
      throw new TypeError('handoff URL lease does not use the exact finite duration')
    }
    const receipt = await createHandoffReceipt({
      operationId: this.runtime.intent.operationId,
      receiveIntentDigest: this.runtime.intent.digest,
      publicationAttemptId: input.attempt.publicationAttemptId,
      packagedArtifactDigest: input.package.digest,
      suggestedName: input.attempt.route.suggestedName,
      urlLeaseEndsAt: input.urlLeaseEndsAt,
      handoffStarted: true,
    })
    const next = this.runtime.reduce(state, this.runtime.event({ kind: 'handoff-started' }, state))
    if (next.kind !== 'download-started' || next.attemptKind !== 'workspace') {
      throw new TypeError('started handoff lacks retry state')
    }
    await this.runtime.repository.commitTransition({
      operationId: this.runtime.intent.operationId,
      expectedLifecycleGeneration: state.generation,
      expectedLeaseId: this.runtime.leaseId,
      records: [await persistedReceiptRecord(receipt)],
      lifecycle: next,
    })
    this.runtime.emit({
      name: 'receive.handoff.download_started',
      operation_id: this.runtime.intent.operationId,
      attempt_kind: 'workspace',
      attempt_id: input.attempt.publicationAttemptId,
      package_digest_present: true,
      package_digest: input.package.digest,
      retryable_until_present: true,
      retryable_until_ms: next.retryableUntil,
    })
    return Object.freeze({ receipt, state: next })
  }

  async recordHandoffNotStarted(input: {
    readonly package: PackagedArtifactV1
    readonly attempt: PublicationAttemptV1
    readonly reason: ExternalAttemptReason
  }): Promise<ReceiveLifecycleState> {
    const state = await this.runtime.lifecycle()
    this.#assertActiveHandoff(state, input.package, input.attempt)
    const now = this.runtime.now()
    const expired = now >= state.retainedDeadline
    const expiryReceipt = expired
      ? await createExpiryReceipt({
          operationId: this.runtime.intent.operationId,
          receiveIntentDigest: this.runtime.intent.digest,
          priorStableState: 'waiting-to-save',
          expiresAt: state.retainedDeadline,
          cleanupState: 'cleanup-pending',
        })
      : undefined
    const next = this.runtime.reduceAt(state, this.runtime.event({
      kind: 'handoff-not-started',
      reason: input.reason,
      ...(expiryReceipt === undefined ? {} : { expiryReceiptDigest: expiryReceipt.digest }),
    }, state), now)
    await this.runtime.repository.commitTransition({
      operationId: this.runtime.intent.operationId,
      expectedLifecycleGeneration: state.generation,
      expectedLeaseId: this.runtime.leaseId,
      ...(expiryReceipt === undefined ? {} : { records: [await persistedReceiptRecord(expiryReceipt)] }),
      lifecycle: next,
    })
    this.runtime.emit({
      name: 'receive.handoff.not_started',
      operation_id: this.runtime.intent.operationId,
      attempt_kind: 'workspace',
      attempt_id: input.attempt.publicationAttemptId,
      external_attempt_reason: input.reason,
    })
    if (next.kind === 'expired') {
      this.runtime.emit({
        name: 'receive.operation.expired',
        operation_id: this.runtime.intent.operationId,
        // This branch expires the waiting-to-save predecessor created above; using
        // that proof avoids widening publication to unrelated retained states.
        prior_stable_state: 'waiting-to-save',
        expires_at_ms: next.expiresAt,
      })
    }
    return next
  }

  async recordHandoffUnknown(input: {
    readonly package: PackagedArtifactV1
    readonly attempt: PublicationAttemptV1
    readonly lastVerifiedRecordDigest: string
  }): Promise<Extract<ReceiveLifecycleState, { kind: 'needs-attention' }>> {
    const state = await this.runtime.lifecycle()
    this.#assertActiveHandoff(state, input.package, input.attempt)
    const next = this.runtime.reduce(state, this.runtime.event({
      kind: 'handoff-unknown',
      lastVerifiedRecordDigest: input.lastVerifiedRecordDigest,
    }, state))
    if (next.kind !== 'needs-attention') throw new TypeError('unknown handoff was retried')
    await this.runtime.commitLifecycle(state, next)
    this.runtime.emit({
      name: 'receive.handoff.unknown',
      operation_id: this.runtime.intent.operationId,
      attempt_kind: 'workspace',
      attempt_id: input.attempt.publicationAttemptId,
      needs_attention_reason: 'publication-unknown',
    })
    this.runtime.emit({
      name: 'receive.operation.needs_attention',
      operation_id: this.runtime.intent.operationId,
      prior_state: state.kind,
      needs_attention_reason: 'publication-unknown',
    })
    return next
  }



  #assertRetainedPackage(state: ReceiveLifecycleState, packaged: PackagedArtifactV1): void {
    if (packaged.operationId !== this.runtime.intent.operationId ||
        packaged.receiveIntentDigest !== this.runtime.intent.digest ||
        ((state.kind !== 'waiting-to-save' || state.packageDigest !== packaged.digest) &&
         (state.kind !== 'download-started' || state.attemptKind !== 'workspace' ||
          state.packageDigest !== packaged.digest))) {
      throw new TypeError('publication package is not the retained workspace artifact')
    }
  }

  #assertActiveManagedAttempt(
    state: ReceiveLifecycleState,
    packaged: PackagedArtifactV1,
    attempt: PublicationAttemptV1,
  ): asserts state is Extract<ReceiveLifecycleState, { kind: 'publishing-managed' }> {
    if (state.kind !== 'publishing-managed' ||
        state.publicationAttemptId !== attempt.publicationAttemptId ||
        state.packageDigest !== packaged.digest || attempt.route.kind !== 'managed') {
      throw new TypeError('managed publication observation escaped its active attempt')
    }
  }

  #assertActiveHandoff(
    state: ReceiveLifecycleState,
    packaged: PackagedArtifactV1,
    attempt: PublicationAttemptV1,
  ): asserts state is Extract<
    ReceiveLifecycleState,
    { kind: 'handing-off'; attemptKind: 'workspace' }
  > {
    if (state.kind !== 'handing-off' || state.attemptKind !== 'workspace' ||
        state.attemptId !== attempt.publicationAttemptId ||
        state.packageDigest !== packaged.digest || attempt.route.kind !== 'handoff') {
      throw new TypeError('handoff observation escaped its active attempt')
    }
  }


}
