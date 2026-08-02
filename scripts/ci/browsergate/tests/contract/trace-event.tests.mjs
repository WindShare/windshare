import assert from 'node:assert/strict'

import {
  BROWSERGATE_TRACE_COMPONENT,
  BROWSERGATE_TRACE_SCHEMA_VERSION,
  classifyBrowsergateTraceOutcome,
  createBrowsergateTraceEvent,
} from '../../trace-event.mjs'

const preflightFailure = createBrowsergateTraceEvent({
    runId: 'browsergate-run-17',
    operationId: 'browser-runtime-generated-semantic-preflight',
    scenario: 'generated-semantic-runtime-preflight',
    milestone: 'settled',
    reportedOutcome: 'failed',
    payload: {
      cleanupOutcome: 'completed',
      failureCode: 'result-record-invalid',
      outputEvidence: {
        stdout: {
          stream: 'stdout',
          segments: [{ sequence: 0, offset: 0, byteLength: 3, base64: 'e30K' }],
        },
      },
    },
  })

assert.deepEqual(
  preflightFailure,
  {
    schemaVersion: BROWSERGATE_TRACE_SCHEMA_VERSION,
    runId: 'browsergate-run-17',
    operationId: 'browser-runtime-generated-semantic-preflight',
    scenario: 'generated-semantic-runtime-preflight',
    component: BROWSERGATE_TRACE_COMPONENT,
    milestone: 'settled',
    outcome: 'failed',
    payload: {
      cleanupOutcome: 'completed',
      failureCode: 'result-record-invalid',
      outputEvidence: {
        stdout: {
          stream: 'stdout',
          segments: [{ sequence: 0, offset: 0, byteLength: 3, base64: 'e30K' }],
        },
      },
      reportedOutcome: 'failed',
    },
  },
)

const preflightLifecycle = [
  createBrowsergateTraceEvent({
    runId: 'browsergate-run-17',
    operationId: 'browser-runtime-generated-semantic-preflight',
    scenario: 'generated-semantic-runtime-preflight',
    milestone: 'started',
  }),
  preflightFailure,
]
assert.equal(preflightLifecycle.filter(({ milestone }) => milestone === 'started').length, 1)
assert.equal(preflightLifecycle.filter(({ milestone }) => milestone === 'settled').length, 1)
assert.equal(preflightLifecycle.at(-1)?.payload.cleanupOutcome, 'completed')
for (const event of preflightLifecycle) {
  assert.deepEqual(Object.keys(event).slice(0, 7), [
    'schemaVersion',
    'runId',
    'operationId',
    'scenario',
    'component',
    'milestone',
    'outcome',
  ])
}

assert.equal(classifyBrowsergateTraceOutcome('started'), 'in-progress')
assert.equal(classifyBrowsergateTraceOutcome('artifact-validated'), 'in-progress')
assert.equal(classifyBrowsergateTraceOutcome('runtime-command-tree-empty'), 'succeeded')
assert.equal(classifyBrowsergateTraceOutcome('runtime-command-tree-not-empty'), 'failed')
assert.equal(classifyBrowsergateTraceOutcome('settled', 'current'), 'succeeded')
assert.equal(classifyBrowsergateTraceOutcome('settled', 'failed'), 'failed')
assert.throws(
  () => createBrowsergateTraceEvent({
    runId: 'run',
    operationId: 'operation',
    scenario: 'scenario',
    milestone: 'failed',
    payload: { operationId: 'cannot-overwrite-identity' },
  }),
  /cannot overwrite/u,
)

process.stdout.write('browsergate trace event contract: PASS\n')
