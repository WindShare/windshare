import type { Page } from '@playwright/test'

import {
  classifyRtcCapability,
  parseCapabilityEvidence,
  type CapabilityEvidence,
} from '../../../scripts/browser-evidence/capability'
import type { PionApplicability } from '../../../scripts/browser-evidence/result'
import type { RtcCapability } from '../../../scripts/browser-evidence/vocabulary'
import type * as RtcCapabilityProbe from '../../ice-topology/rtc-capability'

const RTC_CAPABILITY_PROBE_PATH = '/test/ice-topology/rtc-capability.ts'

export interface NativeRtcCapability {
  readonly evidence: CapabilityEvidence
  readonly rtcCapability: RtcCapability
  readonly pionApplicability: PionApplicability
}

export async function classifyNativePeerConnection(page: Page): Promise<NativeRtcCapability> {
  await page.goto('/')
  const rawEvidence = await page.evaluate(async (path) => {
    const capability = await import(path) as typeof RtcCapabilityProbe
    return capability.probeRtcCapability()
  }, RTC_CAPABILITY_PROBE_PATH)
  const evidence = parseCapabilityEvidence(rawEvidence)
  const rtcCapability = classifyRtcCapability(evidence)
  return {
    evidence,
    rtcCapability,
    pionApplicability: evidence.apiPresence === 'absent' ? 'not-applicable' : 'applicable',
  }
}
