import type {
  DiagnosticBundleHeaderV2,
  DiagnosticBundleFailureEvidenceLineV2,
  DiagnosticBundleIncidentLineV2,
  DiagnosticBundleLocalOutputFailureLineV2,
  DiagnosticBundleTraceCaptureLineV2,
  DiagnosticBundleTraceEventLineV2,
  DiagnosticBundleV2,
} from './diagnostic-bundle-v2'
import { isDeeplyFrozen } from './json'
import { hasFailureEvidenceDetails, summarizeFailureFact } from '../failure/summary'
import { summarizeIncidentConsequencesV2 } from '../incident/consequences'

export const MAX_DIAGNOSTIC_BUNDLE_BYTES = 8 * 1_024 * 1_024
const OMISSION_HEADER_RESERVE_BYTES = 512

type DiagnosticBundleLineV2 =
  | DiagnosticBundleHeaderV2
  | DiagnosticBundleFailureEvidenceLineV2
  | DiagnosticBundleIncidentLineV2
  | DiagnosticBundleLocalOutputFailureLineV2
  | DiagnosticBundleTraceCaptureLineV2
  | DiagnosticBundleTraceEventLineV2
interface EncodedLine { readonly text: string; readonly bytes: number }

export function encodeDiagnosticBundleNdjson(bundle: DiagnosticBundleV2): string {
  return new DiagnosticBundleNdjsonEncoder().encode(bundle)
}

/** Reuse immutable lines without retaining records evicted from the bounded histories. */
export class DiagnosticBundleNdjsonEncoder {
  readonly #lines = new WeakMap<DiagnosticBundleLineV2, EncodedLine>()
  readonly #summaries = new WeakMap<DiagnosticBundleIncidentLineV2, EncodedLine>()
  readonly #maxBytes: number

  constructor(maxBytes = MAX_DIAGNOSTIC_BUNDLE_BYTES) {
    if (!Number.isSafeInteger(maxBytes) || maxBytes <= OMISSION_HEADER_RESERVE_BYTES) {
      throw new RangeError('Diagnostic export capacity must accommodate its header')
    }
    this.#maxBytes = maxBytes
  }

  encode(bundle: DiagnosticBundleV2): string {
    const header = this.#encodeLine(bundle.header)
    const capture = bundle.traceCapture === undefined ? undefined : this.#encodeLine(bundle.traceCapture)
    // Reserve every nominated trigger before admitting any detailed branch or secondary line.
    const incidents = bundle.incidents.map(line => this.#minimalIncident(line))
    let remaining = this.#maxBytes - header.bytes - (capture?.bytes ?? 0) - OMISSION_HEADER_RESERVE_BYTES -
      incidents.reduce((bytes, line) => bytes + line.bytes, 0)
    if (remaining < 0) throw new RangeError('Diagnostic trigger summaries exceed export capacity')
    let incidentsSummarized = 0
    for (let index = incidents.length - 1; index >= 0; index--) {
      const detailed = this.#encodeLine(bundle.incidents[index]!)
      const extra = detailed.bytes - incidents[index]!.bytes
      if (extra <= remaining) { incidents[index] = detailed; remaining -= extra }
      else incidentsSummarized++
    }
    const select = (lines: readonly DiagnosticBundleLineV2[]): EncodedLine[] => {
      const retained: EncodedLine[] = []
      for (let index = lines.length - 1; index >= 0; index--) {
        const line = this.#encodeLine(lines[index]!)
        if (line.bytes > remaining) continue
        remaining -= line.bytes
        retained.push(line)
      }
      return retained.reverse()
    }
    const failures = select(bundle.failureEvidence)
    const output = select(bundle.localOutputFailures)
    const events = select(bundle.traceEvents)
    const omitted = {
      incidents_summarized: incidentsSummarized,
      failure_evidence_records: bundle.failureEvidence.length - failures.length,
      local_output_failures: bundle.localOutputFailures.length - output.length,
      trace_events: bundle.traceEvents.length - events.length,
    }
    const exportHeader = Object.values(omitted).some(count => count > 0)
      ? this.#encodeLine({ ...bundle.header, export_omitted: omitted }) : header
    if (exportHeader.bytes > header.bytes + OMISSION_HEADER_RESERVE_BYTES) {
      throw new RangeError('Diagnostic omission summary exceeds reserved capacity')
    }
    return [exportHeader, ...incidents, ...failures, ...output, ...(capture === undefined ? [] : [capture]), ...events]
      .map(line => line.text).join('')
  }

  #minimalIncident(line: DiagnosticBundleIncidentLineV2): EncodedLine {
    const cached = this.#summaries.get(line)
    if (cached !== undefined) return cached
    const { payload } = line.record
    const trigger = summarizeFailureFact(payload.trigger)
    const secondary = [...payload.contributors, ...payload.consequences]
    const overflow = secondary.reduce((count, bucket) => count + BigInt(bucket.count), BigInt(payload.overflow_fact_count))
    const summary = this.#encodeLine({ ...line,
      ...(line.late_consequences === undefined ? {} : {
        late_consequences: summarizeIncidentConsequencesV2(line.late_consequences),
      }), record: { ...line.record, payload: {
      ...payload, trigger: trigger.fact,
      contributors: [], consequences: [], overflow_fact_count: overflow.toString(10),
      evidence_details_omitted: (payload.evidence_details_omitted ?? 0) +
        secondary.filter(bucket => hasFailureEvidenceDetails(bucket.representative.evidence)).length +
        trigger.evidenceDetailsOmitted,
    } } })
    if (isDeeplyFrozen(line)) this.#summaries.set(line, summary)
    return summary
  }

  #encodeLine(line: DiagnosticBundleLineV2): EncodedLine {
    const cached = this.#lines.get(line)
    if (cached !== undefined) return cached
    const encoded = JSON.stringify(line)
    if (encoded === undefined) throw new TypeError('diagnostic bundle line is not standard JSON')
    const text = encoded + '\n'
    const result = { text, bytes: new TextEncoder().encode(text).byteLength }
    if (isDeeplyFrozen(line)) this.#lines.set(line, result)
    return result
  }
}
