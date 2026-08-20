import type {
  DiagnosticBundleHeaderV1,
  DiagnosticBundleIncidentLineV1,
  DiagnosticBundleLocalOutputFailureLineV1,
  DiagnosticBundleTraceCaptureLineV1,
  DiagnosticBundleTraceEventLineV1,
  DiagnosticBundleV1,
} from './diagnostic-bundle-v1'

type DiagnosticBundleLineV1 =
  | DiagnosticBundleHeaderV1
  | DiagnosticBundleIncidentLineV1
  | DiagnosticBundleLocalOutputFailureLineV1
  | DiagnosticBundleTraceCaptureLineV1
  | DiagnosticBundleTraceEventLineV1

export function encodeDiagnosticBundleNdjson(bundle: DiagnosticBundleV1): string {
  const lines: DiagnosticBundleLineV1[] = [
    bundle.header,
    ...bundle.incidents,
    ...bundle.localOutputFailures,
    ...(bundle.traceCapture === undefined ? [] : [bundle.traceCapture]),
    ...bundle.traceEvents,
  ]
  return `${lines.map(encodeLine).join('\n')}\n`
}

function encodeLine(line: DiagnosticBundleLineV1): string {
  const encoded = JSON.stringify(line)
  if (encoded === undefined) {
    throw new TypeError('diagnostic bundle line is not standard JSON')
  }
  return encoded
}
