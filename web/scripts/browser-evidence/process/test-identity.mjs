export const MAXIMUM_TEST_IDENTIFIER_BYTES = 128
export const MAXIMUM_TEST_SCENARIO_BYTES = 192

const PROCESS_OWNER_RESERVED_ENVIRONMENT_NAMES = new Set([
  'windshare_test_event_fd',
  'windshare_test_event_handle',
  'windshare_test_run_id',
  'windshare_test_operation_id',
  'windshare_test_scenario',
])

// Test identity and event transport are child capabilities injected by the
// process owner. Ambient copies must never become a second authority channel.
export function isProcessOwnerReservedEnvironmentName(name) {
  return typeof name === 'string' &&
    PROCESS_OWNER_RESERVED_ENVIRONMENT_NAMES.has(asciiFold(name))
}

export function parseTestIdentity({ runId, operationId, scenario }) {
  return Object.freeze({
    runId: requirePortableToken(runId, 'run ID', MAXIMUM_TEST_IDENTIFIER_BYTES, false),
    operationId: requirePortableToken(
      operationId,
      'operation ID',
      MAXIMUM_TEST_IDENTIFIER_BYTES,
      false,
    ),
    scenario: requirePortableToken(scenario, 'scenario', MAXIMUM_TEST_SCENARIO_BYTES, true),
  })
}

function requirePortableToken(value, label, maximumBytes, allowSlash) {
  if (typeof value !== 'string' || value.length < 1 || Buffer.byteLength(value, 'utf8') > maximumBytes) {
    throw new Error(`test ${label} is invalid`)
  }
  for (let index = 0; index < value.length; index += 1) {
    const code = value.charCodeAt(index)
    if (isAsciiAlphaNumeric(code)) continue
    const interior = index > 0 && index < value.length - 1
    const separator = code === 0x2d || code === 0x5f || code === 0x2e || allowSlash && code === 0x2f
    if (!interior || !separator) throw new Error(`test ${label} is invalid`)
  }
  return value
}

function asciiFold(value) {
  return value.replace(/[A-Z]/gu, (character) => character.toLowerCase())
}

function isAsciiAlphaNumeric(code) {
  return code >= 0x61 && code <= 0x7a || code >= 0x41 && code <= 0x5a ||
    code >= 0x30 && code <= 0x39
}
