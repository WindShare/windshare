import type { ReceiveAction, ReceiveAdmission, RetainedTaskSnapshot } from '../../application/receive/contracts'
import { retainedTaskReadiness } from '../../application/receive/admission'
import { isDeliveredReceiveState } from '../../transfer/state'
import type { ReceiverSnapshot } from '../model'
import { activeTaskFacts, presentTask, retainedTaskFacts, type TaskFacts, type TaskPresentation } from '../tasks'
import { taskConnectionBlocking } from './share-presentation'

export type TaskInputs = Pick<
  ReceiverSnapshot,
  'output' | 'progress' | 'taskDisplay' | 'connection' | 'share' | 'retained' | 'activeReceiveOperationId' | 'startAdmission'
>

export type RetainedAdmission = (task: RetainedTaskSnapshot, action: ReceiveAction) => ReceiveAdmission
export type ActiveAdmission = (action: ReceiveAction) => ReceiveAdmission

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
    const { language } = snapshot.output
    const blocking = taskConnectionBlocking(snapshot.connection)
    const activeInputs = [
      snapshot.output, snapshot.progress, snapshot.taskDisplay, blocking,
      snapshot.retained.pending, snapshot.startAdmission.allowed, snapshot.startAdmission.reason, activeAdmission,
    ]
    const activeChanged = !sameInputs(this.#activeInputs, activeInputs)
    if (activeChanged) {
      this.#activeInputs = activeInputs
      this.#currentFacts = activeTaskFacts({
        output: snapshot.output,
        progress: snapshot.progress,
        display: snapshot.taskDisplay,
        blocking,
        ...(activeAdmission === undefined ? {} : { actionAdmission: activeAdmission }),
      }, language)
      this.#active = this.#currentFacts === null ? null : presentTask(this.#currentFacts, language)
    }
    const historyInputs = [
      language, snapshot.retained, snapshot.share?.shareInstance, blocking,
      snapshot.activeReceiveOperationId, snapshot.output.lifecycle, snapshot.output.activation,
      snapshot.output.actions, snapshot.output.interruption,
      snapshot.startAdmission.allowed, snapshot.startAdmission.reason, admission,
      snapshot.activeReceiveOperationId === null ? this.#currentFacts : null,
    ]
    const historyChanged = !sameInputs(this.#historyInputs, historyInputs)
    if (!activeChanged && !historyChanged) return this.#result
    if (historyChanged) {
      this.#historyInputs = historyInputs
      this.#retained = snapshot.retained.tasks
        .map(task => presentTask(
          withSettledTransferProgress(
            retainedTaskFacts(
              task,
              retainedTaskReadiness(task, snapshot.share?.shareInstance ?? null),
              {
                blocking: snapshot.share !== null && task.shareInstance === snapshot.share.shareInstance
                  ? blocking
                  : null,
                ...(admission === undefined ? {} : { admission: action => admission(task, action) }),
              },
              language,
            ),
            this.#currentFacts,
            snapshot.activeReceiveOperationId,
          ),
          language,
        ))
        .sort((left, right) => (right.createdAtMilliseconds ?? 0) - (left.createdAtMilliseconds ?? 0))
    }
    const current = chooseCurrentTask(this.#active, this.#retained, snapshot)
    const tasks = current === null
      ? this.#retained
      : Object.freeze([current, ...this.#retained.filter(task => task.operationId !== current.operationId)])
    this.#result = Object.freeze({ current, tasks, history: this.#retained })
    return this.#result
  }
}

function sameInputs(previous: readonly unknown[], next: readonly unknown[]): boolean {
  return previous.length === next.length && next.every((value, index) => value === previous[index])
}

export function composeTasks(
  snapshot: TaskInputs,
  admission?: RetainedAdmission,
  activeAdmission?: ActiveAdmission,
): TaskComposition {
  return new TaskComposer().compose(snapshot, admission, activeAdmission)
}

function withSettledTransferProgress(
  retained: TaskFacts,
  current: TaskFacts | null,
  activeTaskId: string | null,
): TaskFacts {
  if (
    current === null ||
    current.progress === null ||
    current.progress.transferJobId.length === 0 ||
    activeTaskId === current.lifecycle.taskId ||
    !isDeliveredReceiveState(current.lifecycle) ||
    current.lifecycle.taskId !== retained.lifecycle.taskId ||
    current.lifecycle.generation !== retained.lifecycle.generation
  ) {
    return retained
  }
  return Object.freeze({ ...retained, progress: current.progress })
}

function chooseCurrentTask(
  active: TaskPresentation | null,
  history: readonly TaskPresentation[],
  snapshot: TaskInputs,
): TaskPresentation | null {
  let current = active
  if (current !== null && snapshot.activeReceiveOperationId !== current.operationId) {
    const stored = history.find(task => task.operationId === current?.operationId)
    if (stored !== undefined && stored.generation >= current.generation) current = stored
  }
  return current
}
