import { describe, expect, it } from 'vitest'

import {
  DirectProcess,
  formatDirectProcessDiagnostic,
} from '../../e2e/fixtures/direct-process'
import {
  createCapabilityRedactor,
  formatCapabilityDiagnostic,
} from '../../e2e/fixtures/capability-redactor'
import { formatDiagnosticValue } from '../../src/security/diagnostic-formatter'

describe('browser fixture security runtime', () => {
  it('requires an explicit per-stream disclosure policy and redacts capability stdout', async () => {
    expect(() => new DirectProcess(process.execPath, ['-e', ''], {
      cwd: process.cwd(),
      operationId: 'security-policy-required',
    } as never)).toThrow(/disclosure policy/u)

    const capability = 'capability-secret-actual'
    const processFixture = new DirectProcess(process.execPath, [
      '-e',
      `process.stdout.write(${JSON.stringify(capability)} + '\\n');` +
      'process.stderr.write("safe-stderr\\n"); setTimeout(() => {}, 5000)',
    ], {
      cwd: process.cwd(),
      operationId: 'security-capability-capture',
      disclosure: { stdout: 'capability', stderr: 'safe' },
    })
    try {
      await expect(processFixture.waitFor('stdout', new RegExp(`^${capability}$`, 'mu')))
        .resolves.toBeTruthy()
      await expect(processFixture.waitFor('stderr', /^safe-stderr$/mu)).resolves.toBeTruthy()
      const beforeConsume = JSON.parse(processFixture.diagnostic()) as {
        readonly stdout: string
        readonly stderr: string
      }
      expect(beforeConsume.stdout).toContain('redacted capability')
      expect(beforeConsume.stdout).not.toContain(capability)
      expect(beforeConsume.stderr).toContain('safe-stderr')

      processFixture.consumeReadiness('stdout')
      const afterConsume = JSON.parse(processFixture.diagnostic()) as { readonly stdout: string }
      expect(afterConsume.stdout).not.toContain(capability)
      expect(afterConsume.stdout).toContain('redacted capability')
    } finally {
      await processFixture.stop()
    }
  })

  it('formats nested causes and aggregate errors without mutating or leaking the source graph', () => {
    const cause = new Error('nested cause')
    const aggregate = new AggregateError([cause, new Error('second cause')], 'aggregate')
    const root = new Error('root failure', { cause: aggregate })
    const cyclic: Record<string, unknown> = { root }
    cyclic.self = cyclic

    const formatted = formatDiagnosticValue(cyclic) as Record<string, unknown>
    expect(Object.isFrozen(formatted)).toBe(true)
    expect(formatted.self).toBe('[diagnostic cycle]')
    expect(JSON.stringify(formatted)).toContain('nested cause')
    expect(root.cause).toBe(aggregate)
    expect(aggregate.errors).toHaveLength(2)
  })

  it('fails closed for hostile diagnostic proxies instead of propagating their secret errors', () => {
    const secret = 'proxy-trap-capability'
    const hostile = new Proxy({}, {
      getPrototypeOf: () => {
        throw new Error(secret)
      },
    })
    const formatted = formatDiagnosticValue({ safe: 'visible', hostile }) as Record<string, unknown>

    expect(formatted.safe).toBe('visible')
    expect(formatted.hostile).toBe('[unreadable diagnostic value]')
    expect(JSON.stringify(formatted)).not.toContain(secret)
  })

  it('redacts actual complete URLs before fragments and separate keys recursively', () => {
    const completeUrl = 'https://receiver.invalid/s/share#actual-fragment'
    const fragment = '#actual-fragment'
    const separateKey = 'separate-key-actual'
    const redactor = createCapabilityRedactor({ completeUrl, fragment, separateKey })
    const nested = new AggregateError([
      new Error(`failed at ${completeUrl}; key=${separateKey}`),
    ], `navigate ${completeUrl}`)

    const output = formatCapabilityDiagnostic({
      completeUrl,
      fragment,
      separateKey,
      nested,
    }, { completeUrl, fragment, separateKey })
    expect(output).not.toContain(completeUrl)
    expect(output).not.toContain(fragment)
    expect(output).not.toContain(separateKey)
    expect(output).toContain('[capability-url redacted]')
    expect(output).toContain('[separate-key redacted]')
    expect(redactor.redactText(`url=${completeUrl} key=${separateKey}`)).toContain(
      '[capability-url redacted]',
    )
    redactor.clear()
  })

  it('keeps capability stderr redacted inside nested process errors', () => {
    const secret = 'stderr-capability-secret'
    const snapshot = formatDirectProcessDiagnostic({
      operationId: 'stderr-capability',
      code: 1,
      signal: null,
      spawnError: new AggregateError([
        new Error(`child failed with ${secret}`),
      ], `aggregate ${secret}`),
      stdout: 'ordinary output',
      stderr: `ready ${secret}`,
      stdoutCapturedCharacters: 16,
      stderrCapturedCharacters: 32,
      redactText: (value) => value.split(secret).join('[actual capability redacted]'),
    }, { stdout: 'private', stderr: 'capability' })

    const encoded = JSON.stringify(snapshot)
    expect(encoded).not.toContain(secret)
    expect(snapshot.stderr).toContain('redacted capability')
  })

  it('uses the validated disclosure snapshot even when a formatter callback mutates policy', () => {
    const policy = { stdout: 'private', stderr: 'capability' } as {
      stdout: 'private' | 'safe' | 'capability'
      stderr: 'private' | 'safe' | 'capability'
    }
    const snapshot = formatDirectProcessDiagnostic({
      operationId: 'mutable-policy',
      code: null,
      signal: null,
      spawnError: null,
      stdout: '',
      stderr: 'secret stderr',
      stdoutCapturedCharacters: 0,
      stderrCapturedCharacters: 13,
      redactText: (value) => {
        // This mutation occurs while formatting the detached snapshot. A
        // formatter that reads `policy` after validation would publish stderr.
        policy.stderr = 'safe'
        return value
      },
    }, policy)

    expect(snapshot.stderr).toContain('redacted capability')
    expect(snapshot.stderr).not.toContain('secret stderr')
  })
})
