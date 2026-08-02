import type { OwnedEventSnapshot } from '../../../browser-evidence/process/owned-process-channel.mjs'
import type { NetworkMatrixIdentity } from '../../manifest.ts'

export const LINUX_TOPOLOGY_TRACE_SCHEMA_VERSION =
  'windshare.browser-network-matrix.linux-topology-trace/v1' as const

export type LinuxTopologyTraceComponent =
  | 'contained-browser-broker'
  | 'credential-broker-process-owner'

export type LinuxTopologyTraceScenario =
  | 'contained-browser-sample'
  | 'credential-broker-exchange'

export type LinuxTopologyTraceOutcome = 'started' | 'succeeded' | 'failed'
export type LinuxTopologyTraceContextValue = string | number | boolean | null

export interface LinuxTopologyTraceIdentity {
  readonly component: LinuxTopologyTraceComponent
  readonly scenario: LinuxTopologyTraceScenario
  readonly operationId: string
  readonly runId: string
  readonly profileId: NetworkMatrixIdentity['profileId']
  readonly browser: NetworkMatrixIdentity['browser']
  readonly sampleOrdinal: NetworkMatrixIdentity['sampleOrdinal']
}

export interface LinuxTopologyTraceEvent extends LinuxTopologyTraceIdentity {
  readonly schemaVersion: typeof LINUX_TOPOLOGY_TRACE_SCHEMA_VERSION
  readonly milestone: string
  readonly outcome: LinuxTopologyTraceOutcome
  readonly context: Readonly<Record<string, LinuxTopologyTraceContextValue>>
}

export interface LinuxTopologyTraceFailure {
  readonly name: string
  readonly message: string
}

export interface LinuxTopologyTraceSnapshot extends OwnedEventSnapshot<LinuxTopologyTraceEvent> {
  readonly observedBytes: number
  readonly capturedBytes: number
  readonly failure: LinuxTopologyTraceFailure | null
}

export interface LinuxTopologyTraceChannel extends AsyncIterable<LinuxTopologyTraceEvent> {
  snapshot(): LinuxTopologyTraceSnapshot
}
