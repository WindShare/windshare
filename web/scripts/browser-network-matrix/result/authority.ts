import { networkMatrixError } from '../contract-support.ts'
import type { NetworkSampleResult } from './contract.ts'

export function requireUniqueObservedAuthorities(
  sample: NetworkSampleResult,
  processInstanceIds: Set<string>,
  attemptIds: Set<string>,
  challengeBindings: Set<string>,
): void {
  if (sample.sampleOutcome !== 'observed') return
  if (sample.processInstanceId === null || sample.attemptEvidence === null) {
    networkMatrixError('observed sample lacks its process or attempt authority')
  }
  requireUniqueAuthority(
    processInstanceIds,
    sample.processInstanceId,
    'browser process instance ID',
  )
  requireUniqueAuthority(
    attemptIds,
    sample.attemptEvidence.attemptAuthority.attemptId,
    'attempt ID',
  )
  if (sample.attemptEvidence.challenge !== null) {
    requireUniqueAuthority(
      challengeBindings,
      sample.attemptEvidence.challenge.bindingSha256,
      'challenge binding digest',
    )
  }
}

function requireUniqueAuthority(observed: Set<string>, value: string, label: string): void {
  if (observed.has(value)) networkMatrixError(`network matrix repeats ${label} across observed samples`)
  observed.add(value)
}
