import { isProxy } from 'node:util/types'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { isAbsolute, resolve } from 'node:path'
import { loadNetworkMatrixRegistry, type LoadedNetworkMatrixRegistry } from '../manifest.ts'
import type { NetworkMatrixTraceSnapshot } from '../trace/index.ts'
import {
  NETWORK_MATRIX_EXECUTION_MODES,
  NETWORK_MATRIX_SAMPLE_RETRY_COUNT,
  type NetworkMatrixExecutionMode,
} from '../vocabulary.ts'
import {
  aggregateNetworkMatrixFiles,
} from './aggregate-files.ts'
import {
  startNetworkMatrixExecution,
  type ExecuteNetworkMatrixResult,
  type NetworkMatrixRuntimeBootstrap,
} from './execute.ts'
import {
  openNetworkMatrixPublisherHelper,
  type NetworkMatrixPublisherHelperAuthority,
  type NetworkMatrixPublisherHelperOptions,
} from './publisher-helper.ts'
import {
  loadProductionNetworkMatrixCliRuntime,
  loadProductionNetworkMatrixCliRuntimeFromBytes,
} from '../linux-topology/production-runtime.ts'
import { GitHubActionsOidcBootstrapLease } from '../linux-topology/parent-workload-identity.ts'

type CliOptions = ReadonlyMap<string, readonly string[]>

export interface BrowserNetworkMatrixCliComposition {
  readonly runtimeBootstrap?: NetworkMatrixRuntimeBootstrap
  readonly workloadIdentityBootstrap?: GitHubActionsOidcBootstrapLease
  readonly productionRuntimeConfigBytes?: Uint8Array
  readonly platform?: NodeJS.Platform
  readonly openPublisherHelper?: (
    options: NetworkMatrixPublisherHelperOptions,
  ) => Promise<NetworkMatrixPublisherHelperAuthority>
  readonly loadRegistry?: (manifestPath: string) => Promise<LoadedNetworkMatrixRegistry>
  readonly writeSummary?: (encoded: string) => void
}

export class NetworkMatrixRuntimeNotWiredError extends Error {
  constructor() {
    super('concrete local browser network matrix topology runtime is not wired')
    this.name = 'NetworkMatrixRuntimeNotWiredError'
  }
}

export async function browserNetworkMatrixCli(
  arguments_: readonly string[],
  composition: BrowserNetworkMatrixCliComposition = {},
): Promise<number> {
  requireOidcRequestSentinelsAbsent()
  const [command, ...optionArguments] = arguments_
  if (command === undefined) throw new Error(cliUsage())
  const options = parseOptions(optionArguments)
  if (command === 'execute') return executeCommand(options, composition)
  if (command === 'aggregate') return aggregateCommand(options, composition)
  throw new Error(`unknown browser network matrix command ${JSON.stringify(command)}\n${cliUsage()}`)
}

async function executeCommand(
  options: CliOptions,
  composition: BrowserNetworkMatrixCliComposition,
): Promise<number> {
  assertOnlyOptions(options, [
    'mode', 'run-id', 'manifest', 'output-root', 'helper-manifest', 'publisher-helper',
    'process-owner', 'runtime-config', 'checkout-sha', 'repository-root',
  ])
  const configuredRuntime = await configuredProductionRuntime(options, composition)
  if (composition.runtimeBootstrap === undefined && configuredRuntime === undefined) {
    throw new NetworkMatrixRuntimeNotWiredError()
  }
  const publisherOptions = publisherHelperOptions(
    options,
    composition,
    configuredRuntime?.platform,
  )
  const mode = executionMode(requiredOption(options, 'mode'))
  let workloadIdentityBootstrap: GitHubActionsOidcBootstrapLease | undefined
  let result: ExecuteNetworkMatrixResult | undefined
  let traceSnapshot: NetworkMatrixTraceSnapshot | undefined
  let runnerTraceSnapshot: NetworkMatrixTraceSnapshot | null | undefined
  let outerFailure: unknown
  try {
    result = await withPublisherHelper(publisherOptions, composition, async (publisher) => {
      workloadIdentityBootstrap = configuredRuntime === undefined
        ? undefined
        : composition.workloadIdentityBootstrap
      if (configuredRuntime !== undefined && workloadIdentityBootstrap === undefined) {
        throw new Error('production browser network matrix requires one pre-minted OIDC bootstrap')
      }
      const runtimeBootstrap = composition.runtimeBootstrap ?? configuredRuntime
        ?.bindWorkloadIdentityBootstrap(
          workloadIdentityBootstrap as GitHubActionsOidcBootstrapLease,
          publisherOptions.processOwnerPath,
        )
      if (runtimeBootstrap === undefined) throw new NetworkMatrixRuntimeNotWiredError()
      let commandResult: ExecuteNetworkMatrixResult | undefined
      let commandFailure: unknown
      try {
        const registry = await (composition.loadRegistry ?? loadNetworkMatrixRegistry)(
          resolve(requiredOption(options, 'manifest')),
        )
        const execution = startNetworkMatrixExecution({
          registry,
          runId: requiredOption(options, 'run-id'),
          executionMode: mode,
          outputRoot: resolve(requiredOption(options, 'output-root')),
          runtimeBootstrap,
          publisher: publisher.artifactPublisher,
        })
        try {
          commandResult = await execution.result
        } finally {
          traceSnapshot = execution.traces.snapshot()
          runnerTraceSnapshot = await execution.runnerTraces
        }
      } catch (cause) {
        commandFailure = cause
      }
      try {
        await workloadIdentityBootstrap?.closeAndWait()
      } catch (cleanupFailure) {
        if (commandFailure !== undefined) {
          throw new AggregateError(
            [commandFailure, cleanupFailure],
            'browser network matrix command and OIDC bootstrap cleanup both failed',
            { cause: cleanupFailure },
          )
        }
        throw cleanupFailure
      }
      if (commandFailure !== undefined) throw commandFailure
      if (commandResult === undefined) throw new Error('browser network matrix command returned no result')
      return commandResult
    })
  } catch (primaryFailure) {
    outerFailure = primaryFailure
  }

  let tracePublicationFailure: unknown
  if (traceSnapshot !== undefined) {
    try {
      if (runnerTraceSnapshot === undefined) {
        throw new Error('browser network matrix runner trace settlement was not retained')
      }
      if (result !== undefined && result.runnerTraces !== runnerTraceSnapshot) {
        throw new Error('browser network matrix runner trace settlement changed before publication')
      }
      if (result?.commandOutcome === 'completed' && runnerTraceSnapshot === null) {
        throw new Error('completed browser network matrix execution lacks runner trace evidence')
      }
      writeSettledExecutionTraces(traceSnapshot, runnerTraceSnapshot)
    } catch (cause) {
      tracePublicationFailure = cause
    }
  }
  if (outerFailure !== undefined && tracePublicationFailure !== undefined) {
    throw new AggregateError(
      [outerFailure, tracePublicationFailure],
      'browser network matrix command and trace publication both failed',
      { cause: outerFailure },
    )
  }
  if (outerFailure !== undefined) throw outerFailure
  if (tracePublicationFailure !== undefined) throw tracePublicationFailure
  if (result === undefined) throw new Error('browser network matrix command returned no result')

  writeSummary(composition, {
    command: 'execute',
    commandOutcome: result.commandOutcome,
    mode,
    runId: result.run.runId,
    runOutcome: result.run.runOutcome,
    evidenceOutcome: result.aggregate.evidenceOutcome,
    acceptanceOutcome: scheduledExecutionAccepted(result) ? 'passed' : 'failed',
    retryCount: NETWORK_MATRIX_SAMPLE_RETRY_COUNT,
    outputRoot: result.publication.outputRoot,
    runPath: result.publication.runPath,
    aggregatePath: result.publication.aggregatePath,
  })
  return scheduledExecutionAccepted(result) ? 0 : 1
}

function requireOidcRequestSentinelsAbsent(): void {
  for (const name of Object.keys(process.env)) {
    const folded = name.toUpperCase()
    if (
      folded === 'ACTIONS_ID_TOKEN_REQUEST_URL' ||
      folded === 'ACTIONS_ID_TOKEN_REQUEST_TOKEN'
    ) throw new Error('OIDC request authority reached the browser network matrix process')
  }
}

async function configuredProductionRuntime(
  options: CliOptions,
  composition: BrowserNetworkMatrixCliComposition,
): Promise<Awaited<ReturnType<typeof loadProductionNetworkMatrixCliRuntime>> | undefined> {
  const runtimeConfig = optionalOption(options, 'runtime-config')
  const checkoutSha = optionalOption(options, 'checkout-sha')
  const repositoryRoot = optionalOption(options, 'repository-root')
  if (composition.runtimeBootstrap !== undefined) {
    if (
      runtimeConfig !== undefined || composition.productionRuntimeConfigBytes !== undefined ||
      checkoutSha !== undefined || repositoryRoot !== undefined
    ) {
      throw new Error('injected network matrix runtime cannot receive production checkout authority')
    }
    return undefined
  }
  if (composition.productionRuntimeConfigBytes !== undefined) {
    if (runtimeConfig !== undefined) {
      throw new Error('retained runtime config cannot be combined with a path runtime config')
    }
    return loadProductionNetworkMatrixCliRuntimeFromBytes(
      composition.productionRuntimeConfigBytes,
      currentCheckoutAuthority(
        requiredOption(options, 'checkout-sha'),
        requiredOption(options, 'repository-root'),
      ),
    )
  }
  if (runtimeConfig === undefined) throw new NetworkMatrixRuntimeNotWiredError()
  return loadProductionNetworkMatrixCliRuntime(
    resolve(runtimeConfig),
    currentCheckoutAuthority(
      requiredOption(options, 'checkout-sha'),
      requiredOption(options, 'repository-root'),
    ),
  )
}

async function aggregateCommand(
  options: CliOptions,
  composition: BrowserNetworkMatrixCliComposition,
): Promise<number> {
  assertOnlyOptions(options, [
    'manifest', 'run', 'output', 'helper-manifest', 'publisher-helper', 'process-owner',
  ])
  const publisherOptions = publisherHelperOptions(options, composition)
  const result = await withPublisherHelper(publisherOptions, composition, async (publisher) => {
    const registry = await (composition.loadRegistry ?? loadNetworkMatrixRegistry)(
      resolve(requiredOption(options, 'manifest')),
    )
    const inputPaths = optionValues(options, 'run').map((path) => resolve(path))
    return aggregateNetworkMatrixFiles({
      registry,
      inputPaths,
      outputPath: resolve(requiredOption(options, 'output')),
      publisher: publisher.aggregatePublisher,
    })
  })
  writeSummary(composition, {
    command: 'aggregate',
    modes: result.runs.map(({ executionMode }) => executionMode),
    runIds: result.runs.map(({ runId }) => runId),
    evidenceOutcome: result.aggregate.evidenceOutcome,
    acceptanceOutcome: result.aggregate.evidenceOutcome === 'complete' ? 'passed' : 'failed',
    retryCount: NETWORK_MATRIX_SAMPLE_RETRY_COUNT,
    outputPath: result.publication.path,
  })
  return result.aggregate.evidenceOutcome === 'complete' ? 0 : 1
}

function scheduledExecutionAccepted(
  result: ExecuteNetworkMatrixResult,
): boolean {
  return result.commandOutcome === 'completed' &&
    result.runtimeCleanupOutcome === 'completed' &&
    result.runnerTraces !== null &&
    result.run.executionMode === 'scheduled' &&
    result.run.orchestrationOutcome === 'healthy' &&
    result.run.runOutcome === 'completed' &&
    result.aggregate.evidenceOutcome === 'complete'
}

function writeSettledExecutionTraces(
  execution: NetworkMatrixTraceSnapshot,
  runner: NetworkMatrixTraceSnapshot | null,
): void {
  const executionEvents = requireSettledTraceEvents(execution, 'execution')
  const runnerEvents = runner === null ? Object.freeze([]) : requireSettledTraceEvents(runner, 'runner')
  // Both journals are validated before the first byte is published, preventing a
  // valid execution prefix from disguising rejected runner evidence.
  for (const trace of [...executionEvents, ...runnerEvents]) {
    process.stderr.write(`${JSON.stringify(trace)}\n`)
  }
}

function requireSettledTraceEvents(
  snapshot: NetworkMatrixTraceSnapshot,
  journal: 'execution' | 'runner',
): readonly NetworkMatrixTraceSnapshot['events'][number][] {
  if (isProxy(snapshot)) {
    throw new Error(`browser network matrix ${journal} trace snapshot is a Proxy`)
  }
  const descriptors = Object.getOwnPropertyDescriptors(snapshot)
  const data = (key: keyof NetworkMatrixTraceSnapshot): unknown => {
    const descriptor = descriptors[key]
    if (descriptor === undefined || !('value' in descriptor)) {
      throw new Error(`browser network matrix ${journal} trace snapshot has an accessor or missing field`)
    }
    return descriptor.value
  }
  const events = data('events')
  const observedEvents = data('observedEvents')
  const capturedEvents = data('capturedEvents')
  const observedBytes = data('observedBytes')
  const capturedBytes = data('capturedBytes')
  if (
    isProxy(events) ||
    !Array.isArray(events) ||
    data('failure') !== null ||
    data('completed') !== true ||
    data('truncated') !== false ||
    !Number.isSafeInteger(observedEvents) ||
    !Number.isSafeInteger(capturedEvents) ||
    !Number.isSafeInteger(observedBytes) ||
    !Number.isSafeInteger(capturedBytes) ||
    observedEvents !== capturedEvents ||
    observedBytes !== capturedBytes ||
    events.length !== capturedEvents ||
    events.some((event) => isProxy(event) || !Object.isFrozen(event))
  ) {
    throw new Error(`browser network matrix ${journal} lifecycle trace did not settle completely`)
  }
  return events
}

function parseOptions(arguments_: readonly string[]): CliOptions {
  const result = new Map<string, string[]>()
  for (let index = 0; index < arguments_.length; index += 2) {
    const name = arguments_[index]
    const value = arguments_[index + 1]
    if (name === undefined || !/^--[a-z][a-z-]*$/u.test(name) || value === undefined) {
      throw new Error(`invalid browser network matrix options\n${cliUsage()}`)
    }
    const key = name.slice(2)
    result.set(key, [...(result.get(key) ?? []), value])
  }
  return result
}

function assertOnlyOptions(options: CliOptions, allowed: readonly string[]): void {
  for (const name of options.keys()) {
    if (!allowed.includes(name)) throw new Error(`unknown browser network matrix option --${name}`)
  }
}

function requiredOption(options: CliOptions, name: string): string {
  const values = optionValues(options, name)
  if (values.length !== 1) {
    throw new Error(`browser network matrix option --${name} must appear exactly once`)
  }
  return values[0] as string
}

function optionValues(options: CliOptions, name: string): readonly string[] {
  return options.get(name) ?? []
}

function optionalOption(options: CliOptions, name: string): string | undefined {
  const values = optionValues(options, name)
  if (values.length > 1) {
    throw new Error(`browser network matrix option --${name} must appear at most once`)
  }
  return values[0]
}

async function withPublisherHelper<T>(
  options: NetworkMatrixPublisherHelperOptions,
  composition: BrowserNetworkMatrixCliComposition,
  use: (authority: NetworkMatrixPublisherHelperAuthority) => Promise<T>,
): Promise<T> {
  const authority = await (composition.openPublisherHelper ?? openNetworkMatrixPublisherHelper)(options)
  let result: T | undefined
  let primaryFailure: unknown
  let failed = false
  try {
    result = await use(authority)
  } catch (cause) {
    primaryFailure = cause
    failed = true
  }
  try {
    await authority.close()
  } catch (closeFailure) {
    if (failed) {
      throw new AggregateError(
        [primaryFailure, closeFailure],
        'browser network matrix command and publisher authority cleanup both failed',
        { cause: closeFailure },
      )
    }
    throw closeFailure
  }
  if (failed) throw primaryFailure
  return result as T
}

function publisherHelperOptions(
  options: CliOptions,
  composition: BrowserNetworkMatrixCliComposition,
  configuredPlatform?: 'linux' | 'win32',
): NetworkMatrixPublisherHelperOptions {
  const platform = publisherPlatform(configuredPlatform ?? composition.platform ?? process.platform)
  return Object.freeze({
    helperManifestPath: requiredOption(options, 'helper-manifest'),
    publisherHelperPath: requiredOption(options, 'publisher-helper'),
    platform,
    processOwnerPath: requiredOption(options, 'process-owner'),
  })
}

function publisherPlatform(value: NodeJS.Platform): 'linux' | 'win32' {
  if (value !== 'linux' && value !== 'win32') {
    throw new Error(`browser network matrix publisher is unsupported on ${value}`)
  }
  return value
}

function executionMode(value: string): NetworkMatrixExecutionMode {
  if (!NETWORK_MATRIX_EXECUTION_MODES.includes(value as NetworkMatrixExecutionMode)) {
    throw new Error(`unknown browser network matrix execution mode ${JSON.stringify(value)}`)
  }
  return value as NetworkMatrixExecutionMode
}

function currentCheckoutAuthority(
  checkoutSha: string,
  repositoryRoot: string,
): Readonly<{ checkoutSha: string, repositoryRoot: string }> {
  if (!/^[a-f0-9]{40}$/u.test(checkoutSha)) {
    throw new Error('browser network matrix checkout SHA must be a lowercase 40-character Git object ID')
  }
  if (
    !isAbsolute(repositoryRoot) || resolve(repositoryRoot) !== repositoryRoot ||
    repositoryRoot.includes('\0')
  ) {
    throw new Error('browser network matrix repository root must be a canonical absolute path')
  }
  return Object.freeze({ checkoutSha, repositoryRoot })
}

function writeSummary(
  composition: BrowserNetworkMatrixCliComposition,
  value: Readonly<Record<string, unknown>>,
): void {
  ;(composition.writeSummary ?? ((encoded: string) => process.stdout.write(encoded)))(
    `${JSON.stringify(value)}\n`,
  )
}

function cliUsage(): string {
  return [
    'browser network matrix local commands:',
    '  execute --mode scheduled --run-id ID --manifest FILE --output-root NEW_DIR --helper-manifest FILE --publisher-helper FILE --process-owner FILE --runtime-config FILE --checkout-sha SHA --repository-root DIR',
    '  aggregate --manifest FILE --run RUN_JSON [--run RUN_JSON] --output NEW_FILE --helper-manifest FILE --publisher-helper FILE --process-owner FILE',
  ].join('\n')
}

const invokedPath = process.argv[1]
if (
  invokedPath !== undefined &&
  pathToFileURL(resolve(invokedPath)).href === pathToFileURL(fileURLToPath(import.meta.url)).href
) {
  browserNetworkMatrixCli(process.argv.slice(2)).then(
    (exitCode) => { process.exitCode = exitCode },
    () => {
      process.stderr.write(`${JSON.stringify({
        component: 'browser-network-matrix-cli',
        outcome: 'failed',
        failureCode: 'cli-command-failed',
      })}\n`)
      process.exitCode = 1
    },
  )
}
