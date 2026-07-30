import type {
  BrowserSelectedPairEvidence,
  PionSelectedPairEvidence,
} from '../attempt-evidence.ts'
import type { LogicalAttempt } from '../attempt-collector.ts'
import { contractError } from '../contract/json.ts'
import type { MainRouteEvidence, PeerAdmissionObservation } from '../route-evidence.ts'
import {
  readVerifiedTestIceTopologyLock,
  selectedPairAllowedByTopology,
  type TestIceTopology,
  type TestIceTopologyResolution,
  type VerifiedTestIceTopologyLock,
} from '../test-ice-topology.ts'
import type {
  MainBrowserSampleResult,
  PionAcceptanceDisposition,
  PionBrowserSampleResult,
} from './contract.ts'

export function validateMainAcceptance(
  result: MainBrowserSampleResult,
  topologyLock: VerifiedTestIceTopologyLock,
): void {
  const { profile: topology, resolution } = readVerifiedTestIceTopologyLock(topologyLock)
  if (
    result.resultStatus !== 'final-valid' || result.executionOutcome !== 'healthy' ||
    result.playwrightOutcome !== 'passed' || result.deliveryOutcome !== 'succeeded'
  ) {
    contractError('main acceptance requires valid, healthy, passed, successful delivery evidence')
  }
  if (result.rtcCapability === 'available') {
    if (
      result.peerAttemptOutcome !== 'admitted' || result.routeEvidence?.mode !== 'hot-switch' ||
      !hasDirectSelectedPairProof(result.attempts, topology, resolution)
    ) {
      contractError('available RTC acceptance requires admission, direct pair proof, and hot-switch fence proof')
    }
    return
  }
  if (result.rtcCapability === 'unavailable') {
    if (
      result.peerAttemptOutcome !== 'not-started' || result.attempts.length !== 0 ||
      result.routeEvidence?.mode !== 'relay-only'
    ) {
      contractError('unavailable RTC acceptance requires attempt-free exact relay fallback')
    }
    return
  }
  contractError('unknown or unusable RTC capability is never an accepted main result')
}

export function validatePionAcceptance(
  result: PionBrowserSampleResult,
): PionAcceptanceDisposition {
  if (
    result.resultStatus !== 'final-valid' || result.executionOutcome !== 'healthy' ||
    result.playwrightOutcome !== 'passed'
  ) {
    contractError('Pion acceptance requires valid, healthy, passed execution evidence')
  }
  if (result.rtcCapability === 'available') {
    if (result.applicability !== 'applicable' || result.nativeInteropOutcome !== 'succeeded') {
      contractError('available RTC Pion acceptance requires successful applicable native interop')
    }
    return 'accepted'
  }
  if (result.rtcCapability === 'unavailable') {
    if (result.applicability !== 'not-applicable' || result.nativeInteropOutcome !== 'not-started') {
      contractError('unavailable RTC Pion evidence must be explicitly not-applicable')
    }
    return 'requires-main-relay-fallback'
  }
  contractError('unknown or unusable RTC capability is never accepted by the Pion suite')
}

/** Runtime admission remains an observed fact when pair proof is absent. This
 * predicate gives the later verdict one explicit authority for rejecting that
 * otherwise admitted sample without rewriting its peer outcome. */
export function hasDirectSelectedPairProof(
  attempts: readonly LogicalAttempt[],
  topology: TestIceTopology,
  resolution: TestIceTopologyResolution,
): boolean {
  const admitted = attempts.filter((attempt) => attempt.outcome === 'admitted')
  return admitted.length > 0 && admitted.every((attempt) => {
    const browser = attempt.events.find(({ evidence }) =>
      evidence.side === 'browser' && evidence.stage === 'admitted')?.evidence
    const sender = attempt.events.find(({ evidence }) =>
      evidence.side === 'sender' && evidence.stage === 'admitted')?.evidence
    if (
      browser?.side !== 'browser' || browser.stage !== 'admitted' ||
      sender?.side !== 'sender' || sender.stage !== 'admitted' ||
      browser.selectedPair === null || sender.selectedPair === null
    ) {
      return false
    }
    return selectedPairAllowedByTopology(browser.selectedPair, topology, resolution) &&
      selectedPairAllowedByTopology(sender.selectedPair, topology, resolution) &&
      selectedPairsCorrelate(browser.selectedPair, sender.selectedPair)
  })
}

export function validateHotSwitchAttemptCorrelation(
  route: MainRouteEvidence,
  attempts: readonly LogicalAttempt[],
): void {
  const admission = route.observations.find(
    (observation): observation is PeerAdmissionObservation => observation.kind === 'peer-admitted',
  )
  if (admission === undefined) contractError('hot-switch route evidence lacks peer admission')
  const matches = attempts.filter((attempt) =>
    attempt.sessionId === admission.sessionId && attempt.peerPathId === admission.peerPathId &&
    attempt.attemptId === admission.attemptId)
  if (matches.length !== 1 || matches[0]?.outcome !== 'admitted') {
    contractError('hot-switch route admission does not identify one admitted logical attempt')
  }
  const browserAdmission = matches[0].events.find(({ evidence }) =>
    evidence.side === 'browser' && evidence.stage === 'admitted')?.evidence
  if (
    browserAdmission?.side !== 'browser' || browserAdmission.stage !== 'admitted' ||
    browserAdmission.lane.laneId !== admission.lane.laneId ||
    browserAdmission.lane.laneEpoch !== admission.lane.laneEpoch
  ) {
    contractError('hot-switch route admission lane differs from attempt admission')
  }
}

export function selectedPairsCorrelate(
  browser: BrowserSelectedPairEvidence,
  pion: PionSelectedPairEvidence,
): boolean {
  return browserLocalEndpointMatchesPionRemote(browser.local, pion.remote) &&
    browserRemoteEndpointMatchesPionLocal(browser.remote, pion.local)
}

function browserLocalEndpointMatchesPionRemote(
  browser: BrowserSelectedPairEvidence['local'],
  pion: PionSelectedPairEvidence['local'],
): boolean {
  if (browser.protocol !== pion.protocol) return false
  if (browser.port !== undefined && browser.port !== pion.port) return false
  if (browser.address === undefined) return browser.candidateType === 'host'
  if (isIpLiteral(browser.address)) return browser.address === pion.address
  return browser.candidateType === 'host' && isMdnsHostname(browser.address)
}

function browserRemoteEndpointMatchesPionLocal(
  browser: BrowserSelectedPairEvidence['remote'],
  pion: PionSelectedPairEvidence['local'],
): boolean {
  return browser.address !== undefined && isIpLiteral(browser.address) &&
    browser.address === pion.address && browser.port === pion.port &&
    browser.protocol === pion.protocol
}

function isIpLiteral(address: string): boolean {
  return address.includes(':') || /^(?:0|[1-9]\d{0,2})(?:\.(?:0|[1-9]\d{0,2})){3}$/u.test(address)
}

function isMdnsHostname(address: string): boolean {
  return /^(?=.{1,253}\.?$)(?:[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)+local\.?$/u
    .test(address)
}
