import { expect } from '@playwright/test'

import {
  reducePeerAttemptOutcome,
  type LogicalAttempt,
} from '../../scripts/browser-evidence/attempt-collector'
import type { ChildEvidenceReporter } from '../../scripts/browser-evidence/child-evidence'
import type { MainRouteEvidence } from '../../scripts/browser-evidence/route-evidence'
import {
  MAIN_TRANSFER_BYTES,
  MAIN_TRANSFER_SHA256,
} from '../../scripts/browser-evidence/result'
import {
  selectedPairAllowedByTopology,
  type VerifiedTestIceTopologyLock,
} from '../../scripts/browser-evidence/test-ice-topology'
import type { RtcCapability } from '../../scripts/browser-evidence/vocabulary'
import type { NativeRtcCapability } from '../../test/transport/webrtc/browser-capability'
import type {
  BrowserAttemptTerminal,
  HotSwitchDeliveryTerminal,
  HotSwitchRuntimeTerminal,
} from './hot-switch-evidence'
import type { PromiseSettlement } from './hot-switch-sample-boundary'

export interface CollectedSample {
  readonly capability: NativeRtcCapability
  readonly expectedSha256: string
  readonly topologyLock: VerifiedTestIceTopologyLock
  readonly attempts: readonly LogicalAttempt[]
  readonly peerTerminal: PromiseSettlement<BrowserAttemptTerminal> | null
  readonly delivery: PromiseSettlement<HotSwitchDeliveryTerminal>
  readonly runtime: PromiseSettlement<HotSwitchRuntimeTerminal>
  readonly routeEvidence: MainRouteEvidence
  readonly routeError?: unknown
  readonly attemptError?: unknown
  readonly orchestrationErrors: readonly unknown[]
}

interface EvidencePublicationGate {
  publish(operation: (reporter: ChildEvidenceReporter) => void): void
}

export function assertAcceptedSample(sample: CollectedSample): void {
  if (sample.delivery.error !== undefined) throw sample.delivery.error
  const delivery = sample.delivery.value
  if (delivery === undefined) throw new Error('Delivery terminal disappeared after settlement')
  expect(delivery.outcome).toBe('succeeded')
  expect(delivery.evidence).toEqual({
    expectedBytes: MAIN_TRANSFER_BYTES,
    receivedBytes: MAIN_TRANSFER_BYTES,
    expectedSha256: MAIN_TRANSFER_SHA256,
    receivedSha256: MAIN_TRANSFER_SHA256,
    terminal: 'succeeded',
  })
  expect(delivery.jobOutcome).toEqual({
    status: 'Succeeded',
    failures: [],
    failureCount: 0,
    omittedFailureCount: 0,
  })
  if (sample.runtime.error !== undefined) throw sample.runtime.error
  expect(sample.runtime.value?.error).toBeUndefined()
  if (sample.attemptError !== undefined) throw sample.attemptError
  if (sample.routeError !== undefined) throw sample.routeError
  if (sample.orchestrationErrors.length > 0) {
    throw aggregateFailure(sample.orchestrationErrors, 'Hot-switch orchestration evidence failed')
  }

  const attemptOutcome = reducePeerAttemptOutcome(sample.attempts)
  if (sample.capability.rtcCapability === 'available') {
    if (sample.peerTerminal?.error !== undefined) throw sample.peerTerminal.error
    expect(sample.peerTerminal?.value?.stage).toBe('admitted')
    expect(attemptOutcome).toBe('admitted')
    expect(sample.routeEvidence.mode).toBe('hot-switch')
    assertDirectPairProof(sample.attempts, sample.capability.rtcCapability, sample.topologyLock)
    return
  }
  if (sample.capability.rtcCapability === 'unavailable') {
    expect(sample.peerTerminal).toBeNull()
    expect(sample.attempts).toEqual([])
    expect(attemptOutcome).toBe('not-started')
    expect(sample.routeEvidence.mode).toBe('relay-only')
    expect(sample.routeEvidence.observations.every(
      (observation) => observation.kind === 'dispatch' && observation.route === 'relay',
    )).toBe(true)
    return
  }

  expect(sample.routeEvidence.mode).toBe('relay-only')
  expect(sample.routeEvidence.observations.every(
    (observation) => observation.kind === 'dispatch' && observation.route === 'relay',
  )).toBe(true)
  throw new Error(
    `RTC capability ${sample.capability.rtcCapability} blocks acceptance after exact relay fallback`,
  )
}

function assertDirectPairProof(
  attempts: readonly LogicalAttempt[],
  rtcCapability: RtcCapability,
  topology: VerifiedTestIceTopologyLock,
): void {
  expect(rtcCapability).toBe('available')
  const admitted = attempts.filter((attempt) => attempt.outcome === 'admitted')
  expect(admitted).toHaveLength(1)
  const browser = admitted[0]?.events.find(({ evidence }) =>
    evidence.side === 'browser' && evidence.stage === 'admitted')?.evidence
  const sender = admitted[0]?.events.find(({ evidence }) =>
    evidence.side === 'sender' && evidence.stage === 'admitted')?.evidence
  if (browser?.side !== 'browser' || browser.stage !== 'admitted') {
    throw new Error('Admitted attempt lacks its browser terminal')
  }
  if (sender?.side !== 'sender' || sender.stage !== 'admitted') {
    throw new Error('Admitted attempt lacks its sender terminal')
  }
  expect(browser.selectedPair).not.toBeNull()
  expect(sender.selectedPair).not.toBeNull()
  if (browser.selectedPair === null || sender.selectedPair === null) return

  expect(selectedPairAllowedByTopology(
    browser.selectedPair,
    topology.profile,
    topology.resolution,
  )).toBe(true)
  expect(selectedPairAllowedByTopology(
    sender.selectedPair,
    topology.profile,
    topology.resolution,
  )).toBe(true)
}

export function validateReporterTopology(
  reporter: ChildEvidenceReporter | null,
  topology: VerifiedTestIceTopologyLock,
): void {
  if (
    reporter !== null &&
    (reporter.context.topologyProfileSha256 !== topology.profileSha256 ||
      reporter.context.topologyResolutionSha256 !== topology.resolutionSha256)
  ) {
    throw new Error('Main product topology digests differ from the parent evidence context')
  }
}

export function recordCompletedAuthorities(
  gate: EvidencePublicationGate,
  delivery: HotSwitchDeliveryTerminal | undefined,
  route: MainRouteEvidence | undefined,
): void {
  gate.publish((reporter) => {
    if (delivery !== undefined) reporter.recordDelivery(delivery.outcome, delivery.evidence)
    if (route !== undefined) reporter.recordRoute(route)
  })
}

function aggregateFailure(failures: readonly unknown[], message: string): unknown {
  if (failures.length === 1) return failures[0]
  return new AggregateError(failures, message)
}
