const SAMPLE_COUNT = 5
const BROWSERS = Object.freeze(['chromium', 'firefox', 'webkit'])
const CONNECTIVITY_METRICS = Object.freeze([
  'firstBrokerByteMilliseconds',
  'p2pStartMilliseconds',
  'relayAdmissionMilliseconds',
])

const COMMON_CONTRACTS = Object.freeze({
  'connectivity-preview-unknown': fixedContract(
    CONNECTIVITY_METRICS,
    { productionControllerAction: true, realTimer: true, syntheticSession: true },
  ),
  'connectivity-download-small': fixedContract(
    CONNECTIVITY_METRICS,
    { productionControllerAction: true, realTimer: true, syntheticSession: true },
  ),
  'connectivity-download-unknown': fixedContract(
    CONNECTIVITY_METRICS,
    { productionControllerAction: true, realTimer: true, syntheticSession: true },
  ),
  'connectivity-download-large': fixedContract(
    CONNECTIVITY_METRICS,
    { productionControllerAction: true, realTimer: true, syntheticSession: true },
  ),
  'media-preview': fixedContract(
    [
      'imageFirstFrameMilliseconds',
      'imageOpenMilliseconds',
      'videoMetadataMilliseconds',
      'videoSeekMilliseconds',
    ],
    { imageDecode: true, mp4RangeParser: true },
  ),
})

const ROUTE_SCENARIOS = Object.freeze({
  chromium: 'real-relay-to-peer-hot-switch',
  firefox: 'real-relay-to-peer-hot-switch',
  webkit: 'real-relay-fallback',
})

export function validateR8TrendRecords(records, sampleCount = SAMPLE_COUNT) {
  if (sampleCount !== SAMPLE_COUNT) fail(`sample count must be ${SAMPLE_COUNT}`)
  if (!Array.isArray(records)) fail('trend output must be an array')
  const expectedIdentities = expectedRecordIdentities()
  if (records.length !== expectedIdentities.size) {
    fail(`expected ${expectedIdentities.size} trend records, found ${records.length}`)
  }
  const observed = new Set()
  for (const record of records) {
    const value = requireObject(record, 'trend record')
    const browser = requireString(value.browser, 'trend browser')
    const scenario = requireString(value.scenario, 'trend scenario')
    const identity = `${browser}/${scenario}`
    if (!expectedIdentities.has(identity)) fail(`unexpected trend identity ${identity}`)
    if (observed.has(identity)) fail(`duplicate trend identity ${identity}`)
    observed.add(identity)
    validateRecord(value, browser, scenario, sampleCount)
  }
  for (const identity of expectedIdentities) {
    if (!observed.has(identity)) fail(`missing trend identity ${identity}`)
  }
  return Object.freeze({
    browsers: BROWSERS.length,
    records: records.length,
    sampleCount,
  })
}

function validateRecord(record, browser, scenario, sampleCount) {
  if (record.schema !== 1) fail(`${browser}/${scenario} has an unsupported schema`)
  const statistics = requireObject(record.statistics, `${browser}/${scenario} statistics`)
  if (statistics.percentileMethod !== 'nearest-rank' || statistics.decimalPlaces !== 3) {
    fail(`${browser}/${scenario} has an unsupported statistic contract`)
  }
  const workload = requireObject(record.workload, `${browser}/${scenario} workload`)
  if (workload.samples !== sampleCount) {
    fail(`${browser}/${scenario} workload does not declare ${sampleCount} samples`)
  }
  const capabilities = requireObject(record.capabilities, `${browser}/${scenario} capabilities`)
  const unavailable = requireObject(record.unavailable, `${browser}/${scenario} unavailable reasons`)
  const metrics = requireObject(record.metrics, `${browser}/${scenario} metrics`)
  const contract = contractFor(browser, scenario, capabilities)
  requireExactKeys(capabilities, Object.keys(contract.capabilities), `${browser}/${scenario} capabilities`)
  for (const [name, expected] of Object.entries(contract.capabilities)) {
    if (typeof capabilities[name] !== 'boolean') {
      fail(`${browser}/${scenario}/${name} capability is not boolean`)
    }
    if (expected !== null && capabilities[name] !== expected) {
      fail(`${browser}/${scenario}/${name} capability contradicts the fixed runtime contract`)
    }
  }
  const unavailableCapabilities = Object.entries(capabilities)
    .filter(([, available]) => available === false)
    .map(([name]) => name)
  requireExactKeys(unavailable, unavailableCapabilities, `${browser}/${scenario} unavailable reasons`)
  for (const name of unavailableCapabilities) {
    requireString(unavailable[name], `${browser}/${scenario}/${name} unavailable reason`)
  }
  const metricNames = contract.metrics(capabilities)
  requireExactKeys(metrics, metricNames, `${browser}/${scenario} metrics`)
  for (const name of metricNames) validateMetric(metrics[name], browser, scenario, name, sampleCount)
}

function contractFor(browser, scenario, capabilities) {
  if (scenario in COMMON_CONTRACTS) return COMMON_CONTRACTS[scenario]
  if (scenario === 'output-portable-opfs') {
    return {
      capabilities: {
        heapTelemetry: null,
        originPrivateStaging: null,
        portableDownload: true,
      },
      metrics: (observed) => [
        'portableMilliseconds',
        ...(observed.originPrivateStaging ? ['opfsMilliseconds'] : []),
        ...(observed.heapTelemetry ? ['portableHeapPeakBytes'] : []),
        ...(observed.heapTelemetry && observed.originPrivateStaging ? ['opfsHeapPeakBytes'] : []),
      ],
    }
  }
  if (scenario === 'wide-directory-ui') {
    return {
      capabilities: {
        heapTelemetry: null,
        indexedDbSpool: true,
        productionCatalogDecode: true,
        productionController: true,
        virtualPage: true,
      },
      metrics: (observed) => [
        'domNodeCount',
        ...(observed.heapTelemetry ? ['observedHeapBytes'] : []),
        'renderMilliseconds',
      ],
    }
  }
  if (scenario === ROUTE_SCENARIOS[browser]) {
    const peer = browser !== 'webkit'
    return {
      capabilities: { peerConnection: peer, realRelay: true },
      metrics: () => [
        'firstRelayByteMilliseconds',
        ...(peer ? ['firstPeerByteMilliseconds', 'peerAfterRelayCutMilliseconds'] : []),
        'peerAttemptStartMilliseconds',
        'transferCompletionMilliseconds',
      ],
    }
  }
  fail(`no trend contract exists for ${browser}/${scenario}`)
}

function validateMetric(metric, browser, scenario, name, sampleCount) {
  const value = requireObject(metric, `${browser}/${scenario}/${name}`)
  requireExactKeys(value, ['p50', 'p95', 'raw'], `${browser}/${scenario}/${name}`)
  if (!Array.isArray(value.raw) || value.raw.length !== sampleCount) {
    fail(`${browser}/${scenario}/${name} does not contain ${sampleCount} raw samples`)
  }
  for (const sample of value.raw) requireFiniteNonNegative(sample, `${browser}/${scenario}/${name}`)
  requireFiniteNonNegative(value.p50, `${browser}/${scenario}/${name} p50`)
  requireFiniteNonNegative(value.p95, `${browser}/${scenario}/${name} p95`)
  const sorted = [...value.raw].sort((left, right) => left - right)
  if (value.p50 !== nearestRank(sorted, 0.5) || value.p95 !== nearestRank(sorted, 0.95)) {
    fail(`${browser}/${scenario}/${name} percentiles do not match its raw samples`)
  }
}

function expectedRecordIdentities() {
  const identities = new Set()
  for (const browser of BROWSERS) {
    for (const scenario of Object.keys(COMMON_CONTRACTS)) identities.add(`${browser}/${scenario}`)
    identities.add(`${browser}/output-portable-opfs`)
    identities.add(`${browser}/wide-directory-ui`)
    identities.add(`${browser}/${ROUTE_SCENARIOS[browser]}`)
  }
  return identities
}

function fixedContract(metrics, capabilities) {
  return Object.freeze({ capabilities: Object.freeze(capabilities), metrics: () => [...metrics] })
}

function requireObject(value, label) {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) fail(`${label} is not an object`)
  return value
}

function requireString(value, label) {
  if (typeof value !== 'string' || value.trim().length === 0) fail(`${label} is empty`)
  return value
}

function requireFiniteNonNegative(value, label) {
  if (typeof value !== 'number' || !Number.isFinite(value) || value < 0) {
    fail(`${label} contains a non-finite or negative value`)
  }
}

function requireExactKeys(value, expected, label) {
  const actual = Object.keys(value).sort()
  const wanted = [...expected].sort()
  if (actual.length !== wanted.length || actual.some((key, index) => key !== wanted[index])) {
    fail(`${label} keys differ: expected ${wanted.join(', ')}, found ${actual.join(', ')}`)
  }
}

function nearestRank(sorted, percentile) {
  return sorted[Math.max(0, Math.ceil(percentile * sorted.length) - 1)]
}

function fail(message) {
  throw new Error(`R8 trend contract: ${message}`)
}
