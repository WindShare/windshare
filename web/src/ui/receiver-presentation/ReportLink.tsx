import { shellCopy } from '../locales/shell'
import { useCopy } from '../preferences/use-preferences'
import { useSyncExternalStore } from 'react'

export interface ShareReportSource {
  readonly subscribe: (listener: () => void) => () => void
  readonly getSnapshot: () => string | null
}
export function ReportLink({ source }: { readonly source: ShareReportSource }) {
  const t = useCopy(shellCopy)
  const url = useSyncExternalStore(source.subscribe, source.getSnapshot, source.getSnapshot)
  return url ? <a className="share-report-link" href={url} target="_blank" rel="noopener noreferrer" referrerPolicy="no-referrer">{t.reportShare}</a> : null
}
