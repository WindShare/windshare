import type { AsyncActionCallbacks, ReceiverController } from '../controller'
import type { SafeNameRestoreActions, TaskViewActions } from '../tasks/TaskView'

// Reuse one frozen adapter per controller so high-frequency progress re-renders keep
// referential stability without requiring every caller to memoize manually.
const restoreActionsByController = new WeakMap<ReceiverController, SafeNameRestoreActions>()

export function taskActions(
  controller: ReceiverController,
  callbacks?: AsyncActionCallbacks,
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

export function safeNameRestoreActions(controller: ReceiverController): SafeNameRestoreActions {
  const cached = restoreActionsByController.get(controller)
  if (cached !== undefined) return cached
  const actions = Object.freeze<SafeNameRestoreActions>({
    defaultPlatform: () => controller.detectRestorePlatform(),
    prepare: (operationId, platform) => controller.prepareSafeNameRestore(operationId, platform),
    exportArtifact: (operationId, platform) => controller.exportSafeNameRestoreArtifact(operationId, platform),
    downloadManifest: plan => controller.downloadSafeNameRestoreManifest(plan),
    mapError: error => controller.mapTaskFailure(error),
  })
  restoreActionsByController.set(controller, actions)
  return actions
}
