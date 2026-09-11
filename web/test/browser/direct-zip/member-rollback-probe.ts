import type { V2BoundReceiveOperation } from '../../../src/ui/v2-receive-runtime'
import {
  createMemberRollbackFixture, MEMBER_ROLLBACK_SIGNAL, readMemberRollbackState,
} from './member-rollback-fixture'
import type { MemberRollbackRevisionMode } from './member-rollback-source'

export async function probeProductionMemberRollback(databaseName: string, mode: MemberRollbackRevisionMode) {
  const fixture = await createMemberRollbackFixture(databaseName, mode)
  try { return await finishMemberRollbackFixture(fixture, await fixture.resume()) }
  finally { await fixture.close() }
}

export async function finishMemberRollbackFixture(
  fixture: Awaited<ReturnType<typeof createMemberRollbackFixture>>, active: V2BoundReceiveOperation,
) {
  const resumed = await active.plans.openDirectResumableZip(fixture.intent, MEMBER_ROLLBACK_SIGNAL)
  await fixture.source.run(resumed, 'resumed', MEMBER_ROLLBACK_SIGNAL)
  const lifecycle = await resumed.settle({
    transferJobId: active.transferJobId, worker: {} as never,
    materialization: resumed.ordered.materializationSummary(),
  }, MEMBER_ROLLBACK_SIGNAL)
  const archive = new Uint8Array(await (await fixture.file.getFile()).arrayBuffer())
  const completed = (await readMemberRollbackState(fixture.databaseName, fixture.intent.operationId)).checkpoint
  const { paused, pausedMember, source } = fixture
  return {
    lifecycle: lifecycle.kind, archive: Array.from(archive), expected: source.expected,
    prefixBefore: Array.from(fixture.prefix), prefixAfter: Array.from(archive.slice(0, fixture.rollbackOffset)),
    paused: { phase: paused.phase, ordinal: paused.entryOrdinal.toString(),
      safePayload: paused.committedSelectedPayloadBytes.toString(),
      memberOffset: pausedMember.memberPayloadOffset.toString(),
      completedPayload: pausedMember.rollback.safeSelectedPayloadBytes.toString() },
    completed: { safePayload: completed.committedSelectedPayloadBytes.toString() },
    opens: source.opens, ranges: source.ranges, initialDurable: source.initialDurable,
  }
}
