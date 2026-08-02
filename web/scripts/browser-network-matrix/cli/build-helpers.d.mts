export const HELPER_BUILD_MANIFEST_SCHEMA_VERSION:
  'windshare.browser-network-matrix.helper-build/v1'

export interface HelperBuildOperation {
  readonly operation: 'artifact-publisher' | 'test-process-owner'
  readonly role: 'artifact-publisher' | 'test-process-owner'
  readonly cwd: string
  readonly packagePath: string
  readonly outputPath: string
  readonly platform: 'win32' | 'linux'
  readonly goArchitecture: 'amd64' | 'arm64'
  readonly deadlineMs: number
  readonly maximumOutputBytes: number
  readonly arguments: readonly string[]
}

export interface HelperBuildManifest {
  readonly schemaVersion: typeof HELPER_BUILD_MANIFEST_SCHEMA_VERSION
  readonly platform: 'win32' | 'linux'
  readonly architecture: 'amd64' | 'arm64'
  readonly helpers: readonly Readonly<{
    role: 'artifact-publisher' | 'test-process-owner'
    path: string
    sha256: string
  }>[]
}

export class HelperBuildError extends Error {
  constructor(message: string, options: Readonly<{
    cause?: unknown
    operation: string
    outputDirectory: string
    outputOwned: boolean
  }>)
  readonly operation: string
  readonly outputDirectory: string
  readonly outputOwned: boolean
}

export function helperBuildPlan(
  outputDirectory: string,
  platform?: NodeJS.Platform,
  architecture?: string,
): readonly HelperBuildOperation[]

export function buildNetworkMatrixHelpers(
  outputDirectory: string,
  options?: Readonly<{
    platform?: NodeJS.Platform
    architecture?: string
    runOperation?: (operation: HelperBuildOperation) => Promise<void>
    onProgress?: (event: Readonly<Record<string, unknown>>) => void
  }>,
): Promise<Readonly<{
  outputDirectory: string
  manifestPath: string
  manifest: HelperBuildManifest
}>>

export function runBuildHelpersCli(
  arguments_: readonly string[],
  options?: Readonly<{
    stdout?: { write(value: string): unknown }
    stderr?: { write(value: string): unknown }
    build?: typeof buildNetworkMatrixHelpers
  }>,
): Promise<number>
