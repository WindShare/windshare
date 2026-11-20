import { shellCopy } from '../locales/shell'
import { useCopy } from '../preferences/use-preferences'
import { useSyncExternalStore } from 'react'

export interface ShareReportSource {
  readonly observeReport: (listener: () => void) => () => void
  readonly getReportURL: () => string | undefined
}
export function ReportLink({ source }: { readonly source: ShareReportSource }) {
  const t = useCopy(shellCopy)
  const url = useSyncExternalStore(source.observeReport, source.getReportURL, source.getReportURL)
  return url ? <a className="share-report-link" href={url} target="_blank" rel="noopener noreferrer" referrerPolicy="no-referrer">{t.reportShare}</a> : null
}
