import { bindRuntimeOutputFailures } from './browser-receive/retained-diagnostics'
import { snapshotReceiveOperationDisplay, type ReceiveOperationDisplay } from '../output/workspace/operation-display'
import {
  probeBrowserEnvironment,
  startFSAParentPicker,
} from '../output/capability/acquisition'
import type { BrowserCapabilityRuntime } from '../output/capability/contract'
import { probeNativeObjectSupport, type NativeSupportRuntime } from '../output/origin-private/native-object/support'
import {
  createOutputFailureBinding,
  type LocalOutputOperationFailureDiagnosticsPort,
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
  BrowserHandoffTargetOffer,
  DirectZipSupportFacts,
  EnvironmentTargetOfferInput,
  FSADirectoryContainerOffer,
  OfferedArtifactChoice,
  PortableEnvironmentOffer,
  WorkspaceEnvironmentOffer,
  ZipRouteRecommendationPolicyV1,
} from '../output/planning'
import type { WorkspaceStageTraceListener } from '../output/workspace/stages'
import type { ArtifactChoiceID } from '../transfer/intent'
import type {
  V2ArtifactPresentationAuthority,
  V2ReceiveCompositionPort,
  V2RouteCommitInput,
  V2RouteCommitResult,
} from './v2-receive-runtime'
import { V2PresentationSourceError } from './v2-receive-runtime'
import type { BrowserReceiveWindow } from './browser-receive/contracts'
import { FSAArtifactPresentationAuthority } from './browser-receive/fsa-route'
import { startPortableArtifactAuthority } from './browser-receive/portable-route'
import {
  listBrowserRetainedOperations,
  readBrowserCompatibleNameRepairSummary,
  type BrowserRetainedCompositionOptions,
} from './browser-receive/retained'
import {
  portableEnvironmentOffer,
  quotaAvailability,
  unavailableRoute,
  workspaceEnvironmentOffer,
} from './browser-receive/shared'
import { startWorkspaceArtifactAuthority } from './browser-receive/workspace-route'
import {
  inspectBrowserDirectZipEnvironment,
  startBrowserDirectZipAuthority,
  type BrowserDirectZipCompositionPort,
  type InstalledBrowserDirectZipRoute,
} from './browser-receive/direct-zip'

export type { BrowserReceiveWindow } from './browser-receive/contracts'
export type { BrowserRetainedContinuationExecutor } from './browser-receive/retained'

export interface BrowserReceiveCompositionOptions extends BrowserRetainedCompositionOptions {
  readonly onTrace?: WorkspaceStageTraceListener
  readonly directZip?: BrowserDirectZipCompositionPort
}

interface InstalledBrowserRouteRegistry {
  readonly runtime: BrowserCapabilityRuntime
  readonly fsaParent: FSADirectoryContainerOffer | null
  readonly handoffTarget: BrowserHandoffTargetOffer | null
  readonly workspaceOffer: WorkspaceEnvironmentOffer | null
  readonly portableOffer: PortableEnvironmentOffer | null
  readonly directZipTarget: EnvironmentTargetOfferInput | null
  readonly directZipSupport: DirectZipSupportFacts
  readonly zipRecommendationPolicy: ZipRouteRecommendationPolicyV1 | null
  readonly installedDirectZip: InstalledBrowserDirectZipRoute | undefined
}

/**
 * Product offers are derived from complete provider assemblies, not isolated browser APIs.
 * That prevents a picker or Blob probe from advertising a route whose durable owner is absent.
 */
export function createBrowserReceiveComposition(
  windowPort: BrowserReceiveWindow,
  options: BrowserReceiveCompositionOptions = {},
): V2ReceiveCompositionPort {
  let installedDirectZip: InstalledBrowserDirectZipRoute | undefined
  let nativeObjectSupported = false
  let nativeSupportProbe: Promise<boolean> | undefined
  const composition: V2ReceiveCompositionPort = {
    retained: Object.freeze({
      list: (signal: AbortSignal) => listBrowserRetainedOperations(windowPort, options, signal),
      readRepairSummary: (operationId: string, signal: AbortSignal) =>
        readBrowserCompatibleNameRepairSummary(options, operationId, signal),
    }),
    environment: async (signal) => {
      signal.throwIfAborted()
      nativeSupportProbe ??= probeNativeObjectSupport(windowPort as unknown as NativeSupportRuntime)
      nativeObjectSupported = await nativeSupportProbe
      const registry = await inspectBrowserRouteRegistry(windowPort, options.directZip, nativeObjectSupported, signal)
      installedDirectZip = registry.installedDirectZip
      return probeBrowserEnvironment(registry.runtime, {
        ...(registry.directZipTarget === null ? {} : { targets: [registry.directZipTarget] }),
        ...(registry.workspaceOffer === null ? {} : { workspace: registry.workspaceOffer }),
        ...(registry.portableOffer === null ? {} : { portable: registry.portableOffer }),
        directZipSupport: registry.directZipSupport,
        ...(registry.zipRecommendationPolicy === null
          ? {}
          : { zipRecommendationPolicy: registry.zipRecommendationPolicy }),
      }).offers
    },
    startArtifactAuthority: (action, preClickRanking, failures, display) => startProductionAuthority(
      windowPort,
      action,
      preClickRanking,
      options.onTrace,
      options.outputTrace,
      options.localOutputFailures,
      installedDirectZip,
      nativeObjectSupported,
      failures,
      display,
    ),
  }
  return Object.freeze(composition)
}

async function inspectBrowserRouteRegistry(
  windowPort: BrowserReceiveWindow,
  directZip: BrowserDirectZipCompositionPort | undefined,
  nativeObjectSupported: boolean,
  signal: AbortSignal,
): Promise<InstalledBrowserRouteRegistry> {
  signal.throwIfAborted()
  const hasRepository = typeof windowPort.indexedDB?.open === 'function'
  const hasLocks = typeof windowPort.navigator.locks?.request === 'function'
  const hasWorkspaceDirectory =
    typeof windowPort.navigator.storage?.getDirectory === 'function'
  const handoffFacts = probeBrowserHandoffCapabilities(
    windowPort as unknown as BrowserHandoffCapabilityRuntime,
  )
  const hasWorkspaceRoute = nativeObjectSupported && hasRepository && hasLocks && hasWorkspaceDirectory &&
    handoffFacts.supportsWorkspacePackage
  const hasPortableRoute = hasRepository && handoffFacts.supportsPortableArtifact
  const runtime: BrowserCapabilityRuntime = Object.freeze({
    ...(hasRepository && hasLocks && typeof windowPort.showDirectoryPicker === 'function'
      ? { showDirectoryPicker: windowPort.showDirectoryPicker.bind(windowPort) }
      : {}),
    browserHandoff: windowPort as unknown as BrowserHandoffCapabilityRuntime,
  })
  const initial = probeBrowserEnvironment(runtime)
  const workspaceOffer = hasWorkspaceRoute
    ? workspaceEnvironmentOffer(await quotaAvailability(windowPort.navigator.storage, signal))
    : null
  const portableOffer = hasPortableRoute ? portableEnvironmentOffer() : null
  const directZipContribution = await inspectBrowserDirectZipEnvironment(
    windowPort,
    directZip,
    signal,
  )
  const installedDirectZip = directZip !== undefined && directZipContribution.lookup.kind === 'available'
    ? Object.freeze({ directZip, facts: directZipContribution.lookup.facts })
    : undefined
  return Object.freeze({
    runtime,
    fsaParent: initial.fsaParent,
    handoffTarget: initial.browserHandoff,
    workspaceOffer,
    portableOffer,
    directZipTarget: directZipContribution.target ?? null,
    directZipSupport: directZipContribution.lookup.kind === 'available'
      ? directZipContribution.lookup.facts.support
      : directZipContribution.lookup.support,
    zipRecommendationPolicy: directZipContribution.lookup.kind === 'available'
      ? directZipContribution.lookup.facts.recommendationPolicy
      : null,
    installedDirectZip,
  })
}

function inspectBrowserRouteRegistrySynchronously(
  windowPort: BrowserReceiveWindow,
  nativeObjectSupported: boolean,
): InstalledBrowserRouteRegistry {
  const hasRepository = typeof windowPort.indexedDB?.open === 'function'
  const hasLocks = typeof windowPort.navigator.locks?.request === 'function'
  const hasWorkspaceDirectory =
    typeof windowPort.navigator.storage?.getDirectory === 'function'
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
  const hasWorkspaceRoute = nativeObjectSupported && hasRepository && hasLocks && hasWorkspaceDirectory &&
    handoffFacts.supportsWorkspacePackage
  const hasPortableRoute = hasRepository && handoffFacts.supportsPortableArtifact
  return Object.freeze({
    runtime,
    fsaParent: environment.fsaParent,
    handoffTarget: environment.browserHandoff,
    workspaceOffer: hasWorkspaceRoute
      ? workspaceEnvironmentOffer(null)
      : null,
    portableOffer: hasPortableRoute
      ? portableEnvironmentOffer()
      : null,
    directZipTarget: null,
    directZipSupport: Object.freeze({
      kind: 'unavailable',
      reason: 'runtime-not-installed',
    }),
    zipRecommendationPolicy: null,
    installedDirectZip: undefined,
  })
}

function localOutputFailuresOption(
  failures: LocalOutputOperationFailureDiagnosticsPort | undefined,
): Readonly<{ localOutputFailures?: LocalOutputOperationFailureDiagnosticsPort }> {
  return failures === undefined
    ? Object.freeze({})
    : Object.freeze({ localOutputFailures: failures })
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

function bindArtifactAuthorityOutputFailures(
  authority: V2ArtifactPresentationAuthority,
  binding: OutputFailureBinding,
  display?: ReceiveOperationDisplay,
): V2ArtifactPresentationAuthority {
  return Object.freeze({
    ready: authority.ready,
    commit: async (input: V2RouteCommitInput) =>
      bindCommittedOperation(await authority.commit({ ...input,
        ...(display === undefined ? {} : { display }) }), binding, display),
    release: (reason: unknown) => authority.release(reason),
  })
}

function bindCommittedOperation(
  result: V2RouteCommitResult,
  binding: OutputFailureBinding,
  display?: ReceiveOperationDisplay,
): V2RouteCommitResult {
  if (result.kind !== 'bound-operation') return result
  return Object.freeze({
    kind: 'bound-operation',
    operation: bindRuntimeOutputFailures(result.operation, binding, result.display ?? display),
  })
}

function startProductionAuthority(
  windowPort: BrowserReceiveWindow,
  offered: OfferedArtifactChoice,
  preClickRanking: readonly ArtifactChoiceID[],
  trace: WorkspaceStageTraceListener | undefined,
  outputTrace: BrowserReceiveCompositionOptions['outputTrace'],
  localOutputFailures: LocalOutputOperationFailureDiagnosticsPort | undefined,
  installedDirectZip: InstalledBrowserDirectZipRoute | undefined,
  nativeObjectSupported: boolean,
  failures?: OutputFailureSinks,
  displayInput?: ReceiveOperationDisplay,
): V2ArtifactPresentationAuthority {
  const registry = inspectBrowserRouteRegistrySynchronously(windowPort, nativeObjectSupported)
  const binding = createOutputFailureBinding(failures)
  const display = displayForRoute(offered, displayInput)
  switch (offered.route.kind) {
    case 'direct-tree': {
      if (offered.route.target.kind !== 'fsa-parent-directory' ||
          registry.fsaParent === null ||
          offered.route.target.routeId !== registry.fsaParent.routeId) {
        throw unavailableRoute()
      }
      // startFSAParentPicker invokes showDirectoryPicker before returning this click-stack call.
      let picked: ReturnType<typeof startFSAParentPicker>
      try {
        picked = startFSAParentPicker(registry.runtime, registry.fsaParent)
      } catch (error) {
        throw pickerPresentationError(error)
      }
      const diagnostics = diagnosticsFor('file_system_access', outputTrace, binding.sinks)
      return bindArtifactAuthorityOutputFailures(
        new FSAArtifactPresentationAuthority({
          offered,
          picked,
          preClickRanking,
          ...(diagnostics === undefined ? {} : { diagnostics }),
          ...localOutputFailuresOption(localOutputFailures),
        }),
        binding,
        display,
      )
    }
    case 'workspace-then-publish': {
      if (registry.workspaceOffer === null ||
          registry.handoffTarget?.supportsWorkspacePackage !== true ||
          offered.route.workspace.routeId !== registry.workspaceOffer.routeId ||
          offered.route.publicationTarget.routeId !== registry.handoffTarget.routeId) {
        throw unavailableRoute()
      }
      const diagnostics = diagnosticsFor('origin_private', outputTrace, binding.sinks)
      return bindArtifactAuthorityOutputFailures(
        startWorkspaceArtifactAuthority({
          windowPort,
          offered,
          preClickRanking,
          ...(trace === undefined ? {} : { trace }),
          ...(diagnostics === undefined ? {} : { diagnostics }),
        }),
        binding,
        display,
      )
    }
    case 'portable-handoff':
      if (registry.portableOffer === null ||
          registry.handoffTarget?.supportsPortableArtifact !== true ||
          offered.route.portable.routeId !== registry.portableOffer.routeId ||
          offered.route.handoffTarget.routeId !== registry.handoffTarget.routeId) {
        throw unavailableRoute()
      }
      return bindArtifactAuthorityOutputFailures(
        startPortableArtifactAuthority(
          windowPort,
          offered,
          diagnosticsFor('portable', outputTrace, binding.sinks),
          preClickRanking,
        ),
        binding,
        display,
      )
    case 'direct-resumable-zip':
      return bindArtifactAuthorityOutputFailures(
        startBrowserDirectZipAuthority(windowPort, offered, preClickRanking, installedDirectZip),
        binding,
        display,
      )
    case 'direct-atomic':
      throw unavailableRoute()
  }
}

function displayForRoute(offered: OfferedArtifactChoice, input: ReceiveOperationDisplay | undefined) {
  if (input === undefined) return undefined
  const browserDownload = offered.route.kind === 'portable-handoff' || offered.route.kind === 'workspace-then-publish'
  return snapshotReceiveOperationDisplay({
    ...input, ...(browserDownload ? { destinationLabel: 'Browser downloads' } : {}),
  })
}

function pickerPresentationError(error: unknown): unknown {
  return error instanceof DOMException && error.name === 'AbortError'
    ? new V2PresentationSourceError('picker_refused', error)
    : error
}
