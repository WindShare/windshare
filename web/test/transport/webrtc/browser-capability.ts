import type { Page } from '@playwright/test'

import {
  classifyRtcCapability,
  parseRtcCapabilityDiagnostic,
  type RtcCapabilityDiagnostic,
  type RtcCapabilityStatus,
} from '../../ice-topology/rtc-capability'
import type * as RtcCapabilityProbe from '../../ice-topology/rtc-capability'

const RTC_CAPABILITY_PROBE_PATH = '/test/ice-topology/rtc-capability.ts'

export interface NativeRtcCapabilityDiagnostic {
  readonly diagnostic: RtcCapabilityDiagnostic
  readonly rtcCapability: RtcCapabilityStatus
  readonly pionApplicability: 'applicable' | 'not-applicable'
}

export async function classifyNativePeerConnection(
  page: Page,
  baseURL?: string,
): Promise<NativeRtcCapabilityDiagnostic> {
  await page.goto(baseURL === undefined ? '/' : new URL('/', baseURL).href)
  const rawDiagnostic = await page.evaluate(async (path) => {
    const capability = await import(path) as typeof RtcCapabilityProbe
    return capability.probeRtcCapability()
  }, RTC_CAPABILITY_PROBE_PATH)
  const diagnostic = parseRtcCapabilityDiagnostic(rawDiagnostic)
  const rtcCapability = classifyRtcCapability(diagnostic)
  return {
    diagnostic,
    rtcCapability,
    pionApplicability: diagnostic.apiPresence === 'absent' ? 'not-applicable' : 'applicable',
  }
}
