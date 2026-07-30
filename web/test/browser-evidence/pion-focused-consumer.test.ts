import { describe, expect, it } from 'vitest'

import {
  CAPABILITY_PROBE_DEADLINE_MS,
  type CapabilityEvidence,
} from '../../scripts/browser-evidence/capability'
import type { NativeInteropEvidence } from '../../scripts/browser-evidence/result'
import { BROWSER_EVIDENCE_SCHEMA_VERSION } from '../../scripts/browser-evidence/vocabulary'
import type { NativeRtcCapability } from '../transport/webrtc/browser-capability'
import {
  executePionFocusedSample,
  type PionFocusedConsumer,
  type PionFocusedObservation,
  type PionInteropSample,
  type SuccessfulPionInterop,
} from '../transport/webrtc/pion-focused-consumer'

const TOPOLOGY = {
  topologyId: 'pr-same-host-kernel-route-ipv4',
  topologyProfileSha256: 'a'.repeat(64),
  topologyResolutionSha256: 'b'.repeat(64),
} as const
const ATTEMPT_ID = 'focused-consumer-attempt'
const SUCCEEDED_EVIDENCE: NativeInteropEvidence = {
  browser: { attemptId: ATTEMPT_ID, selectedPair: null },
  pion: { attemptId: ATTEMPT_ID, selectedPair: null },
}
const FAILED_EVIDENCE: NativeInteropEvidence = {
  browser: { attemptId: ATTEMPT_ID, selectedPair: null },
  pion: { attemptId: ATTEMPT_ID, selectedPair: null },
  failureCode: 'negotiation',
  failureMessage: 'native negotiation failed',
}

interface SyntheticInteropResult extends SuccessfulPionInterop {
  readonly proof: 'verified'
}

const SUCCEEDED_RESULT: SyntheticInteropResult = {
  topology: TOPOLOGY,
  nativeInteropEvidence: SUCCEEDED_EVIDENCE,
  proof: 'verified',
}

describe('focused Pion sample consumer', () => {
  it('uses API absence as the only not-applicable gate', async () => {
    const state = consumerState({ outcome: 'succeeded', result: SUCCEEDED_RESULT })

    await executePionFocusedSample(nativeCapability('unavailable'), state.consumer)

    expect(state.events).toEqual(['read-topology', 'retain:not-started'])
    expect(state.observations).toHaveLength(1)
    expect(state.observations[0]).toMatchObject({
      ...TOPOLOGY,
      rtcCapability: 'unavailable',
      capabilityEvidence: {
        apiPresence: 'absent',
        probeOutcome: 'not-started',
      },
      applicability: 'not-applicable',
      nativeInteropOutcome: 'not-started',
      nativeInteropEvidence: null,
    })
  })

  it('retains successful interop before an unusable probe blocks the verdict', async () => {
    const state = consumerState({ outcome: 'succeeded', result: SUCCEEDED_RESULT })

    await expect(
      executePionFocusedSample(nativeCapability('unusable'), state.consumer),
    ).rejects.toThrow(/local offer probe classified it as unusable/u)

    expect(state.events).toEqual(['run-interop', 'retain:succeeded', 'verify-success'])
    expect(state.observations[0]).toMatchObject({
      rtcCapability: 'unusable',
      capabilityEvidence: {
        apiPresence: 'present',
        probeOutcome: 'failed',
        failureCode: 'offer-creation',
        failureMessage: 'createOffer rejected',
      },
      applicability: 'applicable',
      nativeInteropOutcome: 'succeeded',
      nativeInteropEvidence: SUCCEEDED_EVIDENCE,
    })
  })

  it('retains failed interop before rejecting an unusable sample', async () => {
    const state = consumerState({
      outcome: 'failed',
      topology: TOPOLOGY,
      nativeInteropEvidence: FAILED_EVIDENCE,
    })

    await expect(
      executePionFocusedSample(nativeCapability('unusable'), state.consumer),
    ).rejects.toThrow(/native browser\/Pion attempt failed: native negotiation failed/u)

    expect(state.events).toEqual(['run-interop', 'retain:failed'])
    expect(state.observations).toHaveLength(1)
    expect(state.observations[0]).toMatchObject({
      rtcCapability: 'unusable',
      capabilityEvidence: {
        apiPresence: 'present',
        probeOutcome: 'failed',
      },
      applicability: 'applicable',
      nativeInteropOutcome: 'failed',
      nativeInteropEvidence: FAILED_EVIDENCE,
    })
  })

  it('accepts an available probe only after retaining and verifying successful interop', async () => {
    const state = consumerState({ outcome: 'succeeded', result: SUCCEEDED_RESULT })

    await expect(
      executePionFocusedSample(nativeCapability('available'), state.consumer),
    ).resolves.toBeUndefined()

    expect(state.events).toEqual(['run-interop', 'retain:succeeded', 'verify-success'])
    expect(state.observations[0]).toMatchObject({
      rtcCapability: 'available',
      capabilityEvidence: {
        apiPresence: 'present',
        probeOutcome: 'succeeded',
      },
      applicability: 'applicable',
      nativeInteropOutcome: 'succeeded',
      nativeInteropEvidence: SUCCEEDED_EVIDENCE,
    })
  })
})

function consumerState(sample: PionInteropSample<SyntheticInteropResult>): {
  readonly consumer: PionFocusedConsumer<SyntheticInteropResult>
  readonly events: string[]
  readonly observations: PionFocusedObservation[]
} {
  const events: string[] = []
  const observations: PionFocusedObservation[] = []
  return {
    events,
    observations,
    consumer: {
      readPionTopology: async () => {
        events.push('read-topology')
        return TOPOLOGY
      },
      runPionInteropSample: async () => {
        events.push('run-interop')
        return sample
      },
      retainObservation: async (observation) => {
        events.push(`retain:${observation.nativeInteropOutcome}`)
        observations.push(observation)
      },
      verifySuccessfulInterop: (result, observation) => {
        events.push('verify-success')
        expect(result.proof).toBe('verified')
        expect(observation.nativeInteropEvidence).toBe(result.nativeInteropEvidence)
      },
    },
  }
}

function nativeCapability(
  rtcCapability: 'available' | 'unavailable' | 'unusable',
): NativeRtcCapability {
  return {
    evidence: capabilityEvidence(rtcCapability),
    rtcCapability,
    pionApplicability: rtcCapability === 'unavailable' ? 'not-applicable' : 'applicable',
  }
}

function capabilityEvidence(
  rtcCapability: 'available' | 'unavailable' | 'unusable',
): CapabilityEvidence {
  if (rtcCapability === 'available') {
    return {
      schemaVersion: BROWSER_EVIDENCE_SCHEMA_VERSION,
      apiPresence: 'present',
      probeOutcome: 'succeeded',
      probeDeadlineMs: CAPABILITY_PROBE_DEADLINE_MS,
    }
  }
  if (rtcCapability === 'unavailable') {
    return {
      schemaVersion: BROWSER_EVIDENCE_SCHEMA_VERSION,
      apiPresence: 'absent',
      probeOutcome: 'not-started',
      probeDeadlineMs: CAPABILITY_PROBE_DEADLINE_MS,
    }
  }
  return {
    schemaVersion: BROWSER_EVIDENCE_SCHEMA_VERSION,
    apiPresence: 'present',
    probeOutcome: 'failed',
    probeDeadlineMs: CAPABILITY_PROBE_DEADLINE_MS,
    failureCode: 'offer-creation',
    failureMessage: 'createOffer rejected',
  }
}
