import { useSyncExternalStore } from 'react'

export interface ShareReportSource {
  readonly observeReport: (listener: () => void) => () => void
  readonly getReportURL: () => string | undefined
}
export function ReportLink({ source }: { readonly source: ShareReportSource }) {
  const url = useSyncExternalStore(source.observeReport, source.getReportURL, source.getReportURL)
  return url ? <a className="share-report-link" href={url} target="_blank" rel="noopener noreferrer" referrerPolicy="no-referrer">Report share</a> : null
}
