import type { ExecFileException } from 'node:child_process'
import { formatCapabilityDiagnostic, type CapabilityRedactionValues } from './capability-redactor'

type ReceiverExit = Pick<ExecFileException, 'code' | 'signal' | 'killed'> & { stdout?: string; stderr?: string }

const TEST_FAILURE = /^[ \t]*(turn_receive_network_test\.go:\d+): ([^\r\n]+)/mu
const RUNTIME_FAILURE = /^(panic: test timed out\b|panic:|fatal error:)/mu
const TEST_STACK_LOCATION = /(?:[/\\]|^[ \t]*)(turn_receive_network_test\.go:\d+)(?:[ \t]+\+0x[0-9a-f]+)?[ \t]*$/imu
const RUNTIME_REASON: Readonly<Record<string, string>> = {
  'panic: test timed out': 'Go test timed out',
  'panic:': 'Go panic',
  'fatal error:': 'Go runtime fatal error',
}

function goFailure(output: string, directory: string) {
  const failure = TEST_FAILURE.exec(output)
  if (failure) {
    const message = failure[2]!
      .replaceAll(directory, '<fixture>')
      .replaceAll(directory.replaceAll('\\', '/'), '<fixture>')
    // An environment dump has no safe free-form suffix. Its test location is enough to find the caller.
    const reason = /(?<![\w?&])[A-Za-z_]\w*=/u.test(message) ? 'Go receive test failed' : message
    return { source: failure[1], failure: reason }
  }
  // Panic payloads and stack arguments can contain credentials. Only publish the runtime category and our static source name.
  const crash = RUNTIME_FAILURE.exec(output)
  if (crash) return { failure: RUNTIME_REASON[crash[1]!], source: TEST_STACK_LOCATION.exec(output)?.[1] }
  return {}
}

export function nativeTURNReceiveFailure(exit: ReceiverExit, directory: string, secrets: CapabilityRedactionValues): Error {
  const stdout = exit.stdout ?? ''
  const stderr = exit.stderr ?? ''
  const detail = goFailure(stdout + '\n' + stderr, directory)
  const cause = formatCapabilityDiagnostic({
    code: exit.code, signal: exit.signal, killed: exit.killed,
    stdoutBytes: Buffer.byteLength(stdout), stderrBytes: Buffer.byteLength(stderr), ...detail,
  }, secrets)
  return new Error('Native TURN download failed', { cause })
}
