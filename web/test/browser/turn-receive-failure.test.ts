import { expect, it } from 'vitest'
import { nativeTURNReceiveFailure } from '../../e2e/fixtures/turn-receive-failure'

const key = 'private-share-key'
const link = 'https://share.invalid/receive?r=wss%3A%2F%2Frelay.invalid#' + key
const secrets = { completeUrl: link, fragment: '#' + key, separateKey: key }
const fixture = 'C:\\private-user\\turn-fixture'

it.each(['stdout', 'stderr'] as const)('reports the Go assertion from %s and redacts actual capability values', stream => {
  const failure = '    turn_receive_network_test.go:108: context deadline exceeded in ' +
    fixture.replaceAll('\\', '/') + '/parallel.bin for ' + link + ' (' + key + ')'
  const exit = { code: 1, [stream]: 'OTHER_TOKEN=private-environment-value\r\n' + failure + '\r\nFAIL' }
  const error = nativeTURNReceiveFailure(exit, fixture, secrets)
  expect(error.cause).toContain('turn_receive_network_test.go:108')
  expect(error.cause).toContain('context deadline exceeded in <fixture>/parallel.bin')
  expect(error.cause).toContain('[capability-url redacted]')
  expect(error.cause).not.toMatch(/private-|share\.invalid|OTHER_TOKEN/u)
})

it.each([
  ['panic: test timed out after 1m', 'Go test timed out'],
  ['panic: private panic payload', 'Go panic'],
  ['fatal error: private fatal payload', 'Go runtime fatal error'],
])('reports a stderr-only Go runtime failure without exposing stack data (%s)', (line, reason) => {
  const stderr = [
    line + ' ' + link,
    'goroutine 21 [running]:',
    'arbitraryFunction(private-argument)',
    '\t' + fixture + '\\engine\\turn_receive_network_test.go:85 +0x1a2',
    'OTHER_TOKEN=private-environment-value',
  ].join('\n')
  const error = nativeTURNReceiveFailure({ code: 2, stderr }, fixture, secrets)
  expect(error.cause).toContain(reason)
  expect(error.cause).toContain('turn_receive_network_test.go:85')
  expect(error.cause).not.toMatch(/private|share\.invalid|arbitraryFunction|OTHER_TOKEN/u)
})

it('retains exit metadata when the process only reports private output', () => {
  const error = nativeTURNReceiveFailure({ signal: 'SIGTERM', killed: true, stderr: 'OTHER_TOKEN=private' }, fixture, secrets)
  expect(error.cause).toContain('SIGTERM')
  expect(error.cause).toContain('stderrBytes')
  expect(error.cause).not.toContain('OTHER_TOKEN')

  const reported = nativeTURNReceiveFailure({ stderr: 'turn_receive_network_test.go:108: [OTHER_TOKEN=private]' }, fixture, secrets)
  expect(reported.cause).toContain('turn_receive_network_test.go:108')
  expect(reported.cause).not.toContain('OTHER_TOKEN')
})
