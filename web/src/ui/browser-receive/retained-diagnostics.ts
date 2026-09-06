import type { V2BoundReceiveOperation } from '../v2-receive-runtime'
import type {
  OutputFailureBinding,
  OutputDiagnosticBackend,
  OutputDiagnosticsPorts,
  OutputFailureSinks,
  OutputTraceSource,
} from '../../output/diagnostics'

export function diagnosticsOption(
  backend: OutputDiagnosticBackend,
  trace: OutputTraceSource | undefined,
  failures?: OutputFailureSinks,
): { readonly diagnostics?: OutputDiagnosticsPorts } {
  const diagnostics = diagnosticsFor(backend, trace, failures)
  return diagnostics === undefined ? Object.freeze({}) : Object.freeze({ diagnostics })
}

export function diagnosticsFor(
  backend: OutputDiagnosticBackend,
  trace: OutputTraceSource | undefined,
  failures?: OutputFailureSinks,
): OutputDiagnosticsPorts | undefined {
  if (trace === undefined && failures === undefined) return undefined
  return Object.freeze({
    backend,
    ...(failures === undefined ? {} : { failures }),
    ...(trace === undefined ? {} : { trace }),
  })
}

export function bindRuntimeOutputFailures(
  runtime: V2BoundReceiveOperation,
  binding: OutputFailureBinding | undefined,
  display = runtime.display,
): V2BoundReceiveOperation {
  const bindOutputFailures = binding === undefined
    ? runtime.bindOutputFailures?.bind(runtime)
    : (failures: OutputFailureSinks | undefined) => binding.bind(failures)
  const bound: V2BoundReceiveOperation = {
    intent: runtime.intent,
    ...(display === undefined ? {} : { display }),
    get plans() {
      return runtime.plans
    },
    get transferJobId() {
      return runtime.transferJobId
    },
    lifecycle: runtime.lifecycle,
    activeControls: runtime.activeControls,
    ...(runtime.outputProgress === undefined ? {} : { outputProgress: runtime.outputProgress }),
    ...(runtime.repairProjection === undefined
      ? {}
      : { repairProjection: runtime.repairProjection }),
    ...(runtime.subscribeRepairProjectionActivation === undefined
      ? {}
      : {
          subscribeRepairProjectionActivation: (
            listener: Parameters<NonNullable<
              V2BoundReceiveOperation['subscribeRepairProjectionActivation']
            >>[0],
          ) => runtime.subscribeRepairProjectionActivation!(listener),
        }),
    ...(runtime.initialWorkspaceUsage === undefined
      ? {}
      : { initialWorkspaceUsage: runtime.initialWorkspaceUsage }),
    ...(bindOutputFailures === undefined ? {} : { bindOutputFailures }),
    interrupt: (control, transfer) => runtime.interrupt(control, transfer),
    startLifecycleAction: (action, lifecycle) =>
      runtime.startLifecycleAction(action, lifecycle),
    observeExpiry: lifecycle => runtime.observeExpiry(lifecycle),
    resolveWorkspaceUsage: lifecycle => runtime.resolveWorkspaceUsage(lifecycle),
    settleTransferAdmissionFailure: reason => runtime.settleTransferAdmissionFailure(reason),
    detach: () => runtime.detach(),
  }
  return Object.freeze(bound)
}