import type { CapabilityEvidence } from '../../../scripts/browser-evidence/capability'
import type { NativeInteropEvidence } from '../../../scripts/browser-evidence/result'
import {
  BROWSER_EVIDENCE_SCHEMA_VERSION,
  type RtcCapability,
} from '../../../scripts/browser-evidence/vocabulary'
import type { NativeRtcCapability } from './browser-capability'
import type { PionTopologySummary } from './browser-harness'

export interface SuccessfulPionInterop {
  readonly topology: PionTopologySummary
  readonly nativeInteropEvidence: NativeInteropEvidence
}

export type PionInteropSample<TResult extends SuccessfulPionInterop> =
  | { readonly outcome: 'succeeded'; readonly result: TResult }
  | {
      readonly outcome: 'failed'
      readonly topology: PionTopologySummary
      readonly nativeInteropEvidence: NativeInteropEvidence
    }

interface PionFocusedObservationBase extends PionTopologySummary {
  readonly schemaVersion: typeof BROWSER_EVIDENCE_SCHEMA_VERSION
  readonly rtcCapability: RtcCapability
  readonly capabilityEvidence: CapabilityEvidence
}

export interface PionNotApplicableObservation extends PionFocusedObservationBase {
  readonly applicability: 'not-applicable'
  readonly nativeInteropOutcome: 'not-started'
  readonly nativeInteropEvidence: null
}

export interface PionSucceededObservation extends PionFocusedObservationBase {
  readonly applicability: 'applicable'
  readonly nativeInteropOutcome: 'succeeded'
  readonly nativeInteropEvidence: NativeInteropEvidence
}

export interface PionFailedObservation extends PionFocusedObservationBase {
  readonly applicability: 'applicable'
  readonly nativeInteropOutcome: 'failed'
  readonly nativeInteropEvidence: NativeInteropEvidence
}

export type PionFocusedObservation =
  | PionNotApplicableObservation
  | PionSucceededObservation
  | PionFailedObservation

export interface PionFocusedConsumer<TResult extends SuccessfulPionInterop> {
  readonly readPionTopology: () => Promise<PionTopologySummary>
  readonly runPionInteropSample: () => Promise<PionInteropSample<TResult>>
  readonly retainObservation: (observation: PionFocusedObservation) => Promise<void>
  readonly verifySuccessfulInterop: (
    result: TResult,
    observation: PionSucceededObservation,
  ) => void
}

export async function executePionFocusedSample<TResult extends SuccessfulPionInterop>(
  capability: NativeRtcCapability,
  consumer: PionFocusedConsumer<TResult>,
): Promise<void> {
  if (capability.evidence.apiPresence === 'unknown') {
    throw new Error('native RTC API presence remained unknown after the capability probe')
  }
  if (capability.evidence.apiPresence === 'absent') {
    const topology = await consumer.readPionTopology()
    await consumer.retainObservation({
      schemaVersion: BROWSER_EVIDENCE_SCHEMA_VERSION,
      ...topology,
      rtcCapability: capability.rtcCapability,
      capabilityEvidence: capability.evidence,
      applicability: 'not-applicable',
      nativeInteropOutcome: 'not-started',
      nativeInteropEvidence: null,
    })
    return
  }

  // Probe health classifies the environment; API presence alone authorizes the
  // native attempt so an unusable probe cannot erase interop terminal evidence.
  const sample = await consumer.runPionInteropSample()
  if (sample.outcome === 'failed') {
    await consumer.retainObservation({
      schemaVersion: BROWSER_EVIDENCE_SCHEMA_VERSION,
      ...sample.topology,
      rtcCapability: capability.rtcCapability,
      capabilityEvidence: capability.evidence,
      applicability: 'applicable',
      nativeInteropOutcome: 'failed',
      nativeInteropEvidence: sample.nativeInteropEvidence,
    })
    throw new Error(
      `native browser/Pion attempt failed: ${sample.nativeInteropEvidence.failureMessage ?? 'no failure detail'}`,
    )
  }

  const observation: PionSucceededObservation = {
    schemaVersion: BROWSER_EVIDENCE_SCHEMA_VERSION,
    ...sample.result.topology,
    rtcCapability: capability.rtcCapability,
    capabilityEvidence: capability.evidence,
    applicability: 'applicable',
    nativeInteropOutcome: 'succeeded',
    nativeInteropEvidence: sample.result.nativeInteropEvidence,
  }
  // Evidence becomes durable before assertions or the unusable verdict can stop
  // the sample, preserving the completed runtime path for diagnosis.
  await consumer.retainObservation(observation)
  consumer.verifySuccessfulInterop(sample.result, observation)

  if (capability.rtcCapability !== 'available') {
    throw new Error(
      `native RTC API is present but the local offer probe classified it as ${capability.rtcCapability}`,
    )
  }
}
