import { reduceReceiveLifecycle, type LifecycleEvent } from '../../../transfer/lifecycle/reducer'
import type { ReceiveOperationRepository } from '../repository'
import { decodeStoredReceiveLifecycleState } from '../state-codec'
import type { ReceiveLifecycleState } from '../../../transfer/lifecycle/state'
import type { ReceiveIntent } from '../../../transfer/intent'

type LifecycleEventPayload = LifecycleEvent extends infer Event
  ? Event extends LifecycleEvent
    ? Omit<Event, 'expectedGeneration' | 'leaseId'>
    : never
  : never

export async function transitionLifecycle(
  repository: ReceiveOperationRepository,
  intent: ReceiveIntent,
  leaseId: string,
  payload: LifecycleEventPayload,
  expected?: ReceiveLifecycleState,
): Promise<ReceiveLifecycleState> {
  const current = await readLifecycle(repository, intent.operationId)
  if (expected !== undefined && current.generation !== expected.generation) {
    throw new DOMException('Receive lifecycle changed before the requested transition', 'InvalidStateError')
  }
  const reduction = reduceReceiveLifecycle(current, {
    ...payload,
    expectedGeneration: current.generation,
    leaseId,
  } as LifecycleEvent, {
    planKind: intent.plan.kind,
    preparationRequired: intent.plan.preparation !== 'none',
    activeLeaseId: leaseId,
  })
  if (reduction.status !== 'applied') {
    throw new DOMException('Receive lifecycle transition was stale', 'InvalidStateError')
  }
  await repository.commitTransition({
    operationId: intent.operationId,
    expectedLifecycleGeneration: current.generation,
    expectedLeaseId: leaseId,
    lifecycle: reduction.state,
  })
  return reduction.state
}

export async function readLifecycle(
  repository: ReceiveOperationRepository,
  operationId: string,
): Promise<ReceiveLifecycleState> {
  const record = await repository.readLifecycle(operationId)
  if (record === undefined) throw new TypeError('Receive operation has no lifecycle authority')
  return decodeStoredReceiveLifecycleState(record)
}
