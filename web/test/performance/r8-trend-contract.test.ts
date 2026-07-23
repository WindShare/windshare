import { describe, expect, it } from 'vitest'

import { validateR8TrendRecords } from '../../scripts/r8-trend-contract.mjs'

const browsers = ['chromium', 'firefox', 'webkit'] as const
const commonScenarios = [
  'connectivity-preview-unknown',
  'connectivity-download-small',
  'connectivity-download-unknown',
  'connectivity-download-large',
  'media-preview',
] as const

interface TrendTestMetric {
  readonly raw: number[]
  readonly p50: number
  readonly p95: number
}

interface TrendTestRecord {
  schema: number
  statistics: { percentileMethod: string; decimalPlaces: number }
  browser: string
  scenario: string
  workload: { samples: number }
  capabilities: Record<string, boolean>
  unavailable: Record<string, string>
  metrics: Record<string, TrendTestMetric>
}

describe('R8 trend evidence contract', () => {
  it('accepts only the complete fixed browser/scenario matrix', () => {
    const records = validRecords()
    expect(validateR8TrendRecords(records, 5)).toEqual({ browsers: 3, records: 24, sampleCount: 5 })

    expect(() => validateR8TrendRecords(records.slice(1), 5)).toThrow(/expected 24/u)
    expect(() => validateR8TrendRecords([...records, records[0]], 5)).toThrow(/expected 24/u)
    const wrongRoute = structuredClone(records)
    const route = wrongRoute.find((record) => record.browser === 'webkit' &&
      record.scenario === 'real-relay-fallback')
    if (route === undefined) throw new Error('valid fixture lost WebKit route')
    route.scenario = 'real-relay-to-peer-hot-switch'
    expect(() => validateR8TrendRecords(wrongRoute, 5)).toThrow(/unexpected trend identity/u)
  })

  it('rejects partial metrics, invalid samples, and unexplained capabilities', () => {
    const partial = structuredClone(validRecords())
    delete partial[0]?.metrics.firstBrokerByteMilliseconds
    expect(() => validateR8TrendRecords(partial, 5)).toThrow(/metrics keys differ/u)

    const short = structuredClone(validRecords())
    requiredMetric(short[0], 'firstBrokerByteMilliseconds').raw.pop()
    expect(() => validateR8TrendRecords(short, 5)).toThrow(/5 raw samples/u)

    const nonFinite = structuredClone(validRecords())
    requiredMetric(nonFinite[0], 'firstBrokerByteMilliseconds').raw[0] = Number.NaN
    expect(() => validateR8TrendRecords(nonFinite, 5)).toThrow(/non-finite/u)

    const missingReason = structuredClone(validRecords())
    const webkitOutput = missingReason.find((record) => record.browser === 'webkit' &&
      record.scenario === 'output-portable-opfs')
    if (webkitOutput === undefined) throw new Error('valid fixture lost WebKit output')
    delete webkitOutput.unavailable.originPrivateStaging
    expect(() => validateR8TrendRecords(missingReason, 5)).toThrow(/unavailable reasons keys differ/u)
  })
})

function validRecords() {
  const records: TrendTestRecord[] = []
  for (const browser of browsers) {
    for (const scenario of commonScenarios) records.push(commonRecord(browser, scenario))
    records.push(outputRecord(browser))
    records.push(wideRecord(browser))
    records.push(routeRecord(browser))
  }
  return records
}

function commonRecord(browser: string, scenario: string) {
  if (scenario === 'media-preview') {
    return record(browser, scenario, {
      imageFirstFrameMilliseconds: metric(),
      imageOpenMilliseconds: metric(),
      videoMetadataMilliseconds: metric(),
      videoSeekMilliseconds: metric(),
    }, { imageDecode: true, mp4RangeParser: true })
  }
  return record(browser, scenario, {
    firstBrokerByteMilliseconds: metric(),
    p2pStartMilliseconds: metric(),
    relayAdmissionMilliseconds: metric(),
  }, { productionControllerAction: true, realTimer: true, syntheticSession: true })
}

function outputRecord(browser: string) {
  const originPrivateStaging = browser !== 'webkit'
  const heapTelemetry = browser === 'chromium'
  return record(browser, 'output-portable-opfs', {
    portableMilliseconds: metric(),
    ...(originPrivateStaging ? { opfsMilliseconds: metric() } : {}),
    ...(heapTelemetry ? { portableHeapPeakBytes: metric(), opfsHeapPeakBytes: metric() } : {}),
  }, { heapTelemetry, originPrivateStaging, portableDownload: true }, {
    ...(!originPrivateStaging ? { originPrivateStaging: 'not available' } : {}),
    ...(!heapTelemetry ? { heapTelemetry: 'not available' } : {}),
  })
}

function wideRecord(browser: string) {
  const heapTelemetry = browser === 'chromium'
  return record(browser, 'wide-directory-ui', {
    domNodeCount: metric(),
    ...(heapTelemetry ? { observedHeapBytes: metric() } : {}),
    renderMilliseconds: metric(),
  }, {
    heapTelemetry,
    indexedDbSpool: true,
    productionCatalogDecode: true,
    productionController: true,
    virtualPage: true,
  }, heapTelemetry ? {} : { heapTelemetry: 'not available' })
}

function routeRecord(browser: string) {
  const peer = browser !== 'webkit'
  return record(browser, peer ? 'real-relay-to-peer-hot-switch' : 'real-relay-fallback', {
    firstRelayByteMilliseconds: metric(),
    ...(peer ? { firstPeerByteMilliseconds: metric(), peerAfterRelayCutMilliseconds: metric() } : {}),
    peerAttemptStartMilliseconds: metric(),
    transferCompletionMilliseconds: metric(),
  }, { peerConnection: peer, realRelay: true }, peer ? {} : { peerConnection: 'not available' })
}

function record(
  browser: string,
  scenario: string,
  metrics: Record<string, TrendTestMetric>,
  capabilities: Record<string, boolean>,
  unavailable: Record<string, string> = {},
): TrendTestRecord {
  return {
    schema: 1,
    statistics: { percentileMethod: 'nearest-rank', decimalPlaces: 3 },
    browser,
    scenario,
    workload: { samples: 5 },
    capabilities,
    unavailable,
    metrics,
  }
}

function metric() {
  return { raw: [1, 2, 3, 4, 5], p50: 3, p95: 5 }
}

function requiredMetric(
  record: TrendTestRecord | undefined,
  name: string,
): TrendTestMetric {
  const value = record?.metrics[name]
  if (value === undefined) throw new Error(`valid fixture lost metric ${name}`)
  return value
}
