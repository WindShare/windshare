import type { ShareReport } from '../../application/network/report'

type ReportOwnership =
  | { readonly kind: 'none' }
  | { readonly kind: 'pending'; readonly report: ShareReport }
  | { readonly kind: 'committed'; readonly report: ShareReport | undefined }

/**
 * Owns the single share-report URL exposed by ReceiverController.
 *
 * Committed shares always take precedence so blocked, deduplicated, or failed
 * replacements never clear or overwrite the active share's report link. When
 * no share is committed yet, an initial join attempt retains its report
 * watcher across connecting and initial failure (for example, an offline
 * sender) until cancelled, replaced, committed, or disposed.
 */
export class ShareReportScope {
  readonly #onChange: () => void
  #state: ReportOwnership = { kind: 'none' }
  #unsubscribe: (() => void) | undefined
  #url: string | null = null

  constructor(onChange: () => void) {
    this.#onChange = onChange
  }

  get url(): string | null { return this.#url }

  adoptPending(report: ShareReport): boolean {
    // A committed share (including self-hosted, where report is undefined)
    // must not be preempted by a speculative replacement attempt.
    if (this.#state.kind === 'committed') return false
    if (this.#state.kind === 'pending') this.#state.report.close()
    this.#state = { kind: 'pending', report }
    this.#observe(report)
    return true
  }

  clearPending(): void {
    if (this.#state.kind !== 'pending') return
    this.#state.report.close()
    this.#state = { kind: 'none' }
    this.#observe(undefined)
  }

  commit(report: ShareReport | undefined): void {
    // Promoting the active initial join's report transfers ownership to
    // JoinedBrowserShare without tearing down its live watcher.
    if (this.#state.kind === 'pending' && this.#state.report !== report) {
      this.#state.report.close()
    }
    this.#state = { kind: 'committed', report }
    this.#observe(report)
  }

  clear(): void {
    if (this.#state.kind === 'pending') this.#state.report.close()
    this.#state = { kind: 'none' }
    this.#observe(undefined)
  }

  close(): void {
    this.#unsubscribe?.()
    this.#unsubscribe = undefined
    if (this.#state.kind === 'pending') this.#state.report.close()
    this.#state = { kind: 'none' }
    this.#url = null
  }

  #observe(source: ShareReport | undefined): void {
    this.#unsubscribe?.()
    this.#unsubscribe = undefined
    if (source !== undefined) {
      this.#unsubscribe = source.subscribe(() => this.#sync(source.url ?? null))
    }
    this.#sync(source?.url ?? null)
  }

  #sync(next: string | null): void {
    if (this.#url === next) return
    this.#url = next
    this.#onChange()
  }
}
