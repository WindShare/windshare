import type {
  DiagnosticBundleHeaderV2,
  DiagnosticBundleIncidentLineV2,
  DiagnosticBundleLocalOutputFailureLineV2,
  DiagnosticBundleTraceCaptureLineV2,
  DiagnosticBundleTraceEventLineV2,
  DiagnosticBundleV2,
} from './diagnostic-bundle-v2'

type DiagnosticBundleLineV2 =
  | DiagnosticBundleHeaderV2
  | DiagnosticBundleIncidentLineV2
  | DiagnosticBundleLocalOutputFailureLineV2
  | DiagnosticBundleTraceCaptureLineV2
  | DiagnosticBundleTraceEventLineV2

export function encodeDiagnosticBundleNdjson(bundle: DiagnosticBundleV2): string {
  const lines: DiagnosticBundleLineV2[] = [
    bundle.header,
    ...bundle.incidents,
    ...bundle.localOutputFailures,
    ...(bundle.traceCapture === undefined ? [] : [bundle.traceCapture]),
    ...bundle.traceEvents,
  ]
  return `${lines.map(encodeLine).join('\n')}\n`
}

function encodeLine(line: DiagnosticBundleLineV2): string {
  const encoded = JSON.stringify(line)
  if (encoded === undefined) {
    throw new TypeError('diagnostic bundle line is not standard JSON')
  }
  return encoded
}
