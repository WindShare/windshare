export const BROWSERGATE_TRACE_SCHEMA_VERSION = 'windshare.browsergate-trace/v1'
export const BROWSERGATE_TRACE_COMPONENT = 'browser-orchestration'
export const BROWSERGATE_DEFAULT_RUN_ID = 'browsergate'

const RESERVED_PAYLOAD_FIELDS = Object.freeze([
  'schemaVersion',
  'runId',
  'operationId',
  'scenario',
  'component',
  'milestone',
  'outcome',
  'reportedOutcome',
])

export function createBrowsergateTraceEvent({
  runId = BROWSERGATE_DEFAULT_RUN_ID,
  operationId,
  scenario = operationId,
  milestone,
  reportedOutcome,
  payload = {},
}) {
  requireIdentity(runId, 'run ID')
  requireIdentity(operationId, 'operation ID')
  requireIdentity(scenario, 'scenario')
  requireIdentity(milestone, 'milestone')
  if (!isRecord(payload)) throw new Error('browsergate trace payload must be an object')
  if (RESERVED_PAYLOAD_FIELDS.some((field) => Object.hasOwn(payload, field))) {
    throw new Error('browsergate trace payload cannot overwrite event identity')
  }

  const outcome = classifyBrowsergateTraceOutcome(milestone, reportedOutcome)
  const projectedPayload = Object.freeze({
    ...payload,
    ...(reportedOutcome === undefined ? {} : { reportedOutcome }),
  })
  return Object.freeze({
    schemaVersion: BROWSERGATE_TRACE_SCHEMA_VERSION,
    runId,
    operationId,
    scenario,
    component: BROWSERGATE_TRACE_COMPONENT,
    milestone,
    outcome,
    ...(Object.keys(projectedPayload).length === 0 ? {} : { payload: projectedPayload }),
  })
}

export function classifyBrowsergateTraceOutcome(milestone, reportedOutcome) {
  requireIdentity(milestone, 'milestone')
  if (reportedOutcome !== undefined && typeof reportedOutcome !== 'string') {
    throw new Error('browsergate reported outcome must be a string')
  }

  const reported = reportedOutcome?.toLowerCase()
  if (['failed', 'failure', 'quarantined'].includes(reported)) return 'failed'
  if (['not-run', 'not_run', 'skipped'].includes(reported)) return 'not-run'
  if (['passed', 'success', 'succeeded', 'completed', 'current'].includes(reported)) {
    return 'succeeded'
  }

  if (/(?:^|-)not-run$/u.test(milestone)) return 'not-run'
  if (/(?:failed|failure|tree-not-empty|rejected)$/u.test(milestone)) return 'failed'
  if (/(?:completed|passed|tree-empty|settled)$/u.test(milestone)) return 'succeeded'
  return 'in-progress'
}

function requireIdentity(value, label) {
  if (typeof value !== 'string' || value.length < 1 || value.length > 256 || /[\r\n\0]/u.test(value)) {
    throw new Error(`browsergate trace ${label} is invalid`)
  }
}

function isRecord(value) {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}
