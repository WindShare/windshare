import type { ReceiverController, TaskActionCallbacks } from '../controller'
import type { TaskViewActions } from '../tasks/TaskView'

export function taskActions(
  controller: ReceiverController,
  callbacks?: TaskActionCallbacks,
): TaskViewActions {
  return {
    perform: (action, overrideCallbacks) => {
      if (action.disabledReason !== null) return
      const effective = overrideCallbacks ?? callbacks
      if (action.target.kind === 'active') controller.performLifecycleAction(action.target.action, effective)
      else controller.performRetainedAction(action.target.task, action.target.action, effective)
    },
  }
}
