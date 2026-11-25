import { useEffect, useEffectEvent, useRef, useState } from 'react'
import type {
  RestorePlatform,
  SafeNameRestoreExportResult,
  SafeNameRestorePlan,
  SafeNameRestorePrepared,
  SafeNameRestoreStep,
  SafeNameUnsupportedEntry,
} from '../../output/model'
import type { SafeRenamedSummary } from '../../transfer/state'
import { FailureNotice } from '../controls/FailureNotice'
import { receiverCopy } from '../locales/receiver'
import type { ReceiverFailure } from '../model'
import { useCopy } from '../preferences/use-preferences'
import { ReceiverIcon } from '../receiver-presentation/ReceiverIcon'
import type { TaskPresentation } from './model'

const COPIED_INDICATOR_MS = 2_000

export interface SafeNameRestoreActions {
  readonly defaultPlatform: () => RestorePlatform
  readonly prepare: (operationId: string, platform: RestorePlatform) => Promise<SafeNameRestorePrepared>
  readonly exportArtifact: (operationId: string, platform: RestorePlatform) => Promise<SafeNameRestoreExportResult>
  readonly downloadManifest: (plan: SafeNameRestorePlan) => boolean
  readonly mapError: (error: unknown) => ReceiverFailure
}

type LoadOutcome =
  | Readonly<{ kind: 'ready'; prepared: SafeNameRestorePrepared }>
  | Readonly<{ kind: 'error'; failure: ReceiverFailure }>

interface KeyedLoadResult {
  readonly key: string
  readonly outcome: LoadOutcome
}

type KeyedFeedback = Readonly<{ key: string; kind: 'copied' | 'saved' | 'copy-failed' }>

type KeyedExport = Readonly<{
  key: string
  result: SafeNameRestoreExportResult | null
  failure: ReceiverFailure | null
}>

async function copyTextToClipboard(text: string): Promise<boolean> {
  try {
    if (typeof navigator === 'undefined' || typeof navigator.clipboard?.writeText !== 'function') {
      return false
    }
    await navigator.clipboard.writeText(text)
    return true
  } catch {
    return false
  }
}

function SampleAdjustedList({ safeRenamed }: { readonly safeRenamed: SafeRenamedSummary }) {
  const t = useCopy(receiverCopy).downloads
  const remaining = Math.max(0, safeRenamed.count - safeRenamed.sample.length)
  return <ul className="safe-name-entries">
    {safeRenamed.sample.map((entry, index) => <li key={`${index}-${entry.physicalName}`} className="safe-name-entry">
      <div className="safe-name-entry-paths">
        <span className="safe-name-logical">{entry.logicalPath.join('/')}</span>
        <small className="safe-name-physical">{entry.physicalName}</small>
      </div>
    </li>)}
    {remaining > 0 && <li className="safe-name-entry safe-name-entry-more">{t.moreAdjusted(remaining)}</li>}
  </ul>
}

function RestoreGuide({ platform, folder }: {
  readonly platform: RestorePlatform
  readonly folder: string
}) {
  const t = useCopy(receiverCopy).downloads
  if (platform === 'windows') {
    const parts = t.restoreGuideWindowsParts
    return <p className="safe-name-guide">
      {parts.beforeFolder}<code>{folder}</code>{parts.afterFolder}<code>powershell</code>{parts.afterCommand}
    </p>
  }
  const parts = t.restoreGuidePosixParts
  return <p className="safe-name-guide">
    {parts.beforeFolder}<code>{folder}</code>{parts.afterFolder}
  </p>
}

function RestorableEntriesList({ steps }: { readonly steps: readonly SafeNameRestoreStep[] }) {
  const t = useCopy(receiverCopy).downloads
  const [copiedId, setCopiedId] = useState<string | null>(null)
  const timerRef = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)

  useEffect(() => () => {
    if (timerRef.current !== undefined) clearTimeout(timerRef.current)
  }, [])

  const copyName = async (id: string, name: string) => {
    if (!await copyTextToClipboard(name)) return
    setCopiedId(id)
    if (timerRef.current !== undefined) clearTimeout(timerRef.current)
    timerRef.current = setTimeout(() => { setCopiedId(null) }, COPIED_INDICATOR_MS)
  }

  return <div className="safe-name-list-section">
    <h4 className="safe-name-section-title">{t.restoreRestorableHeading(steps.length)}</h4>
    <ul className="safe-name-entries">
      {steps.map(step => {
        const id = `${step.parentPhysicalPath.join('/')}/${step.fromName}`
        const isCopied = copiedId === id
        return <li key={id} className="safe-name-entry">
          <div className="safe-name-entry-paths">
            <span className="safe-name-logical">{step.logicalPath.join('/')}</span>
            <small className="safe-name-physical">{step.fromName}</small>
          </div>
          <button
            type="button"
            className="quiet-action safe-name-copy-btn"
            onClick={() => { void copyName(id, step.toName) }}
          >{isCopied ? t.copied : t.copyOriginalName}</button>
        </li>
      })}
    </ul>
  </div>
}

function UnsupportedEntriesList({ entries }: { readonly entries: readonly SafeNameUnsupportedEntry[] }) {
  const t = useCopy(receiverCopy).downloads
  if (entries.length === 0) return null
  return <div className="safe-name-list-section safe-name-unsupported">
    <h4 className="safe-name-section-title">{t.restoreUnsupportedTitle(entries.length)}</h4>
    <ul className="safe-name-entries">
      {entries.map(entry => {
        const id = entry.physicalPath.join('/')
        const reason = entry.reason === 'case-collision' ? t.restoreReasonCaseCollision : t.restoreReasonInvalidOs
        return <li key={id} className="safe-name-entry safe-name-entry-unsupported">
          <div className="safe-name-entry-paths">
            <span className="safe-name-logical">{entry.logicalPath.join('/')}</span>
            <small className="safe-name-physical">{entry.fromName}</small>
          </div>
          <span className="safe-name-reason">{reason}</span>
        </li>
      })}
    </ul>
  </div>
}

function PlatformSwitch({ platform, onChange, disabled }: {
  readonly platform: RestorePlatform
  readonly onChange: (next: RestorePlatform) => void
  readonly disabled: boolean
}) {
  const t = useCopy(receiverCopy).downloads
  return <div className="safe-name-platform-row">
    <span className="safe-name-platform-label">{t.restorePlatformLabel}</span>
    <div className="safe-name-platform-switch" role="group" aria-label={t.restorePlatformLabel}>
      <button
        type="button"
        className={platform === 'windows' ? 'safe-name-platform-active' : ''}
        aria-pressed={platform === 'windows'}
        disabled={disabled}
        onClick={() => onChange('windows')}
      >{t.restorePlatformWindows}</button>
      <button
        type="button"
        className={platform === 'posix' ? 'safe-name-platform-active' : ''}
        aria-pressed={platform === 'posix'}
        disabled={disabled}
        onClick={() => onChange('posix')}
      >{t.restorePlatformPosix}</button>
    </div>
  </div>
}

function RestoreActionButtons({
  platform,
  plan,
  inlineCommand,
  exported,
  exporting,
  copiedCommand,
  busy,
  onCopy,
  onExport,
  onDownloadManifest,
}: {
  readonly platform: RestorePlatform
  readonly plan: SafeNameRestorePlan
  readonly inlineCommand: string | null
  readonly exported: SafeNameRestoreExportResult | null
  readonly exporting: boolean
  readonly copiedCommand: boolean
  readonly busy: boolean
  readonly onCopy: (command: string) => void
  readonly onExport: () => void
  readonly onDownloadManifest: (plan: SafeNameRestorePlan) => void
}) {
  const t = useCopy(receiverCopy).downloads
  if (exported !== null) {
    return <div className="safe-name-actions">
      <button type="button" className="primary-action" onClick={() => onCopy(exported.runCommand)}>
        {copiedCommand && <ReceiverIcon name="check" />}
        {copiedCommand ? t.copied : t.copyCommand}
      </button>
      <button type="button" className="quiet-action" onClick={() => onDownloadManifest(plan)}>
        {t.exportManifestJson}
      </button>
    </div>
  }
  if (inlineCommand !== null) {
    const saveLabel = platform === 'windows' ? t.saveRestoreArtifactWindows : t.saveRestoreArtifactPosix
    return <div className="safe-name-actions">
      <button type="button" className="primary-action" onClick={() => onCopy(inlineCommand)}>
        {copiedCommand && <ReceiverIcon name="check" />}
        {copiedCommand ? t.copied : t.copyRestoreCommand}
      </button>
      <button type="button" className="quiet-action" disabled={exporting || busy} onClick={onExport}>
        {exporting ? t.savingRestoreArtifact : saveLabel}
      </button>
    </div>
  }
  const savePrimaryLabel = platform === 'windows' ? t.saveRestoreArtifactPrimaryWindows : t.saveRestoreArtifactPrimaryPosix
  return <div className="safe-name-actions">
    <button type="button" className="primary-action" disabled={exporting || busy} onClick={onExport}>
      {exporting ? t.savingRestoreArtifact : savePrimaryLabel}
    </button>
    <button type="button" className="quiet-action" onClick={() => onDownloadManifest(plan)}>
      {t.exportManifestJson}
    </button>
  </div>
}

function RestoreFeedbackBanner({ kind, exported }: {
  readonly kind: KeyedFeedback['kind'] | null
  readonly exported: SafeNameRestoreExportResult | null
}) {
  const t = useCopy(receiverCopy).downloads
  if (kind === null) return null
  let message = t.restoreCopyFailed
  if (kind === 'saved' && exported !== null) {
    message = t.restoreSavedFeedback(exported.directoryLabel, exported.fileName)
  } else if (kind === 'copied') {
    message = exported === null ? t.restoreCopiedFeedback : t.restoreCopiedExportedFeedback(exported.fileName)
  }
  return <p
    className={kind === 'copy-failed' ? 'safe-name-feedback safe-name-feedback-warn' : 'safe-name-feedback'}
    role="status"
  >{message}</p>
}

function RestoreCommandPreview({ command }: { readonly command: string | null }) {
  const t = useCopy(receiverCopy).downloads
  if (command === null) return null
  return <details className="safe-name-command-preview">
    <summary className="safe-name-command-summary">
      <ReceiverIcon name="chevron-right" className="safe-name-chevron" />
      <span>{t.viewCommand}</span>
    </summary>
    <pre><code>{command}</code></pre>
  </details>
}

function AvailableRestoreBody({ operationId, actions, busy, platform, onPlatformChange, isOpen }: {
  readonly operationId: string
  readonly actions: SafeNameRestoreActions
  readonly busy: boolean
  readonly platform: RestorePlatform
  readonly onPlatformChange: (next: RestorePlatform) => void
  readonly isOpen: boolean
}) {
  const t = useCopy(receiverCopy).downloads
  const [reloadToken, setReloadToken] = useState(0)
  const [loadResult, setLoadResult] = useState<KeyedLoadResult | null>(null)
  const [exporting, setExporting] = useState(false)
  const [exportState, setExportState] = useState<KeyedExport | null>(null)
  const [feedback, setFeedback] = useState<KeyedFeedback | null>(null)
  const [copiedKey, setCopiedKey] = useState<string | null>(null)
  const copyTimerRef = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)

  const requestKey = `${operationId}:${platform}`

  // Action ports are non-reactive; wrapping them prevents unrelated parent re-renders
  // (such as 10Hz download progress updates) from restarting IndexedDB plan scans.
  const prepareRestore = useEffectEvent((id: string, targetPlatform: RestorePlatform) =>
    actions.prepare(id, targetPlatform),
  )
  const mapRestoreError = useEffectEvent((error: unknown) => actions.mapError(error))

  useEffect(() => () => {
    if (copyTimerRef.current !== undefined) clearTimeout(copyTimerRef.current)
  }, [])

  useEffect(() => {
    if (!isOpen) return
    let cancelled = false
    const key = `${operationId}:${platform}`
    void prepareRestore(operationId, platform).then(
      prepared => {
        if (!cancelled) setLoadResult({ key, outcome: { kind: 'ready', prepared } })
      },
      error => {
        if (!cancelled) setLoadResult({ key, outcome: { kind: 'error', failure: mapRestoreError(error) } })
      },
    )
    return () => {
      cancelled = true
    }
  }, [isOpen, operationId, platform, reloadToken])

  if (!isOpen) return null

  const activeOutcome = loadResult?.key === requestKey ? loadResult.outcome : null
  if (activeOutcome === null) {
    return <p className="safe-name-hint" role="status">{t.restoreLoading}</p>
  }
  if (activeOutcome.kind === 'error') {
    return <div className="safe-name-body">
      <div className="task-action-error" role="alert">
        <FailureNotice failure={activeOutcome.failure} context="action" />
      </div>
      <div className="safe-name-actions">
        <button type="button" className="quiet-action" onClick={() => setReloadToken(n => n + 1)}>{t.retry}</button>
      </div>
    </div>
  }

  const { plan, inlineCommand } = activeOutcome.prepared
  const currentExport = exportState?.key === requestKey ? exportState : null
  const exported = currentExport?.result ?? null
  const exportFailure = currentExport?.failure ?? null
  const activeFeedback = feedback?.key === requestKey ? feedback.kind : null
  const commandToAudit = exported?.runCommand ?? inlineCommand

  const handleCopyCommand = (command: string) => {
    void copyTextToClipboard(command).then(ok => {
      setFeedback({ key: requestKey, kind: ok ? 'copied' : 'copy-failed' })
      if (!ok) return
      setCopiedKey(requestKey)
      if (copyTimerRef.current !== undefined) clearTimeout(copyTimerRef.current)
      copyTimerRef.current = setTimeout(() => { setCopiedKey(null) }, COPIED_INDICATOR_MS)
    })
  }

  const handleExport = () => {
    if (exporting || busy) return
    setExporting(true)
    setExportState({ key: requestKey, result: null, failure: null })
    setFeedback(null)
    setCopiedKey(null)
    void actions.exportArtifact(operationId, platform).then(
      result => {
        setExportState({ key: requestKey, result, failure: null })
        setFeedback({ key: requestKey, kind: 'saved' })
      },
      error => {
        setExportState({ key: requestKey, result: null, failure: actions.mapError(error) })
      },
    ).finally(() => setExporting(false))
  }

  return <div className="safe-name-body">
    {plan.restorable.length > 0 && <>
      <RestoreGuide platform={platform} folder={plan.targetFolderLabel} />
      <RestoreActionButtons
        platform={platform}
        plan={plan}
        inlineCommand={inlineCommand}
        exported={exported}
        exporting={exporting}
        copiedCommand={copiedKey === requestKey}
        busy={busy}
        onCopy={handleCopyCommand}
        onExport={handleExport}
        onDownloadManifest={actions.downloadManifest}
      />
      <RestoreFeedbackBanner kind={activeFeedback} exported={exported} />
      {exportFailure !== null && <div className="task-action-error" role="alert">
        <FailureNotice failure={exportFailure} context="action" />
      </div>}
      <RestoreCommandPreview command={commandToAudit} />
      <RestorableEntriesList steps={plan.restorable} />
    </>}

    <UnsupportedEntriesList entries={plan.unsupported} />
    <PlatformSwitch platform={platform} onChange={onPlatformChange} disabled={exporting} />
  </div>
}

export function SafeNameRestorePanel({ task, initialOpen = false, actions, busy = false }: {
  readonly task: TaskPresentation
  readonly initialOpen?: boolean
  readonly actions?: SafeNameRestoreActions
  readonly busy?: boolean
}) {
  const t = useCopy(receiverCopy).downloads
  const [isOpen, setIsOpen] = useState(initialOpen)
  const [platform, setPlatform] = useState<RestorePlatform>(() => actions?.defaultPlatform() ?? 'posix')

  if (task.safeNameRestore === 'none' || task.safeRenamed === null) return null

  return <details
    className="detail-disclosure safe-name-panel"
    open={isOpen}
    onToggle={event => setIsOpen(event.currentTarget.open)}
  >
    <summary className="safe-name-summary">
      <span className="safe-name-summary-label">
        <ReceiverIcon name="chevron-right" className="safe-name-chevron" />
        <span>{t.adjustedNames}</span>
      </span>
      <span className="safe-name-count">{task.safeRenamed.count}</span>
    </summary>
    {task.safeNameRestore === 'active' && <div className="safe-name-body">
      <p className="safe-name-hint">{t.restoreActiveHint}</p>
      <SampleAdjustedList safeRenamed={task.safeRenamed} />
    </div>}
    {task.safeNameRestore === 'paused' && <div className="safe-name-body">
      <p className="safe-name-hint">{t.restorePausedHint}</p>
      <SampleAdjustedList safeRenamed={task.safeRenamed} />
    </div>}
    {task.safeNameRestore === 'available' && (actions !== undefined
      ? <AvailableRestoreBody
          operationId={task.operationId}
          actions={actions}
          busy={busy}
          platform={platform}
          onPlatformChange={setPlatform}
          isOpen={isOpen}
        />
      : <div className="safe-name-body">
          <SampleAdjustedList safeRenamed={task.safeRenamed} />
        </div>)}
  </details>
}
