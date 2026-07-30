import type {
  BrowserSampleContainmentBackend,
  BrowserSampleContainmentRequest,
} from '../../scripts/browser-evidence/process/containment.ts'
import type { RunnerProcessEvidence } from '../../scripts/browser-evidence/execution-evidence.ts'
import { runSyntheticScenario } from './synthetic-scenario.ts'

export function createDeterministicTestContainmentBackend(): BrowserSampleContainmentBackend {
  return Object.freeze({
    kind: 'test' as const,
    preflight: async () => undefined,
    execute: executeDeterministicScenario,
  })
}

/**
 * A contract test must not acquire host process authority merely to manufacture
 * evidence. The command remains part of the request so an invalid executable
 * still projects the same spawn-failure contract without invoking the OS.
 */
async function executeDeterministicScenario(request: BrowserSampleContainmentRequest) {
  if (request.command.executable !== process.execPath) {
    return Object.freeze({
      processEvidence: spawnFailure(request.command.executable),
      timedOut: false,
    })
  }
  const mode = request.command.environment?.SYNTHETIC_CHILD_MODE
  if (mode === undefined) throw new Error('deterministic containment requires an explicit scenario mode')
  const encodedDelay = request.command.environment?.SYNTHETIC_CHILD_DELAY_MS
  const delayMs = encodedDelay === undefined ? 0 : Number(encodedDelay)
  if (delayMs > request.deadlineMs) {
    return Object.freeze({
      processEvidence: Object.freeze({ terminal: 'signaled' as const, signal: 'SIGKILL' }),
      timedOut: true,
    })
  }
  const outcome = await runSyntheticScenario({
    context: request.childContext,
    mode,
    delayMs,
    ...(request.command.environment === undefined
      ? {}
      : { environment: request.command.environment }),
    stdout: request.stdout,
    stderr: request.stderr,
  })
  return Object.freeze({
    processEvidence: Object.freeze({ terminal: 'exited' as const, exitCode: outcome.exitCode }),
    timedOut: false,
  })
}

function spawnFailure(executable: string): RunnerProcessEvidence {
  return Object.freeze({
    terminal: 'spawn-failed',
    errorCode: 'ENOENT',
    errorMessage: `deterministic executable authority is unavailable: ${executable}`.slice(0, 512),
  })
}
