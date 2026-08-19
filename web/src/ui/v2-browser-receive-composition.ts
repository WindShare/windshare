import {
  probeBrowserEnvironment,
  startFSAParentPicker,
} from '../output/capability/acquisition'
import type { BrowserCapabilityRuntime } from '../output/capability/contract'
import {
  createOutputFailureBinding,
  type OutputDiagnosticBackend,
  type OutputDiagnosticsPorts,
  type OutputFailureBinding,
  type OutputFailureSinks,
} from '../output/diagnostics'
import {
  probeBrowserHandoffCapabilities,
  type BrowserHandoffCapabilityRuntime,
} from '../output/portable/packaged-handoff'
import type {
  ArtifactAction,
  BrowserHandoffTargetOffer,
  PortableEnvironmentOffer,
  WorkspaceEnvironmentOffer,
} from '../output/planning'
import type { WorkspaceStageTraceListener } from '../output/workspace/stages'
import type {
  V2ReceiveCompositionPort,
  V2StartedArtifactAuthority,
} from './v2-receive-runtime'
import { V2PresentationSourceError } from './v2-receive-runtime'
import type { BrowserReceiveWindow } from './browser-receive/contracts'
import { StartedFSAReceive } from './browser-receive/fsa'
import { StartedPortableReceive } from './browser-receive/portable'
import {
  bindRuntimeOutputFailures,
  listBrowserRetainedOperations,
  type BrowserRetainedCompositionOptions,
} from './browser-receive/retained'
import {
  portableEnvironmentOffer,
  quotaAvailability,
  unavailableRoute,
  workspaceEnvironmentOffer,
} from './browser-receive/shared'
import { StartedWorkspaceReceive } from './browser-receive/workspace'

export type { BrowserReceiveWindow } from './browser-receive/contracts'
export type { BrowserRetainedContinuationExecutor } from './browser-receive/retained'

export interface BrowserReceiveCompositionOptions extends BrowserRetainedCompositionOptions {
  readonly onTrace?: WorkspaceStageTraceListener
}

interface BrowserProviders {
  readonly fsa: boolean
  readonly workspace: boolean
  readonly portable: boolean
  readonly runtime: BrowserCapabilityRuntime
  readonly handoffTarget: BrowserHandoffTargetOffer | null
  readonly workspaceOffer: WorkspaceEnvironmentOffer | null
  readonly portableOffer: PortableEnvironmentOffer | null
}

/**
 * Product offers are derived from complete provider assemblies, not isolated browser APIs.
 * That prevents a picker or Blob probe from advertising a route whose durable owner is absent.
 */
export function createBrowserReceiveComposition(
  windowPort: BrowserReceiveWindow,
  options: BrowserReceiveCompositionOptions = {},
): V2ReceiveCompositionPort {
  const composition: V2ReceiveCompositionPort = {
    retained: Object.freeze({
      list: (signal: AbortSignal) => listBrowserRetainedOperations(windowPort, options, signal),
    }),
    environment: async (signal) => {
      const providers = await inspectBrowserProviders(windowPort, signal)
      return probeBrowserEnvironment(providers.runtime, {
        ...(providers.workspaceOffer === null ? {} : { workspace: providers.workspaceOffer }),
        ...(providers.portableOffer === null ? {} : { portable: providers.portableOffer }),
      }).offers
    },
    startArtifactAuthority: (action, failures) => startProductionAuthority(
      windowPort,
      action,
      options.onTrace,
      options.outputTrace,
      failures,
    ),
  }
  return Object.freeze(composition)
}

async function inspectBrowserProviders(
  windowPort: BrowserReceiveWindow,
  signal: AbortSignal,
): Promise<BrowserProviders> {
  signal.throwIfAborted()
  const hasRepository = typeof windowPort.indexedDB?.open === 'function'
  const hasLocks = typeof windowPort.navigator.locks?.request === 'function'
  const hasWorkspaceStorage =
    typeof windowPort.navigator.storage?.getDirectory === 'function' &&
    typeof windowPort.navigator.storage?.estimate === 'function'
  const handoffFacts = probeBrowserHandoffCapabilities(
    windowPort as unknown as BrowserHandoffCapabilityRuntime,
  )
  const workspace = hasRepository && hasLocks && hasWorkspaceStorage &&
    handoffFacts.supportsWorkspacePackage
  const portable = hasRepository && handoffFacts.supportsPortableArtifact
  const runtime: BrowserCapabilityRuntime = Object.freeze({
    ...(hasRepository && hasLocks && typeof windowPort.showDirectoryPicker === 'function'
      ? { showDirectoryPicker: windowPort.showDirectoryPicker.bind(windowPort) }
      : {}),
    browserHandoff: windowPort as unknown as BrowserHandoffCapabilityRuntime,
  })
  const initial = probeBrowserEnvironment(runtime)
  const workspaceOffer = workspace
    ? workspaceEnvironmentOffer(await quotaAvailability(windowPort.navigator.storage, signal))
    : null
  const portableOffer = portable ? portableEnvironmentOffer() : null
  return Object.freeze({
    fsa: initial.fsaParent !== null,
    workspace,
    portable,
    runtime,
    handoffTarget: initial.browserHandoff,
    workspaceOffer,
    portableOffer,
  })
}

function inspectBrowserProvidersSynchronously(windowPort: BrowserReceiveWindow): BrowserProviders {
  const hasRepository = typeof windowPort.indexedDB?.open === 'function'
  const hasLocks = typeof windowPort.navigator.locks?.request === 'function'
  const hasWorkspaceStorage =
    typeof windowPort.navigator.storage?.getDirectory === 'function' &&
    typeof windowPort.navigator.storage?.estimate === 'function'
  const handoffFacts = probeBrowserHandoffCapabilities(
    windowPort as unknown as BrowserHandoffCapabilityRuntime,
  )
  const runtime: BrowserCapabilityRuntime = Object.freeze({
    ...(hasRepository && hasLocks && typeof windowPort.showDirectoryPicker === 'function'
      ? { showDirectoryPicker: windowPort.showDirectoryPicker.bind(windowPort) }
      : {}),
    browserHandoff: windowPort as unknown as BrowserHandoffCapabilityRuntime,
  })
  const environment = probeBrowserEnvironment(runtime)
  return Object.freeze({
    fsa: environment.fsaParent !== null,
    workspace: hasRepository && hasLocks && hasWorkspaceStorage &&
      handoffFacts.supportsWorkspacePackage,
    portable: hasRepository && handoffFacts.supportsPortableArtifact,
    runtime,
    handoffTarget: environment.browserHandoff,
    workspaceOffer: hasRepository && hasLocks && hasWorkspaceStorage &&
        handoffFacts.supportsWorkspacePackage
      ? workspaceEnvironmentOffer(null)
      : null,
    portableOffer: hasRepository && handoffFacts.supportsPortableArtifact
      ? portableEnvironmentOffer()
      : null,
  })
}

function diagnosticsFor(
  backend: OutputDiagnosticBackend,
  trace: BrowserReceiveCompositionOptions['outputTrace'],
  failures?: OutputFailureSinks,
): OutputDiagnosticsPorts | undefined {
  if (trace === undefined && failures === undefined) return undefined
  return Object.freeze({
    backend,
    ...(failures === undefined ? {} : { failures }),
    ...(trace === undefined ? {} : { trace }),
  })
}

function bindStartedAuthorityOutputFailures(
  authority: V2StartedArtifactAuthority,
  binding: OutputFailureBinding,
): V2StartedArtifactAuthority {
  return Object.freeze({
    finalize: async (
      freezeIntent: Parameters<V2StartedArtifactAuthority['finalize']>[0],
      signal: AbortSignal,
    ) => bindRuntimeOutputFailures(
      await authority.finalize(freezeIntent, signal),
      binding,
    ),
    release: (reason: unknown) => authority.release(reason),
  })
}

function pickerPresentationError(error: unknown): unknown {
  return error instanceof DOMException && error.name === 'AbortError'
    ? new V2PresentationSourceError('picker_refused', error)
    : error
}

function startProductionAuthority(
  windowPort: BrowserReceiveWindow,
  action: ArtifactAction,
  trace: WorkspaceStageTraceListener | undefined,
  outputTrace: BrowserReceiveCompositionOptions['outputTrace'],
  failures?: OutputFailureSinks,
): V2StartedArtifactAuthority {
  const providers = inspectBrowserProvidersSynchronously(windowPort)
  const binding = createOutputFailureBinding(failures)
  switch (action.plan.kind) {
    case 'direct-tree': {
      if (!providers.fsa || action.plan.target.kind !== 'fsa-parent-directory') {
        throw unavailableRoute()
      }
      // startFSAParentPicker invokes showDirectoryPicker before returning this click-stack call.
      let picked: ReturnType<typeof startFSAParentPicker>
      try {
        picked = startFSAParentPicker(providers.runtime, action.plan.target)
      } catch (error) {
        throw pickerPresentationError(error)
      }
      return bindStartedAuthorityOutputFailures(
        new StartedFSAReceive(
          action,
          picked,
          diagnosticsFor('file_system_access', outputTrace, binding.sinks),
        ),
        binding,
      )
    }
    case 'workspace-then-publish':
      if (!providers.workspace || providers.workspaceOffer === null ||
          providers.handoffTarget?.supportsWorkspacePackage !== true) {
        throw unavailableRoute()
      }
      return bindStartedAuthorityOutputFailures(
        new StartedWorkspaceReceive(
          windowPort,
          action,
          trace,
          diagnosticsFor('origin_private', outputTrace, binding.sinks),
        ),
        binding,
      )
    case 'portable-handoff':
      if (!providers.portable || providers.portableOffer === null ||
          providers.handoffTarget?.supportsPortableArtifact !== true) {
        throw unavailableRoute()
      }
      return bindStartedAuthorityOutputFailures(
        new StartedPortableReceive(
          windowPort,
          action,
          diagnosticsFor('portable', outputTrace, binding.sinks),
        ),
        binding,
      )
    case 'direct-atomic':
      throw unavailableRoute()
  }
}
