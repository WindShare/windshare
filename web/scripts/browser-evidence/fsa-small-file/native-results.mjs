#!/usr/bin/env node
import { mkdir, readFile, writeFile } from 'node:fs/promises'
import { dirname, resolve } from 'node:path'

import {
  BASELINE_RESULT_SCHEMA,
  PRODUCT_RESULT_SCHEMA,
  validateBaselineResult,
  validateProductResult,
} from './results.mjs'
import { loadCanonicalWorkload } from './workload.mjs'

function parseArguments(argv) {
  const [command, ...rest] = argv
  const values = { baseline: [], product: [], rawProduct: [] }
  for (let index = 0; index < rest.length; index += 1) {
    const argument = rest[index]
    const value = rest[++index]
    if (value === undefined || !argument.startsWith('--')) throw new Error(`Invalid argument: ${argument}`)
    const name = argument.slice(2).replace(/-([a-z])/g, (_, letter) => letter.toUpperCase())
    if (name === 'baseline' || name === 'product' || name === 'rawProduct') values[name].push(value)
    else values[name] = value
  }
  return { command, values }
}

async function json(path) {
  return JSON.parse(await readFile(resolve(path), 'utf8'))
}

async function emit(path, value) {
  const output = resolve(path)
  await mkdir(dirname(output), { recursive: true })
  await writeFile(output, `${JSON.stringify(value, null, 2)}\n`, { encoding: 'utf8', flag: 'wx' })
}

function required(values, name) {
  const value = values[name]
  if (typeof value !== 'string' || value.length === 0) throw new Error(`--${name} is required`)
  return value
}

function repetition(values) {
  const value = Number.parseInt(required(values, 'repetition'), 10)
  if (!Number.isSafeInteger(value) || value < 0) throw new Error('--repetition must be non-negative')
  return value
}

function phaseDifference(milestones, left, right) {
  const start = Number(milestones[left])
  const end = Number(milestones[right])
  if (!Number.isFinite(start) || !Number.isFinite(end) || end < start) {
    throw new Error(`Invalid product milestone interval: ${left} -> ${right}`)
  }
  return end - start
}

const CLAIM_PHASES = Object.freeze([
  'classification',
  'inspection_union',
  'reclassification',
  'installation',
])

function requireClaimPhases(summary) {
  const phases = summary?.claim_batches?.phases
  if (phases === undefined || JSON.stringify(Object.keys(phases).sort()) !== JSON.stringify([...CLAIM_PHASES].sort())) {
    throw new Error('Native summary does not contain the exact four claim phases')
  }
  for (const phase of CLAIM_PHASES) {
    for (const field of [
      'batch_count',
      'member_count',
      'queue_ms',
      'run_ms',
      'active_ms',
      'overlap_ms',
      'maximum_active',
      'active_at_completion',
    ]) {
      if (phases[phase]?.[field] === undefined) {
        throw new Error(`Native claim phase ${phase} is missing ${field}`)
      }
    }
  }
}

async function assemble(values) {
  const [raw, hostVerification, environment, canonical] = await Promise.all([
    json(required(values, 'raw')),
    json(required(values, 'host')),
    json(required(values, 'environment')),
    loadCanonicalWorkload(),
  ])
  const run = repetition(values)
  const common = {
    runId: required(values, 'runId'),
    pairId: required(values, 'pairId'),
    repetition: run,
    warmup: run === 0,
    workloadSha256: canonical.sha256,
    environment,
    concurrency: 8,
    hostVerification,
  }
  if (values.kind === 'baseline') {
    const timing = raw.result?.timing
    if (raw.mode !== 'baseline' || raw.result?.ok !== true || timing === undefined) {
      throw new Error('Raw baseline evidence is not successful')
    }
    return validateBaselineResult({
      schema: BASELINE_RESULT_SCHEMA,
      ...common,
      timing: {
        startMilestone: 'authority-acquired',
        endMilestone: 'baseline-complete',
        durationMilliseconds: timing.durationMilliseconds,
        pickerWaitExcluded: true,
      },
      phaseDurations: {
        authorityToFirstWriteMilliseconds: timing.authorityToFirstWriteMilliseconds,
        firstWriteToLastByteMilliseconds: timing.firstWriteToLastByteMilliseconds,
        lastByteToCompletedMilliseconds: timing.lastByteToCompletedMilliseconds,
      },
      outcome: { lifecycle: 'Completed', bytesWritten: 6_762_858, route: 'pure-fsa' },
    })
  }
  if (values.kind !== 'product') throw new Error('--kind must be baseline or product')
  const product = raw.result
  const summary = product?.diagnostics?.summary
  const milestones = summary?.milestones
  if (raw.mode !== 'product' || product?.ok !== true || product.lifecycle !== 'Published' || milestones === undefined) {
    throw new Error('Raw product evidence is not Published')
  }
  const durationMilliseconds = phaseDifference(milestones, 'authority_acquired', 'published')
  return validateProductResult({
    schema: PRODUCT_RESULT_SCHEMA,
    ...common,
    timing: {
      startMilestone: 'authority-acquired',
      endMilestone: 'published',
      durationMilliseconds,
      pickerWaitExcluded: true,
    },
    phaseDurations: {
      authorityToFirstContentRequestMilliseconds: phaseDifference(milestones, 'authority_acquired', 'first_content_request'),
      firstContentRequestToFirstWriteMilliseconds: phaseDifference(milestones, 'first_content_request', 'first_write'),
      firstWriteToLastByteMilliseconds: phaseDifference(milestones, 'first_write', 'last_byte'),
      lastByteToLastFinalTransactionMilliseconds: phaseDifference(milestones, 'last_byte', 'last_final'),
      lastFinalTransactionToSettlementMilliseconds: phaseDifference(milestones, 'last_final', 'settlement_started'),
      settlementToPublishedMilliseconds: phaseDifference(milestones, 'settlement_started', 'published'),
    },
    outcome: {
      lifecycle: 'Published',
      bytesWritten: Number(summary.bytes.final),
      artifactRoute: 'DirectTree',
      fallbackRoute: null,
      partial: false,
      needsAttention: false,
    },
  })
}

function median(values) {
  const ordered = [...values].sort((left, right) => left - right)
  const middle = Math.floor(ordered.length / 2)
  return ordered.length % 2 === 0 ? (ordered[middle - 1] + ordered[middle]) / 2 : ordered[middle]
}

function distribution(values) {
  return {
    minimum: Math.min(...values),
    median: median(values),
    maximum: Math.max(...values),
  }
}

function phaseDistributions(results) {
  const measured = results.filter(result => !result.warmup)
  return Object.fromEntries(Object.keys(measured[0].phaseDurations).map(name => [
    name,
    distribution(measured.map(result => result.phaseDurations[name])),
  ]))
}

async function summarizeNative(values) {
  const [paired, baselines, products, raws] = await Promise.all([
    json(required(values, 'paired')),
    Promise.all(values.baseline.map(json)),
    Promise.all(values.product.map(json)),
    Promise.all(values.rawProduct.map(json)),
  ])
  baselines.forEach(validateBaselineResult)
  products.forEach(validateProductResult)
  if (baselines.length !== products.length || products.length !== raws.length) {
    throw new Error('Native observation inputs must have matching pair counts')
  }
  const measuredRaw = raws.filter((_, index) => !products[index].warmup)
  const summaries = measuredRaw.map(raw => raw.result.diagnostics.summary)
  const invariants = measuredRaw.map(raw => raw.result.publicationInvariants)
  for (const summary of summaries) {
    for (const field of [
      'queue',
      'namespace_by_kind',
      'file_pipeline',
      'claim_batches',
      'output_resources',
      'counter_overflowed',
    ]) {
      if (summary?.[field] === undefined) throw new Error(`Native summary is missing ${field}`)
    }
    requireClaimPhases(summary)
  }
  return {
    schema: 'windshare/fsa-small-file-native-observations/v1',
    createdAt: new Date().toISOString(),
    pairedEvidence: paired,
    phaseTails: {
      baselineMilliseconds: phaseDistributions(baselines),
      productMilliseconds: phaseDistributions(products),
    },
    settings: {
      baselineConcurrency: 8,
      productMaximumConcurrentFilePipelines: 15,
      productMaximumActiveNativeWriters: 8,
      productMaximumConcurrentInitialClaimInspections: 3,
      productInitialClaimBatchMode: 'single-batch',
    },
    product: {
      peakActiveWriters: distribution(summaries.map(summary => summary.peaks.active_writers)),
      peakActiveNamespace: distribution(summaries.map(summary => summary.peaks.active_namespace)),
      queue: summaries.map(summary => summary.queue),
      namespaceByKind: summaries.map(summary => summary.namespace_by_kind),
      filePipelines: summaries.map(summary => summary.file_pipeline),
      checkpoints: summaries.map(summary => summary.checkpoints),
      finalTransactions: summaries.map(summary => summary.final_transactions),
      ledgers: summaries.map(summary => summary.ledger),
      revisionOpens: summaries.map(summary => summary.revision_opens),
      claimBatches: summaries.map(summary => summary.claim_batches),
      outputResources: summaries.map(summary => summary.output_resources),
      counterOverflowed: summaries.map(summary => summary.counter_overflowed),
      publicationInvariants: invariants,
      diagnosticsHealth: measuredRaw.map(raw => raw.result.diagnostics.header?.diagnostics_health_at_export ?? null),
    },
  }
}

function diagnosticRecord(product, raw, productPath, rawPath) {
  validateProductResult(product)
  const summary = raw.result?.diagnostics?.summary
  if (raw.mode !== 'product' || raw.result?.ok !== true || raw.result.lifecycle !== 'Published') {
    throw new Error('Raw diagnostic evidence is not Published')
  }
  for (const field of [
    'queue',
    'namespace_by_kind',
    'file_pipeline',
    'peaks',
    'revision_opens',
    'claim_batches',
    'output_resources',
    'counter_overflowed',
  ]) {
    if (summary?.[field] === undefined) throw new Error(`Diagnostic summary is missing ${field}`)
  }
  requireClaimPhases(summary)
  return {
    resultPath: resolve(productPath),
    rawEvidencePath: resolve(rawPath),
    result: product,
    blockerSummary: {
      queue: summary.queue,
      namespace_by_kind: summary.namespace_by_kind,
      file_pipeline: summary.file_pipeline,
      peaks: summary.peaks,
      revision_opens: summary.revision_opens,
      claim_batches: summary.claim_batches,
      output_resources: summary.output_resources,
      counter_overflowed: summary.counter_overflowed,
      publication_invariants: raw.result.publicationInvariants,
      diagnostics_health: raw.result.diagnostics.header?.diagnostics_health_at_export ?? null,
    },
  }
}

async function summarizeDiagnostic(values) {
  if (values.product.length !== 2 || values.rawProduct.length !== 2) {
    throw new Error('Diagnostic evidence requires exactly one warm-up and one measured Product run')
  }
  const [products, raws] = await Promise.all([
    Promise.all(values.product.map(json)),
    Promise.all(values.rawProduct.map(json)),
  ])
  const records = products.map((product, index) => diagnosticRecord(
    product,
    raws[index],
    values.product[index],
    values.rawProduct[index],
  ))
  if (records[0].result.warmup !== true || records[1].result.warmup !== false) {
    throw new Error('Diagnostic samples must be ordered warm-up then measured')
  }
  if (JSON.stringify(records[0].result.environment) !== JSON.stringify(records[1].result.environment)) {
    throw new Error('Diagnostic samples do not share one environment session')
  }
  return {
    schema: 'windshare/fsa-small-file-native-diagnostic/v1',
    createdAt: new Date().toISOString(),
    warmup: records[0],
    measured: records[1],
  }
}

const { command, values } = parseArguments(process.argv.slice(2))
if (command === 'assemble') await emit(required(values, 'output'), await assemble(values))
else if (command === 'summarize-native') await emit(required(values, 'output'), await summarizeNative(values))
else if (command === 'summarize-diagnostic') await emit(required(values, 'output'), await summarizeDiagnostic(values))
else throw new Error('Usage: native-results.mjs assemble|summarize-native|summarize-diagnostic ...')
