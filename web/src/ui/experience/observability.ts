import type { V2ReceiverControllerOptions } from '../controller/contracts'
import type { V2ReceiverSnapshot } from '../v2-model'
import { compareTaskTransition, type TaskPresentationTransition } from '../tasks'
import { composeTasks } from './task-composition'
import { compareSavingTransition, presentSavingActions, type SavingPresentationTransition } from '../saving'

export class ReceiverExperienceObservability {
  readonly #source: V2ReceiverControllerOptions['trace']
  #tasks = new Map<string, TaskPresentationTransition>()
  #saving: SavingPresentationTransition | null = null

  constructor(source: V2ReceiverControllerOptions['trace']) { this.#source = source }

  publish(snapshot: V2ReceiverSnapshot): void {
    const observer = this.#source?.current
    if (observer === undefined) return
    try {
      const tasks = new Map<string, TaskPresentationTransition>()
      for (const task of composeTasks(snapshot).tasks) {
        const current = task.transition
        if (compareTaskTransition(this.#tasks.get(task.operationId) ?? null, current) !== null) {
          observer(Object.freeze({ name: 'receiver_experience', transition: 'task',
            operationId: current.operation_id, generation: current.generation,
            stage: current.stage, reason: current.reason, attention: current.attention,
            completeness: current.completeness, publication: current.publication,
            elapsedMilliseconds: task.elapsedMilliseconds }))
        }
        tasks.set(task.operationId, current)
      }
      this.#tasks = tasks
      const current = presentSavingActions({ offers: snapshot.output.offers,
        disabledReason: snapshot.startAdmission.reason }).transition
      if (compareSavingTransition(this.#saving, current) !== null) {
        observer(Object.freeze({ name: 'receiver_experience', transition: 'saving',
          projectionEpoch: current.projection_epoch, choiceId: current.choice_id,
          outcome: current.outcome, reason: current.reason }))
      }
      this.#saving = current
    } catch {
      // Presentation diagnostics never acquire authority over transfer publication.
    }
  }

  intent(action: string, snapshot: V2ReceiverSnapshot,
    operation?: Readonly<{ operationId: string; generation: bigint }>): void {
    try {
      this.#source?.current?.(Object.freeze({ name: 'receiver_experience', transition: 'intent',
        action, operationId: operation?.operationId ?? snapshot.output.receiveIntent?.operationId ?? null,
        generation: operation?.generation ?? snapshot.output.lifecycle?.generation ?? 0n }))
    } catch {
      // A diagnostic sink cannot change whether a user's action is admitted.
    }
  }
}
