import type {
  DiagnosticBundleHeaderV2,
  DiagnosticBundleIncidentLineV2,
  DiagnosticBundleLocalOutputFailureLineV2,
  DiagnosticBundleTraceCaptureLineV2,
  DiagnosticBundleTraceEventLineV2,
  DiagnosticBundleV2,
} from './diagnostic-bundle-v2'
import { isDeeplyFrozen } from './json'

type DiagnosticBundleLineV2 =
  | DiagnosticBundleHeaderV2
  | DiagnosticBundleIncidentLineV2
  | DiagnosticBundleLocalOutputFailureLineV2
  | DiagnosticBundleTraceCaptureLineV2
  | DiagnosticBundleTraceEventLineV2

export function encodeDiagnosticBundleNdjson(bundle: DiagnosticBundleV2): string {
  return new DiagnosticBundleNdjsonEncoder().encode(bundle)
}

/** Reuse immutable lines without retaining records evicted from the bounded histories. */
export class DiagnosticBundleNdjsonEncoder {
  readonly #lines = new WeakMap<DiagnosticBundleLineV2, string>()

  encode(bundle: DiagnosticBundleV2): string {
    const lines: DiagnosticBundleLineV2[] = [
      bundle.header,
      ...bundle.incidents,
      ...bundle.localOutputFailures,
      ...(bundle.traceCapture === undefined ? [] : [bundle.traceCapture]),
      ...bundle.traceEvents,
    ]
    return lines.map(line => this.#encodeLine(line)).join('')
  }

  #encodeLine(line: DiagnosticBundleLineV2): string {
    const cached = this.#lines.get(line)
    if (cached !== undefined) return cached
    const encoded = JSON.stringify(line)
    if (encoded === undefined) throw new TypeError('diagnostic bundle line is not standard JSON')
    const text = encoded + '\n'
    if (isDeeplyFrozen(line)) this.#lines.set(line, text)
    return text
  }
}
