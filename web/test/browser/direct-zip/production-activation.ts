import { encodeBase64Url } from '../../../src/crypto/bytes'
import {
  createDirectorySelectionResultRoot, createDirectResumableZipPlan, createFSAOwnedFileBinding,
  createSelectionSpec, createZipArchiveArtifact, deriveArtifactChoiceIdentity,
} from '../../../src/transfer/intent'
import {
  bindReceiveIntent, materializationPlanSemantics,
  type OfferedArtifactChoice, type ResolvedArtifactAction,
} from '../../../src/output/planning'
import { admitDirectZipRuntimeV1 } from '../../../src/output/direct-zip/session'
import type { BrowserReceiveWindow } from '../../../src/ui/browser-receive/contracts'
import type { createBrowserDirectZipComposition } from '../../../src/ui/browser-receive/direct-zip/production'
import {
  observeBrowserDirectZipFeatureFacts, BROWSER_DIRECT_ZIP_TARGET_ROUTE_ID,
} from '../../../src/ui/browser-receive/direct-zip/support'
import type { createBrowserReceiveComposition } from '../../../src/ui/v2-browser-receive-composition'

const id = (width: number, fill: number) => encodeBase64Url(new Uint8Array(width).fill(fill))

export async function prepareProductionDirectZipActivation(
  windowPort: BrowserReceiveWindow,
  receiver: ReturnType<typeof createBrowserReceiveComposition>,
  directZip: ReturnType<typeof createBrowserDirectZipComposition>,
  signal: AbortSignal,
) {
  const environment = await receiver.environment(signal)
  const source = await directZip.capabilities.read(signal)
  const admission = await admitDirectZipRuntimeV1({
    capabilities: { featureFacts: observeBrowserDirectZipFeatureFacts(windowPort), authority: source.authority },
    ...(source.policy === undefined ? {} : { policy: source.policy }),
  })
  if (admission.kind !== 'available') throw new Error('Browser Direct ZIP capability admission failed')
  const rootId = id(16, 2)
  const selection = await createSelectionSpec({
    shareInstance: id(16, 3), syntheticRoot: rootId,
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
  const artifact = await createZipArchiveArtifact(createDirectorySelectionResultRoot(rootId, 'shared'))
  const seedBinding = await createFSAOwnedFileBinding({
    operationId: id(16, 4), targetRef: id(32, 5), artifact,
    stableName: 'shared.windshare-' + id(16, 6) + '.zip', policies: admission.facts.support.policies,
  })
  const seedPlan = await createDirectResumableZipPlan(artifact, seedBinding)
  const choiceIdentity = await deriveArtifactChoiceIdentity(artifact, seedPlan)
  const route = {
    kind: 'direct-resumable-zip' as const,
    target: environment.targets.find(target => target.routeId === BROWSER_DIRECT_ZIP_TARGET_ROUTE_ID)!,
  } as OfferedArtifactChoice['route']
  const choice = {
    choiceId: choiceIdentity.id, artifactKind: 'zip-archive',
    plan: materializationPlanSemantics(route),
  } as OfferedArtifactChoice['choice']
  const offered = { route, choice } as OfferedArtifactChoice
  const action = {
    kind: 'resolved-artifact-action', choiceId: choiceIdentity.id, choice, route, artifact,
    selectionDigest: selection.digest, resolvedArtifactDigest: artifact.digest,
  } as ResolvedArtifactAction
  const presentation = receiver.startArtifactAuthority(offered, [choiceIdentity.id])
  await presentation.ready
  return {
    environment, rootId,
    commit: () => presentation.commit({
      action, signal, freezeAtFence: candidate => bindReceiveIntent({ selection, action, candidate }),
    }),
  }
}
