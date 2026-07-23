import { expect, test } from '@playwright/test'

import {
  R8_PERFORMANCE_SAMPLE_COUNT,
  reportR8Trend,
  summarizeR8Metric,
} from './r8-trend'
import { R8_WIDE_PROGRESS_PREFIX } from './r8-wide-directory-source'

const OUTPUT_WORKLOAD_BYTES = 4 * 1024 * 1024
const OUTPUT_CHUNK_BYTES = 64 * 1024
const WIDE_DIRECTORY_ENTRIES = 1_048_576
const WIDE_DIRECTORY_DOM_ROW_BUDGET = 256
const WIDE_DIRECTORY_DOM_NODE_BUDGET = 2_048

test('records preview/download activation timing without turning wall time into policy', async ({
  browserName,
  page,
}) => {
  await page.goto('/')
  const evidence = await page.evaluate(async ({ sampleCount }) => {
    const brokerPath = '/src/content/v2-broker.ts'
    const controllerPath = '/src/ui/v2-controller.ts'
    const geometryPath = '/src/content/geometry.ts'
    const policyPath = '/src/connectivity/v2-receiver-policy.ts'
    const selectionPath = '/src/catalog/v2-selection.ts'
    const timingSupportPath = '/test/performance/r8-controller-timing-support.ts'
    const [
      brokerModule,
      controllerModule,
      geometryModule,
      policyModule,
      selectionModule,
      timingSupport,
    ] = await Promise.all([
      import(brokerPath) as Promise<typeof import('../../src/content/v2-broker')>,
      import(controllerPath) as Promise<typeof import('../../src/ui/v2-controller')>,
      import(geometryPath) as Promise<typeof import('../../src/content/geometry')>,
      import(policyPath) as Promise<typeof import('../../src/connectivity/v2-receiver-policy')>,
      import(selectionPath) as Promise<typeof import('../../src/catalog/v2-selection')>,
      import(timingSupportPath) as Promise<typeof import('./r8-controller-timing-support')>,
    ])
    type BlockDemand = import('../../src/content/v2-broker').V2BlockDemand
    type BlockLane = import('../../src/content/v2-broker').V2BlockLane
    type BlockRecord = import('../../src/content/v2-records').V2BlockRecord
    type CatalogEntry = import('../../src/catalog/v2-records').V2CatalogEntry
    type ConnectivityActivation = import('../../src/connectivity/v2-receiver-policy').V2ConnectivityActivation
    type LaneChange = import('../../src/session/v2-runtime-types').V2LaneChange
    type OfferFactory = import('../../src/connectivity/peer-offer').OfferChannelFactory
    type PeerChannel = import('../../src/connectivity/peer-channel').PeerChannel
    type ReceiverSession = import('../../src/session/v2-runtime').V2ReceiverSessionRuntime
    type BrowserGateway = import('../../src/ui/v2-gateway').V2BrowserReceiverGateway
    type BrowseDirectory = import('../../src/ui/v2-gateway').V2BrowseDirectory
    type JoinedShare = import('../../src/ui/v2-gateway').V2JoinedBrowserShare

    const scenarios = [
      { intent: 'preview', sizeClass: 'unknown', relaySynchronous: false },
      { intent: 'download', sizeClass: 'small', relaySynchronous: true },
      { intent: 'download', sizeClass: 'unknown', relaySynchronous: false },
      { intent: 'download', sizeClass: 'large', relaySynchronous: false },
    ] as const

    class PendingOffers implements OfferFactory {
      startedAt: number | undefined

      offer(_route: Parameters<OfferFactory['offer']>[0], signal: AbortSignal): Promise<PeerChannel> {
        this.startedAt = performance.now()
        return new Promise((_resolve, reject) => {
          const abort = () => reject(
            signal.reason ?? new DOMException('Performance peer stopped', 'AbortError'),
          )
          signal.addEventListener('abort', abort, { once: true })
          if (signal.aborted) abort()
        })
      }
    }

    class FakeSession {
      readonly initialLaneId = 1
      readonly listeners = new Set<(change: LaneChange) => void>()

      laneIds(): readonly number[] { return [this.initialLaneId] }

      subscribeLaneChanges(listener: (change: LaneChange) => void): () => void {
        this.listeners.add(listener)
        return () => this.listeners.delete(listener)
      }
    }

    function identity(first: number): Uint8Array<ArrayBuffer> {
      const value = new Uint8Array(16)
      value[0] = first
      return value
    }

    async function runSample(
      scenario: typeof scenarios[number],
      sampleIndex: number,
    ) {
      const session = new FakeSession()
      const offers = new PendingOffers()
      const descriptor = Object.freeze({
        shareInstance: identity(1),
        shareInstanceId: 'r8-share',
        fileId: identity(2),
        fileIdText: `r8-file-${scenario.intent}-${scenario.sizeClass}-${sampleIndex}`,
        fileRevision: identity(3),
        fileRevisionText: 'r8-revision',
        exactSize: 1n,
        geometry: new geometryModule.FileGeometry(1n, 1n),
      })
      const leaseId = identity(4)
      let relayAdmittedAt: number | undefined
      let firstBrokerByteAt: number | undefined
      let observedRoute: string | undefined
      const lanes = new brokerModule.V2LaneSet({
        onBlockFetched: (observation) => {
          firstBrokerByteAt ??= performance.now()
          observedRoute = observation.route
        },
      })
      class Lane implements BlockLane {
        readonly id: number

        constructor(id: number) { this.id = id }

        async fetchBlock(demand: BlockDemand, signal: AbortSignal): Promise<BlockRecord> {
          signal.throwIfAborted()
          return Object.freeze({
            descriptor: demand.descriptor,
            localBlockIndex: demand.localBlockIndex,
            data: Uint8Array.of(0x52),
          })
        }
      }
      const broker = new brokerModule.V2BlockBroker(lanes)
      const connectivity = new policyModule.V2ReceiverConnectivity({
        session: session as unknown as ReceiverSession,
        lanes,
        createBlockLane: (laneId) => {
          if (laneId === session.initialLaneId) relayAdmittedAt ??= performance.now()
          return new Lane(laneId)
        },
        offers,
        randomBytes: (length) => new Uint8Array(length).fill(sampleIndex + 1),
      })
      const selection = new selectionModule.V2SelectionPolicy(true)
      const fileEntry: Extract<CatalogEntry, { kind: 'file' }> = Object.freeze({
        kind: 'file',
        id: identity(5),
        idText: descriptor.fileIdText,
        name: `${descriptor.fileIdText}.bin`,
        expectedSize: scenario.sizeClass === 'large' ? 8n * 1024n * 1024n : 1n,
      })
      const directoryEntry: Extract<CatalogEntry, { kind: 'directory' }> = Object.freeze({
        kind: 'directory',
        id: identity(6),
        idText: `${descriptor.fileIdText}-directory`,
        name: `${descriptor.fileIdText}-directory`,
      })
      const visibleEntry = scenario.intent === 'download' && scenario.sizeClass === 'unknown'
        ? directoryEntry
        : fileEntry
      const root: BrowseDirectory = Object.freeze({
        id: identity(7),
        idText: 'r8-root',
        name: 'Shared files',
        path: Object.freeze([]),
        ancestry: Object.freeze(['r8-root']),
      })
      let activation: ConnectivityActivation | undefined
      let activationBoundaryAt: number | undefined
      const controllerEvents: string[] = []
      const joined = {
        descriptor: Object.freeze({ syntheticRootId: root.idText }),
        recoveryIdentity: `r8-recovery-${sampleIndex}`,
        selection,
        rootDirectory: () => root,
        childDirectory: () => { throw new Error('Timing fixture does not browse child directories') },
        subscribeCatalogScanProgress: () => () => undefined,
        page: async (directory: BrowseDirectory) => {
          return Object.freeze({
            directory,
            pageIndex: 0,
            pageCount: 1,
            entryCount: 1,
            omittedCount: 0n,
            entries: Object.freeze([visibleEntry]),
          })
        },
        beginPreviewConnectivity: () => beginFromController('preview', 'unknown'),
        beginDownloadConnectivity: (sizeClass: 'small' | 'large' | 'unknown') =>
          beginFromController('download', sizeClass),
        preview: (
          _entry: CatalogEntry,
          _connectivity: ConnectivityActivation,
          signal: AbortSignal,
        ) => timingSupport.pendingUntilAbort(signal, 'Preview stopped'),
        transferJob: () => ({
          run: (signal: AbortSignal) => timingSupport.pendingUntilAbort(signal, 'Transfer stopped'),
        }),
        close: async () => undefined,
      } as unknown as JoinedShare
      const gateway = {
        join: async () => joined,
      } as unknown as BrowserGateway
      const controller = new controllerModule.V2ReceiverController(gateway)
      const browsing = timingSupport.waitForBrowsing(controller)
      controller.initialize({
        capabilityInput: 'r8-controller-boundary',
        pageUrl: 'https://receiver.invalid/s/r8-controller-boundary',
      })
      await browsing
      controller.chooseOutput('download')
      const actionInvokedAt = performance.now()
      if (scenario.intent === 'preview') controller.previewFile(fileEntry.idText)
      else controller.startDownload()
      const actionReturnedAt = performance.now()
      try {
        const record = await broker.readBlock({ descriptor, leaseId, localBlockIndex: 0n }, {
          routes: activation!.routes,
          priority: scenario.intent,
        })
        if (record.data[0] !== 0x52 || observedRoute !== 'relay') {
          throw new Error('Connectivity trend did not return the expected relay broker byte')
        }
        if (offers.startedAt === undefined || relayAdmittedAt === undefined ||
            firstBrokerByteAt === undefined || activationBoundaryAt === undefined ||
            activation === undefined) {
          throw new Error('Connectivity trend lost a required lifecycle observation')
        }
        if (controllerEvents[0] !== `activate-${scenario.intent}`) {
          throw new Error('Controller activation was not the first post-guard action')
        }
        return {
          intent: scenario.intent,
          sizeClass: scenario.sizeClass,
          p2pStartMilliseconds: offers.startedAt - activationBoundaryAt,
          relayAdmissionMilliseconds: relayAdmittedAt - activationBoundaryAt,
          firstBrokerByteMilliseconds: firstBrokerByteAt - activationBoundaryAt,
          activationWasSynchronous: activationBoundaryAt >= actionInvokedAt &&
            activationBoundaryAt <= actionReturnedAt,
          p2pStartedDuringAction: offers.startedAt <= actionReturnedAt,
          relaySynchronous: relayAdmittedAt <= actionReturnedAt,
          observedRoute,
        }
      } finally {
        await controller.dispose()
        activation?.close()
        broker.close()
        await connectivity.close()
        lanes.close()
      }

      function beginFromController(
        intent: 'preview' | 'download',
        sizeClass: 'small' | 'large' | 'unknown',
      ): ConnectivityActivation {
        if (activation !== undefined) throw new Error('Controller started connectivity more than once')
        controllerEvents.push(`activate-${intent}`)
        activationBoundaryAt = performance.now()
        activation = connectivity.begin(intent, sizeClass)
        return activation
      }
    }

    const restoreOutputPorts = hideNativeOutputPorts()
    try {
      const samples: Awaited<ReturnType<typeof runSample>>[] = []
      for (let sampleIndex = 0; sampleIndex < sampleCount; sampleIndex += 1) {
        // One scenario batch shares the same event-loop window, while sequential
        // batches remain independent observations for percentile calculation.
        samples.push(...await Promise.all(scenarios.map(
          (scenario) => runSample(scenario, sampleIndex),
        )))
      }
      return {
        controllerBoundary: 'V2ReceiverController first post-guard connectivity activation',
        fallbackPolicyMilliseconds: policyModule.V2_RELAY_CONTENT_FALLBACK_MILLISECONDS,
        scenarios,
        samples,
      }
    } finally {
      restoreOutputPorts()
    }

    function hideNativeOutputPorts(): () => void {
      const storage = navigator.storage as StorageManager | undefined
      const restorers = [
        hideProperty(window, 'showDirectoryPicker'),
        hideProperty(window, 'showSaveFilePicker'),
        ...(storage === undefined ? [] : [hideProperty(storage, 'getDirectory')]),
      ]
      return () => {
        for (const restore of restorers.reverse()) restore()
      }
    }

    function hideProperty(target: object, property: PropertyKey): () => void {
      const previous = Object.getOwnPropertyDescriptor(target, property)
      Object.defineProperty(target, property, {
        configurable: true,
        value: undefined,
        writable: true,
      })
      return () => {
        if (previous === undefined) Reflect.deleteProperty(target, property)
        else Object.defineProperty(target, property, previous)
      }
    }
  }, { sampleCount: R8_PERFORMANCE_SAMPLE_COUNT })

  expect(evidence.fallbackPolicyMilliseconds).toBe(8_000)
  for (const scenario of evidence.scenarios) {
    const samples = evidence.samples.filter(
      (sample) => sample.intent === scenario.intent && sample.sizeClass === scenario.sizeClass,
    )
    expect(samples).toHaveLength(R8_PERFORMANCE_SAMPLE_COUNT)
    expect(samples.every((sample) => sample.activationWasSynchronous)).toBe(true)
    expect(samples.every((sample) => sample.p2pStartedDuringAction)).toBe(true)
    expect(samples.every((sample) => sample.relaySynchronous === scenario.relaySynchronous)).toBe(true)
    expect(samples.every((sample) => sample.observedRoute === 'relay')).toBe(true)
    reportR8Trend({
      browser: browserName,
      scenario: `connectivity-${scenario.intent}-${scenario.sizeClass}`,
      workload: {
        samples: R8_PERFORMANCE_SAMPLE_COUNT,
        scenarioBatchConcurrency: evidence.scenarios.length,
        fallbackPolicyMilliseconds: evidence.fallbackPolicyMilliseconds,
        t0Boundary: evidence.controllerBoundary,
        firstByteBoundary: 'production-broker-after-successful-lane-fetch',
      },
      capabilities: {
        productionControllerAction: true,
        realTimer: true,
        syntheticSession: true,
      },
      unavailable: {},
      metrics: {
        p2pStartMilliseconds: summarizeR8Metric(samples.map((sample) => sample.p2pStartMilliseconds)),
        relayAdmissionMilliseconds: summarizeR8Metric(samples.map((sample) => sample.relayAdmissionMilliseconds)),
        firstBrokerByteMilliseconds: summarizeR8Metric(samples.map((sample) => sample.firstBrokerByteMilliseconds)),
      },
    })
  }
})

test('records bounded portable and production OPFS output trends where available', async ({
  browserName,
  page,
}) => {
  await page.goto('/')
  const evidence = await page.evaluate(async ({ chunkBytes, sampleCount, workloadBytes }) => {
    const portablePath = '/src/output/portable/browser-download.ts'
    const originPrivatePath = '/src/output/origin-private/session.ts'
    const outputPath = '/src/ui/v2-output.ts'
    const [portable, originPrivate, output] = await Promise.all([
      import(portablePath) as Promise<typeof import('../../src/output/portable/browser-download')>,
      import(originPrivatePath) as Promise<typeof import('../../src/output/origin-private/session')>,
      import(outputPath) as Promise<typeof import('../../src/ui/v2-output')>,
    ])
    const payload = new Uint8Array(workloadBytes).map((_value, index) => index & 0xff)
    const expectedDigest = digestHex(await crypto.subtle.digest('SHA-256', payload))
    const portableHeapPeakBytes: number[] = []
    const portableMilliseconds: number[] = []

    for (let sample = 0; sample < sampleCount; sample += 1) {
      let heapPeakBytes = currentHeapBytes()
      let published: Blob | undefined
      const stream = portable.createBoundedPortableDownloadStream('r8-output.bin', {
        createBlob: (parts) => new Blob([...parts], { type: 'application/octet-stream' }),
        publish: (_name, blob) => { published = blob },
      })
      const writer = stream.getWriter()
      const startedAt = performance.now()
      for (let offset = 0; offset < payload.length; offset += chunkBytes) {
        await writer.write(payload.subarray(offset, offset + chunkBytes))
        heapPeakBytes = maximumDefined(heapPeakBytes, currentHeapBytes())
      }
      await writer.close()
      heapPeakBytes = maximumDefined(heapPeakBytes, currentHeapBytes())
      portableMilliseconds.push(performance.now() - startedAt)
      if (published === undefined || published.size !== payload.byteLength) {
        throw new Error('Portable output did not publish the exact bounded payload')
      }
      const actual = await published.arrayBuffer()
      heapPeakBytes = maximumDefined(heapPeakBytes, currentHeapBytes())
      if (digestHex(await crypto.subtle.digest('SHA-256', actual)) !== expectedDigest) {
        throw new Error('Portable output changed payload bytes')
      }
      heapPeakBytes = maximumDefined(heapPeakBytes, currentHeapBytes())
      if (heapPeakBytes !== undefined) portableHeapPeakBytes.push(heapPeakBytes)
    }

    const capabilities = output.browserV2OutputCapabilities()
    const opfsHeapPeakBytes: number[] = []
    const opfsMilliseconds: number[] = []
    let opfsUnavailable = ''
    if (capabilities.originPrivateStaging) {
      for (let sample = 0; sample < sampleCount; sample += 1) {
        let heapPeakBytes = currentHeapBytes()
        let exportedDigest = ''
        let exportedBytes = -1
        const session = await originPrivate.openOriginPrivateOutputSession({
          outputSessionId: `r8-opfs-${sample}-${crypto.randomUUID()}`,
          exporter: {
            export: async (snapshot) => {
              let staged
              for await (const file of snapshot.files()) {
                staged = file
                break
              }
              if (staged === undefined) throw new Error('OPFS trend staged no output file')
              const file = await staged.read()
              exportedBytes = file.size
              const exportedBuffer = await file.arrayBuffer()
              heapPeakBytes = maximumDefined(heapPeakBytes, currentHeapBytes())
              exportedDigest = digestHex(await crypto.subtle.digest('SHA-256', exportedBuffer))
              heapPeakBytes = maximumDefined(heapPeakBytes, currentHeapBytes())
              return originPrivate.ORIGIN_PRIVATE_EXPORT_COMPLETE
            },
          },
        })
        const startedAt = performance.now()
        const begun = await session.beginFile({
          source: {
            shareInstance: 'r8-share',
            fileId: `r8-file-${sample}`,
            fileRevision: 'r8-revision',
          },
          path: ['r8-output.bin'],
          exactSize: BigInt(payload.byteLength),
        })
        for (let offset = 0; offset < payload.length; offset += chunkBytes) {
          await begun.transaction.writeRange(BigInt(offset), payload.subarray(offset, offset + chunkBytes))
          heapPeakBytes = maximumDefined(heapPeakBytes, currentHeapBytes())
        }
        const checkpoint = await begun.transaction.checkpoint()
        if (!checkpoint.covers({ start: 0n, end: BigInt(payload.byteLength) })) {
          throw new Error('OPFS trend checkpoint did not cover its exact payload')
        }
        await begun.transaction.commit()
        await session.finishJob({
          status: 'Succeeded',
          failures: [],
          failureCount: 0,
          omittedFailureCount: 0,
        }, new AbortController().signal)
        heapPeakBytes = maximumDefined(heapPeakBytes, currentHeapBytes())
        opfsMilliseconds.push(performance.now() - startedAt)
        if (exportedBytes !== payload.byteLength || exportedDigest !== expectedDigest) {
          throw new Error('OPFS output changed the staged payload during export')
        }
        if (heapPeakBytes !== undefined) opfsHeapPeakBytes.push(heapPeakBytes)
      }
    } else {
      opfsUnavailable = 'originPrivateStaging capability is unavailable in this engine'
    }

    return {
      capabilities,
      opfsMilliseconds,
      opfsHeapPeakBytes,
      opfsUnavailable,
      portableLimitBytes: portable.PORTABLE_DOWNLOAD_MAXIMUM_BYTES,
      portableHeapPeakBytes,
      portableMilliseconds,
    }

    function digestHex(buffer: ArrayBuffer): string {
      return [...new Uint8Array(buffer)]
        .map((byte) => byte.toString(16).padStart(2, '0'))
        .join('')
    }

    function currentHeapBytes(): number | undefined {
      const memory = performance as Performance & { memory?: { readonly usedJSHeapSize: number } }
      return memory.memory?.usedJSHeapSize
    }

    function maximumDefined(
      left: number | undefined,
      right: number | undefined,
    ): number | undefined {
      if (left === undefined) return right
      if (right === undefined) return left
      return Math.max(left, right)
    }
  }, {
    chunkBytes: OUTPUT_CHUNK_BYTES,
    sampleCount: R8_PERFORMANCE_SAMPLE_COUNT,
    workloadBytes: OUTPUT_WORKLOAD_BYTES,
  })

  expect(evidence.portableLimitBytes).toBe(64 * 1024 * 1024)
  expect(evidence.portableMilliseconds).toHaveLength(R8_PERFORMANCE_SAMPLE_COUNT)
  expect(evidence.capabilities.portableDownload).toBe(true)
  expect(evidence.opfsMilliseconds).toHaveLength(
    evidence.capabilities.originPrivateStaging ? R8_PERFORMANCE_SAMPLE_COUNT : 0,
  )
  expect([0, R8_PERFORMANCE_SAMPLE_COUNT]).toContain(evidence.portableHeapPeakBytes.length)
  expect([0, R8_PERFORMANCE_SAMPLE_COUNT]).toContain(evidence.opfsHeapPeakBytes.length)
  if (!evidence.capabilities.originPrivateStaging) expect(evidence.opfsHeapPeakBytes).toHaveLength(0)
  const metrics = {
    portableMilliseconds: summarizeR8Metric(evidence.portableMilliseconds),
    ...(evidence.opfsMilliseconds.length === 0
      ? {}
      : { opfsMilliseconds: summarizeR8Metric(evidence.opfsMilliseconds) }),
    ...(evidence.portableHeapPeakBytes.length === 0
      ? {}
      : { portableHeapPeakBytes: summarizeR8Metric(evidence.portableHeapPeakBytes) }),
    ...(evidence.opfsHeapPeakBytes.length === 0
      ? {}
      : { opfsHeapPeakBytes: summarizeR8Metric(evidence.opfsHeapPeakBytes) }),
  }
  reportR8Trend({
    browser: browserName,
    scenario: 'output-portable-opfs',
    workload: {
      samples: R8_PERFORMANCE_SAMPLE_COUNT,
      bytesPerSample: OUTPUT_WORKLOAD_BYTES,
      chunkBytes: OUTPUT_CHUNK_BYTES,
      portableHardLimitBytes: evidence.portableLimitBytes,
    },
    capabilities: {
      portableDownload: evidence.capabilities.portableDownload,
      originPrivateStaging: evidence.capabilities.originPrivateStaging,
      heapTelemetry: evidence.portableHeapPeakBytes.length > 0,
    },
    unavailable: {
      ...(evidence.opfsUnavailable === '' ? {} : { originPrivateStaging: evidence.opfsUnavailable }),
      ...(evidence.portableHeapPeakBytes.length > 0
        ? {}
        : { heapTelemetry: 'performance.memory is unavailable in this engine' }),
    },
    metrics,
  })
})

test('keeps a million-entry directory virtual while measuring one committed UI page', async ({
  browserName,
  page,
}, testInfo) => {
  // Authentication plus durable spooling is intentionally the workload; an
  // arbitrary wall-clock cutoff would turn host speed into the acceptance rule.
  testInfo.setTimeout(0)
  page.on('console', (message) => {
    if (message.text().startsWith(R8_WIDE_PROGRESS_PREFIX)) console.log(message.text())
  })
  await page.goto('/')
  const evidence = await page.evaluate(async ({ directoryEntries, sampleCount }) => {
    const harnessPath = '/test/performance/r8-wide-directory-harness.tsx'
    const harness = await import(harnessPath) as typeof import('./r8-wide-directory-harness')
    return harness.measureR8WideDirectoryUi(directoryEntries, sampleCount)
  }, { directoryEntries: WIDE_DIRECTORY_ENTRIES, sampleCount: R8_PERFORMANCE_SAMPLE_COUNT })

  expect(evidence.pageEntries).toBe(WIDE_DIRECTORY_DOM_ROW_BUDGET)
  expect(evidence.pageCount).toBe(4_096)
  expect(evidence.probe).toMatchObject({
    generatedPages: evidence.pageCount,
    generatedEntries: WIDE_DIRECTORY_ENTRIES,
    stagedPages: evidence.pageCount,
    stagedEntries: WIDE_DIRECTORY_ENTRIES,
    stagedPageOwnershipRecords: evidence.pageCount,
    stagedNodeOwnershipKeys: WIDE_DIRECTORY_ENTRIES,
    stagedNameOwnershipKeys: WIDE_DIRECTORY_ENTRIES,
    loadedPages: evidence.pageCount + 1,
    maximumGeneratedRows: WIDE_DIRECTORY_DOM_ROW_BUDGET,
    maximumSourceSenderObjects: 1,
    maximumStoreBoundaryPages: 1,
    maximumStoreBoundaryRows: WIDE_DIRECTORY_DOM_ROW_BUDGET,
    maximumLoadedPageRows: WIDE_DIRECTORY_DOM_ROW_BUDGET,
    maximumControllerRows: WIDE_DIRECTORY_DOM_ROW_BUDGET,
    maximumControllerEntryRecords: WIDE_DIRECTORY_DOM_ROW_BUDGET,
    maximumControllerRootCandidates: 0,
    maximumDomRows: WIDE_DIRECTORY_DOM_ROW_BUDGET,
    protocolFailures: 0,
  })
  expect(evidence.probe.maximumDomNodes).toBeLessThanOrEqual(WIDE_DIRECTORY_DOM_NODE_BUDGET)
  expect(evidence.renderMilliseconds).toHaveLength(R8_PERFORMANCE_SAMPLE_COUNT)
  expect(evidence.domNodeCounts).toHaveLength(R8_PERFORMANCE_SAMPLE_COUNT)
  expect(evidence.renderedRowCounts).toEqual(
    Array.from({ length: R8_PERFORMANCE_SAMPLE_COUNT }, () => evidence.pageEntries),
  )
  expect(evidence.renderedRowCounts.every((count) => count <= WIDE_DIRECTORY_DOM_ROW_BUDGET)).toBe(true)
  expect(evidence.renderedRowCounts.every((count) => count < WIDE_DIRECTORY_ENTRIES)).toBe(true)
  expect([0, R8_PERFORMANCE_SAMPLE_COUNT]).toContain(evidence.heapBytes.length)
  reportR8Trend({
    browser: browserName,
    scenario: 'wide-directory-ui',
    workload: {
      samples: R8_PERFORMANCE_SAMPLE_COUNT,
      directoryEntries: WIDE_DIRECTORY_ENTRIES,
      committedPageEntries: evidence.pageEntries,
      pages: evidence.pageCount,
      generatedEntries: evidence.probe.generatedEntries,
      stagedEntries: evidence.probe.stagedEntries,
      stagedPageOwnershipRecords: evidence.probe.stagedPageOwnershipRecords,
      stagedNodeOwnershipKeys: evidence.probe.stagedNodeOwnershipKeys,
      stagedNameOwnershipKeys: evidence.probe.stagedNameOwnershipKeys,
      loadedPages: evidence.probe.loadedPages,
      maximumGeneratedRows: evidence.probe.maximumGeneratedRows,
      maximumSourceSenderObjects: evidence.probe.maximumSourceSenderObjects,
      maximumStoreBoundaryPages: evidence.probe.maximumStoreBoundaryPages,
      maximumStoreBoundaryRows: evidence.probe.maximumStoreBoundaryRows,
      maximumControllerRows: evidence.probe.maximumControllerRows,
      maximumControllerEntryRecords: evidence.probe.maximumControllerEntryRecords,
      maximumControllerRootCandidates: evidence.probe.maximumControllerRootCandidates,
      maximumDomRows: evidence.probe.maximumDomRows,
      domNodeBudget: WIDE_DIRECTORY_DOM_NODE_BUDGET,
    },
    capabilities: {
      heapTelemetry: evidence.heapBytes.length > 0,
      indexedDbSpool: true,
      productionCatalogDecode: true,
      productionController: true,
      virtualPage: true,
    },
    unavailable: evidence.heapBytes.length > 0
      ? {}
      : { heapTelemetry: 'performance.memory is unavailable in this engine' },
    metrics: {
      renderMilliseconds: summarizeR8Metric(evidence.renderMilliseconds),
      domNodeCount: summarizeR8Metric(evidence.domNodeCounts),
      ...(evidence.heapBytes.length === 0
        ? {}
        : { observedHeapBytes: summarizeR8Metric(evidence.heapBytes) }),
    },
  })
})
