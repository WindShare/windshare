import { describe, expect, it } from 'vitest'
import { V2ControllerObservability } from '../../src/ui/controller/controller-observability'
import { recordingIncidents } from './v2-receiver-orchestration-fixture'

describe('controller failure evidence', () => {
  it.each(['join', 'projection', 'authority_activation'] as const)(
    'records the original exception for an unclassified %s failure', stage => {
      const incidents = recordingIncidents()
      const observability = new V2ControllerObservability({ incidents: incidents.port })
      const attempt = observability.open(stage)
      const error = new TypeError('active receive controls require an active lifecycle state', {
        cause: new Error('resumable-receive supplied pause'),
      })
      error.stack = 'TypeError: invalid controls\n    at activeControlActions (lifecycle.ts:523:11)'
      observability.fail(attempt, stage === 'join' ? 'join' : 'projection_authority', error, stage)
      attempt.close()
      const fact = incidents.facts[0]!.fact
      expect(incidents.decisions).toMatchObject([{ kind: 'incident', outcome: 'failed' }])
      expect(fact).toMatchObject({
        kind: 'unclassified', stage, recoveryDisposition: 'terminal',
        payload: { unclassified: { exception: {
          javascriptKind: 'type-error', errorName: 'TypeError', message: error.message,
          stack: error.stack, cause: 'Error: resumable-receive supplied pause',
        } } },
      })
    },
  )
})
