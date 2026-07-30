import {
  contractError,
  freezeRecord,
  requireBoolean,
  requireEnum,
  requireExactKeys,
  requireRecord,
  requireSafeInteger,
  requireString,
} from './contract/json.ts'
import type { ExecutionOutcome, ResultStatus } from './vocabulary.ts'

export const RUNNER_PROCESS_TERMINALS = Object.freeze([
  'not-started',
  'running-at-collection',
  'spawn-failed',
  'exited',
  'signaled',
] as const)
export const RUNNER_MAXIMUM_EXIT_CODE = 0xffff_ffff as const

export type RunnerProcessEvidence =
  | { readonly terminal: 'not-started' | 'running-at-collection' }
  | { readonly terminal: 'spawn-failed'; readonly errorCode: string; readonly errorMessage: string }
  | { readonly terminal: 'exited'; readonly exitCode: number }
  | { readonly terminal: 'signaled'; readonly signal: string }

export interface ExecutionEvidence {
  readonly pageCrashed: boolean
  readonly targetCrashed: boolean
  readonly unexpectedBrowserDisconnect: boolean
  readonly infrastructureFailure: boolean
  readonly lifecycleCompleted: boolean
  readonly runnerProcess: RunnerProcessEvidence
}

export function parseExecutionEvidence(value: unknown): ExecutionEvidence {
  const evidence = requireRecord(value, 'execution evidence')
  requireExactKeys(
    evidence,
    [
      'pageCrashed',
      'targetCrashed',
      'unexpectedBrowserDisconnect',
      'infrastructureFailure',
      'lifecycleCompleted',
      'runnerProcess',
    ],
    [],
    'execution evidence',
  )
  const parsed = freezeRecord({
    pageCrashed: requireBoolean(evidence.pageCrashed, 'page crashed evidence'),
    targetCrashed: requireBoolean(evidence.targetCrashed, 'target crashed evidence'),
    unexpectedBrowserDisconnect: requireBoolean(
      evidence.unexpectedBrowserDisconnect,
      'unexpected browser disconnect evidence',
    ),
    infrastructureFailure: requireBoolean(
      evidence.infrastructureFailure,
      'infrastructure failure evidence',
    ),
    lifecycleCompleted: requireBoolean(
      evidence.lifecycleCompleted,
      'execution lifecycle completion evidence',
    ),
    runnerProcess: parseRunnerProcessEvidence(evidence.runnerProcess),
  })
  validateExecutionConsistency(parsed)
  return parsed
}

/** Assertion failures remain a Playwright verdict; only raw lifecycle and process
 * facts participate in execution classification. */
export function classifyExecutionOutcome(evidence: ExecutionEvidence): ExecutionOutcome {
  if (
    evidence.pageCrashed || evidence.targetCrashed ||
    evidence.unexpectedBrowserDisconnect
  ) {
    return 'crashed'
  }
  if (
    evidence.infrastructureFailure || evidence.runnerProcess.terminal === 'spawn-failed' ||
    evidence.runnerProcess.terminal === 'signaled'
  ) {
    return 'infrastructure-failed'
  }
  if (evidence.lifecycleCompleted && evidence.runnerProcess.terminal === 'exited') return 'healthy'
  return 'unknown'
}

export function validateRunnerProcessVerdict(
  resultStatus: ResultStatus,
  playwrightOutcome: 'not-started' | 'passed' | 'failed',
  evidence: ExecutionEvidence,
): void {
  const process = evidence.runnerProcess
  if (resultStatus === 'provisional') {
    if (process.terminal !== 'not-started') {
      contractError('provisional result cannot assert runner process termination')
    }
    return
  }
  if (playwrightOutcome === 'not-started') {
    if (process.terminal === 'exited' && process.exitCode === 0) {
      contractError('not-started Playwright outcome contradicts runner exit code zero')
    }
    return
  }
  if (playwrightOutcome === 'passed') {
    if (process.terminal !== 'exited' || process.exitCode !== 0) {
      contractError('passed Playwright outcome requires runner exit code zero')
    }
    return
  }
  if (
    process.terminal !== 'signaled' &&
    (process.terminal !== 'exited' || process.exitCode === 0)
  ) {
    contractError('failed Playwright outcome requires a nonzero or signaled runner terminal')
  }
}

function parseRunnerProcessEvidence(value: unknown): RunnerProcessEvidence {
  const process = requireRecord(value, 'runner process evidence')
  const terminal = requireEnum(
    process.terminal,
    RUNNER_PROCESS_TERMINALS,
    'runner process terminal',
  )
  if (terminal === 'spawn-failed') {
    requireExactKeys(process, ['terminal', 'errorCode', 'errorMessage'], [], 'runner process evidence')
    return freezeRecord({
      terminal,
      errorCode: requirePortableToken(process.errorCode, 'runner spawn error code'),
      errorMessage: requireString(process.errorMessage, 'runner spawn error message', 512),
    })
  }
  if (terminal === 'exited') {
    requireExactKeys(process, ['terminal', 'exitCode'], [], 'runner process evidence')
    return freezeRecord({
      terminal,
      exitCode: requireSafeInteger(process.exitCode, 0, RUNNER_MAXIMUM_EXIT_CODE, 'runner exit code'),
    })
  }
  if (terminal === 'signaled') {
    requireExactKeys(process, ['terminal', 'signal'], [], 'runner process evidence')
    return freezeRecord({ terminal, signal: requirePortableToken(process.signal, 'runner signal') })
  }
  requireExactKeys(process, ['terminal'], [], 'runner process evidence')
  return freezeRecord({ terminal })
}

function validateExecutionConsistency(evidence: ExecutionEvidence): void {
  const process = evidence.runnerProcess
  const browserRuntimeEvidence = evidence.pageCrashed || evidence.targetCrashed ||
    evidence.unexpectedBrowserDisconnect
  if (process.terminal === 'not-started' && (browserRuntimeEvidence || evidence.lifecycleCompleted)) {
    contractError('runner that never started cannot carry browser lifecycle evidence')
  }
  if (process.terminal === 'running-at-collection' && evidence.lifecycleCompleted) {
    contractError('running runner cannot claim a completed execution lifecycle')
  }
  if (process.terminal === 'spawn-failed') validateSpawnFailure(evidence, browserRuntimeEvidence)
  if (process.terminal === 'signaled') validateSignalTermination(evidence, browserRuntimeEvidence)
  if (evidence.lifecycleCompleted && process.terminal !== 'exited') {
    contractError('completed execution lifecycle requires an exited runner process')
  }
}

function validateSpawnFailure(evidence: ExecutionEvidence, browserRuntimeEvidence: boolean): void {
  if (!evidence.infrastructureFailure) {
    contractError('runner spawn failure must be classified as infrastructure evidence')
  }
  if (browserRuntimeEvidence || evidence.lifecycleCompleted) {
    contractError('runner spawn failure cannot carry browser lifecycle evidence')
  }
}

function validateSignalTermination(evidence: ExecutionEvidence, browserRuntimeEvidence: boolean): void {
  if (evidence.lifecycleCompleted) {
    contractError('signaled runner cannot claim a completed execution lifecycle')
  }
  if (!browserRuntimeEvidence && !evidence.infrastructureFailure) {
    contractError('signal termination without browser crash evidence is infrastructure failure')
  }
}

function requirePortableToken(value: unknown, label: string): string {
  const token = requireString(value, label, 128)
  if (!/^[A-Za-z0-9._-]+$/u.test(token)) contractError(`${label} contains non-portable characters`)
  return token
}
