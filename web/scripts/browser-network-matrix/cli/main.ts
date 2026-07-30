import { fileURLToPath, pathToFileURL } from 'node:url'
import { resolve } from 'node:path'
import { loadNetworkMatrixRegistry, type LoadedNetworkMatrixRegistry } from '../manifest.ts'
import type { NetworkMatrixRunTraceSink } from '../runner.ts'
import {
  NETWORK_MATRIX_EXECUTION_MODES,
  type NetworkMatrixExecutionMode,
} from '../vocabulary.ts'
import {
  aggregateNetworkMatrixFiles,
} from './aggregate-files.ts'
import {
  executeNetworkMatrix,
  type NetworkMatrixRuntimeBootstrap,
} from './execute.ts'
import {
  openNetworkMatrixPublisherHelper,
  type NetworkMatrixPublisherHelperAuthority,
  type NetworkMatrixPublisherHelperOptions,
} from './publisher-helper.ts'
import { loadProductionNetworkMatrixCliRuntime } from '../linux-topology/production-runtime.ts'
import { GitHubActionsOidcBootstrapLease } from '../linux-topology/parent-workload-identity.ts'

type CliOptions = ReadonlyMap<string, readonly string[]>

export interface BrowserNetworkMatrixCliComposition {
  readonly runtimeBootstrap?: NetworkMatrixRuntimeBootstrap
  readonly platform?: NodeJS.Platform
  readonly openPublisherHelper?: (
    options: NetworkMatrixPublisherHelperOptions,
  ) => Promise<NetworkMatrixPublisherHelperAuthority>
  readonly loadRegistry?: (manifestPath: string) => Promise<LoadedNetworkMatrixRegistry>
  readonly trace?: NetworkMatrixRunTraceSink
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
    'windows-job-helper', 'runtime-config',
  ])
  const runtimeConfig = optionalOption(options, 'runtime-config')
  if (composition.runtimeBootstrap === undefined && runtimeConfig === undefined) {
    throw new NetworkMatrixRuntimeNotWiredError()
  }
  const configuredRuntime = composition.runtimeBootstrap === undefined
    ? await loadProductionNetworkMatrixCliRuntime(resolve(runtimeConfig as string))
    : undefined
  if (composition.runtimeBootstrap !== undefined && runtimeConfig !== undefined) {
    throw new Error('injected network matrix runtime cannot receive a production runtime config')
  }
  if (composition.runtimeBootstrap === undefined && configuredRuntime === undefined) {
    throw new NetworkMatrixRuntimeNotWiredError()
  }
  const publisherOptions = publisherHelperOptions(
    options,
    composition,
    configuredRuntime?.platform,
    configuredRuntime?.windowsJobHelperPath,
  )
  const mode = executionMode(requiredOption(options, 'mode'))
  let workloadIdentityBootstrap: GitHubActionsOidcBootstrapLease | undefined
  let result: Awaited<ReturnType<typeof executeNetworkMatrix>>
  try {
    result = await withPublisherHelper(publisherOptions, composition, async (publisher) => {
      workloadIdentityBootstrap = configuredRuntime === undefined
        ? undefined
        : GitHubActionsOidcBootstrapLease.capture()
      const runtimeBootstrap = composition.runtimeBootstrap ?? configuredRuntime
        ?.bindWorkloadIdentityBootstrap(workloadIdentityBootstrap as GitHubActionsOidcBootstrapLease)
      if (runtimeBootstrap === undefined) throw new NetworkMatrixRuntimeNotWiredError()
      let commandResult: Awaited<ReturnType<typeof executeNetworkMatrix>> | undefined
      let commandFailure: unknown
      try {
        const registry = await (composition.loadRegistry ?? loadNetworkMatrixRegistry)(
          resolve(requiredOption(options, 'manifest')),
        )
        commandResult = await executeNetworkMatrix({
          registry,
          runId: requiredOption(options, 'run-id'),
          executionMode: mode,
          outputRoot: resolve(requiredOption(options, 'output-root')),
          runtimeBootstrap,
          publisher: publisher.artifactPublisher,
          ...(composition.trace === undefined ? {} : { trace: composition.trace }),
        })
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
    // Publisher authentication intentionally precedes bootstrap capture. If it
    // fails, consume-and-erase the ambient sentinels before an exported caller
    // can catch the rejection and continue inside the same runner process.
    if (configuredRuntime !== undefined && workloadIdentityBootstrap === undefined) {
      try {
        const abandoned = GitHubActionsOidcBootstrapLease.capture()
        await abandoned.forceTerminateAndWait()
      } catch {
        // Capture deletes both sentinels atomically even when one is absent or invalid.
      }
    }
    throw primaryFailure
  }
  writeSummary(composition, {
    command: 'execute',
    commandOutcome: result.commandOutcome,
    mode,
    runId: result.run.runId,
    runOutcome: result.run.runOutcome,
    evidenceOutcome: result.aggregate.evidenceOutcome,
    outputRoot: result.publication.outputRoot,
    runPath: result.publication.runPath,
    aggregatePath: result.publication.aggregatePath,
  })
  return result.commandOutcome === 'completed' ? 0 : 1
}

async function aggregateCommand(
  options: CliOptions,
  composition: BrowserNetworkMatrixCliComposition,
): Promise<number> {
  assertOnlyOptions(options, [
    'manifest', 'run', 'output', 'helper-manifest', 'publisher-helper', 'windows-job-helper',
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
    outputPath: result.publication.path,
  })
  return 0
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
  configuredWindowsJobHelperPath?: string,
): NetworkMatrixPublisherHelperOptions {
  const platform = publisherPlatform(configuredPlatform ?? composition.platform ?? process.platform)
  const optionalWindowsJobHelperPath = optionalOption(options, 'windows-job-helper')
  if (
    configuredWindowsJobHelperPath !== undefined && optionalWindowsJobHelperPath !== undefined
  ) throw new Error('Windows Job helper authority must come from exactly one composition boundary')
  if (platform === 'linux' && optionalWindowsJobHelperPath !== undefined) {
    throw new Error('browser network matrix option --windows-job-helper is only valid on Windows')
  }
  const windowsJobHelperPath = platform === 'win32'
    ? configuredWindowsJobHelperPath ?? requiredOption(options, 'windows-job-helper')
    : undefined
  return Object.freeze({
    helperManifestPath: requiredOption(options, 'helper-manifest'),
    publisherHelperPath: requiredOption(options, 'publisher-helper'),
    platform,
    ...(windowsJobHelperPath === undefined
      ? {}
      : { windowsJobHelperPath }),
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
    '  execute --mode scheduled|manual --run-id ID --manifest FILE --output-root NEW_DIR --helper-manifest FILE --publisher-helper FILE --runtime-config FILE',
    '  aggregate --manifest FILE --run RUN_JSON [--run RUN_JSON] --output NEW_FILE --helper-manifest FILE --publisher-helper FILE [--windows-job-helper FILE on Windows]',
  ].join('\n')
}

const invokedPath = process.argv[1]
if (
  invokedPath !== undefined &&
  pathToFileURL(resolve(invokedPath)).href === pathToFileURL(fileURLToPath(import.meta.url)).href
) {
  browserNetworkMatrixCli(process.argv.slice(2)).then(
    (exitCode) => { process.exitCode = exitCode },
    (cause: unknown) => {
      process.stderr.write(`${JSON.stringify({
        component: 'browser-network-matrix-cli',
        outcome: 'failed',
        error: cause instanceof Error ? cause.message : String(cause),
      })}\n`)
      process.exitCode = 1
    },
  )
}
