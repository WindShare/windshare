import {
  DiagnosticBundleProjectorV2,
  type DiagnosticBundleIdentityV2,
  type DiagnosticBundleSnapshotInput,
  type DiagnosticsStatusV2,
} from './export/diagnostic-bundle-v2'
import { DiagnosticBundleNdjsonEncoder } from './export/ndjson'

export interface DiagnosticsEvidenceSnapshot {
  readonly status: DiagnosticsStatusV2
  readonly hasEvidence: boolean
  export(): string
}

export interface DiagnosticsEvidenceReadPort {
  readEvidence(): DiagnosticsEvidenceSnapshot
}

export type DiagnosticsEvidenceInput = Omit<DiagnosticBundleSnapshotInput, 'identity' | 'time'>

/** Stable snapshot identity lets persistence skip idle checkpoints before encoding any log body. */
export class DiagnosticsEvidenceReader {
  readonly #projector: DiagnosticBundleProjectorV2
  readonly #encoder = new DiagnosticBundleNdjsonEncoder()
  readonly #captureTime: () => string
  #last: Readonly<{
    input: DiagnosticsEvidenceInput
    metadata: string
    snapshot: DiagnosticsEvidenceSnapshot
  }> | undefined

  constructor(identity: DiagnosticBundleIdentityV2, captureTime: () => string) {
    this.#projector = new DiagnosticBundleProjectorV2(identity)
    this.#captureTime = captureTime
  }

  read(input: DiagnosticsEvidenceInput): DiagnosticsEvidenceSnapshot {
    const metadata = evidenceMetadata(input)
    const last = this.#last
    if (last !== undefined && metadata === last.metadata &&
        sameRecords(input.incidents, last.input.incidents) &&
        sameRecords(input.localOutputFailures, last.input.localOutputFailures) &&
        sameRecords(input.traceCapture?.events, last.input.traceCapture?.events)) {
      return last.snapshot
    }
    const time = this.#captureTime()
    const snapshot: DiagnosticsEvidenceSnapshot = Object.freeze({
      status: input.status,
      hasEvidence: input.incidents.length > 0 || (input.traceCapture?.events.length ?? 0) > 0,
      export: () => this.#encode(input, time),
    })
    this.#last = { input, metadata, snapshot }
    return snapshot
  }

  export(input: DiagnosticsEvidenceInput): string {
    return this.#encode(input, this.#captureTime())
  }

  clear(): void { this.#last = undefined }

  #encode(input: DiagnosticsEvidenceInput, time: string): string {
    return this.#encoder.encode(this.#projector.project({ ...input, time }))
  }
}

function sameRecords(left: readonly unknown[] = [], right: readonly unknown[] = []): boolean {
  return left.length === right.length && left.every((record, index) => record === right[index])
}

function evidenceMetadata(input: DiagnosticsEvidenceInput): string {
  // Counts alone miss coalesced replacements and a full ring replacing old events.
  // Compare immutable record identities above; only small metadata is serialized here.
  const trace = input.traceCapture
  const capture = trace === undefined ? undefined : {
    state: trace.state,
    captureGeneration: trace.captureGeneration,
    startedAtMilliseconds: trace.startedAtMilliseconds,
    sealReason: trace.sealReason,
    retainedEventCount: trace.retainedEventCount,
    retainedEventBytes: trace.retainedEventBytes,
    incidentMarkerCount: trace.incidentMarkerCount,
    health: trace.health,
  }
  return JSON.stringify({ status: input.status, capture }, (_key, value: unknown) =>
    typeof value === 'bigint' ? value.toString(10) : value)
}
