import type { ReceiverSnapshot } from '../model'
import type { RetainedReceiveAction } from '../../application/receive/contracts'
import type { LifecycleUserAction } from '../../application/receive/actions'
import { activeTaskFacts, presentTask, retainedTaskFacts, type TaskFacts, type TaskPresentation } from '../tasks'
import { retainedContinuationReadiness } from '../../application/receive/retained/readiness'
import { isReceiveOutputDelivered } from '../../application/receive/completion'
import { taskConnectionBlocking } from './share-presentation'

export type TaskInputs = Pick<ReceiverSnapshot, 'output' | 'progress' | 'taskDisplay' | 'connection' |
  'share' | 'retained' | 'activeReceiveOperationId' | 'startAdmission'>
export type RetainedAdmission = (operation: ReceiverSnapshot['retained']['operations'][number],
  action: RetainedReceiveAction) => Readonly<{ allowed: boolean; reason: string | null }>
export type ActiveAdmission = (action: LifecycleUserAction) => Readonly<{ allowed: boolean; reason: string | null }>
export interface TaskComposition {
  readonly current: TaskPresentation | null
  readonly tasks: readonly TaskPresentation[]
  readonly history: readonly TaskPresentation[]
}

/** History and live byte counters have different invalidation boundaries. */
export class TaskComposer {
  #activeInputs: readonly unknown[] = []
  #historyInputs: readonly unknown[] = []
  #currentFacts: TaskFacts | null = null
  #active: TaskPresentation | null = null
  #retained: readonly TaskPresentation[] = []
  #result: TaskComposition = { current: null, tasks: [], history: [] }

  compose(snapshot: TaskInputs, admission?: RetainedAdmission, activeAdmission?: ActiveAdmission): TaskComposition {
    const blocking = taskConnectionBlocking(snapshot.connection)
    const activeInputs = [snapshot.output, snapshot.progress, snapshot.taskDisplay, blocking,
      snapshot.retained.pending, snapshot.startAdmission.allowed, snapshot.startAdmission.reason, activeAdmission]
    const activeChanged = !sameInputs(this.#activeInputs, activeInputs)
    if (activeChanged) {
      this.#activeInputs = activeInputs
      this.#currentFacts = activeTaskFacts({
        output: snapshot.output, progress: snapshot.progress, display: snapshot.taskDisplay, blocking,
        ...(activeAdmission === undefined ? {} : { actionAdmission: activeAdmission }),
      })
      this.#active = this.#currentFacts === null ? null : presentTask(this.#currentFacts)
    }
    const historyInputs = [snapshot.retained, snapshot.share?.shareInstance, blocking,
      snapshot.activeReceiveOperationId, snapshot.output.lifecycle, snapshot.output.activation,
      snapshot.output.activeControls, snapshot.output.receiveInterruption,
      snapshot.startAdmission.allowed, snapshot.startAdmission.reason, admission,
      snapshot.activeReceiveOperationId === null ? this.#currentFacts : null]
    const historyChanged = !sameInputs(this.#historyInputs, historyInputs)
    if (!activeChanged && !historyChanged) return this.#result
    if (historyChanged) {
      this.#historyInputs = historyInputs
      this.#retained = snapshot.retained.operations.map(operation => presentTask(withSettledTransferProgress(retainedTaskFacts(
        operation, retainedContinuationReadiness(operation, snapshot.share?.shareInstance ?? null), {
          pending: snapshot.retained.pending,
          blocking: snapshot.share !== null && operation.shareInstance === snapshot.share.shareInstance ? blocking : null,
          ...(admission === undefined ? {} : { admission: action => admission(operation, action) }),
        },
      ), this.#currentFacts, snapshot.activeReceiveOperationId)))
        .sort((left, right) => (right.createdAtMilliseconds ?? 0) - (left.createdAtMilliseconds ?? 0))
    }
    const current = chooseCurrentTask(this.#active, this.#retained, snapshot)
    const tasks = current === null ? this.#retained
      : Object.freeze([current, ...this.#retained.filter(task => task.operationId !== current.operationId)])
    this.#result = Object.freeze({ current, tasks, history: this.#retained })
    return this.#result
  }
}

function sameInputs(previous: readonly unknown[], next: readonly unknown[]): boolean {
  return previous.length === next.length && next.every((value, index) => value === previous[index])
}
export function composeTasks(snapshot: TaskInputs, admission?: RetainedAdmission, activeAdmission?: ActiveAdmission): TaskComposition {
  return new TaskComposer().compose(snapshot, admission, activeAdmission)
}

function withSettledTransferProgress(retained: TaskFacts, current: TaskFacts | null, activeOperationId: string | null): TaskFacts {
  if (current === null || current.progress === null || current.progress.transferJobId.length === 0 ||
      activeOperationId === current.lifecycle.operationId || !isReceiveOutputDelivered(current.lifecycle) ||
      current.lifecycle.operationId !== retained.lifecycle.operationId ||
      current.lifecycle.receiveIntentDigest !== retained.lifecycle.receiveIntentDigest ||
      current.lifecycle.generation !== retained.lifecycle.generation) return retained
  // Inventory owns current child storage facts. The settled transfer still owns
  // the count and size observations that portable history does not persist.
  return Object.freeze({ ...retained, progress: current.progress })
}

function chooseCurrentTask(active: TaskPresentation | null, history: readonly TaskPresentation[], snapshot: TaskInputs): TaskPresentation | null {
    let current = active
    const pendingLocal = history.find(task => task.operationId === snapshot.retained.pending?.operationId &&
      task.transition.reason === 'local-finalization-active')
    if (pendingLocal !== undefined && (current === null || current.operationId === pendingLocal.operationId)) current = pendingLocal
    if (current !== null && snapshot.activeReceiveOperationId !== current.operationId && pendingLocal === undefined) {
      const stored = history.find(task => task.operationId === current?.operationId)
      // Once detached, inventory owns current storage and action authority.
      if (stored !== undefined && stored.generation >= current.generation) current = stored
      else current = { ...current, primaryAction: null, secondaryActions: [], destructiveActions: [] }
    }
    return current
}
