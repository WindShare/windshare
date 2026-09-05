import { useState } from 'react'
import type { CompatibleNameRepairPresentation } from '../compatible-name-repair-presentation'

export function CompatibleNameRepairPanel(props: {
  readonly repair: CompatibleNameRepairPresentation | null
  readonly catchUp?: () => void
  readonly busy?: boolean
  readonly writeClipboard?: (text: string) => Promise<void>
}) {
  const repair = props.repair
  if (repair === null || repair.replacementCount === 0) return null
  return (
    <section
      className={`compatible-name-repair compatible-name-repair-${repair.actionMode}`}
      aria-label="Compatible-name restoration"
    >
      <div role="status">
        <strong>{repair.noticeTitle}</strong>
        <p>{repair.noticeDescription}</p>
        <p>{repair.replacementCountLabel}</p>
      </div>
      {repair.visibility === 'notice' && <p>{repair.actionDescription}</p>}
      {repair.visibility === 'secondary' && (
          <details>
            <summary>Restore names after stopping</summary>
            <RestorationAction {...props} repair={repair} />
          </details>
        )}
      {repair.visibility === 'primary' && <RestorationAction {...props} repair={repair} />}
    </section>
  )
}

function RestorationAction(props: {
  readonly repair: CompatibleNameRepairPresentation
  readonly catchUp?: () => void
  readonly busy?: boolean
  readonly writeClipboard?: (text: string) => Promise<void>
}) {
  const { repair } = props
  return (
    <>
      <strong>{repair.actionTitle}</strong>
      <p>{repair.actionDescription}</p>
      {repair.actionMode === 'catch-up-required' && props.catchUp !== undefined && (
        <button type="button" disabled={props.busy} onClick={props.catchUp}>
          Finish local restoration catch-up
        </button>
      )}
      {repair.runCommand !== null && repair.shortCommand !== null && (
        <RestorationCommand
          key={repair.runCommand}
          command={repair.runCommand}
          shortCommand={repair.shortCommand}
          {...(props.writeClipboard === undefined ? {} : { writeClipboard: props.writeClipboard })}
        />
      )}
      <details>
        <summary>Affected names and restoration files</summary>
        {repair.logicalPathSample.length > 0 && (
          <ul>{repair.logicalPathSample.map(path => <li key={path}><code>{path}</code></li>)}</ul>
        )}
        {repair.omittedLogicalPathCount > 0 && (
          <p>{repair.omittedLogicalPathCount} more replacement(s) are not shown.</p>
        )}
        <dl>
          <dt>Restoration script</dt><dd><code>{repair.scriptName}</code></dd>
          <dt>Restoration sidecar</dt><dd><code>{repair.sidecarName}</code></dd>
          <dt>Location</dt><dd>{repair.placementLabel}</dd>
        </dl>
      </details>
    </>
  )
}

function RestorationCommand(props: {
  readonly command: string
  readonly shortCommand: string
  readonly writeClipboard?: (text: string) => Promise<void>
}) {
  const [copyState, setCopyState] = useState<'idle' | 'copying' | 'copied' | 'failed'>('idle')
  const copy = async () => {
    setCopyState('copying')
    try {
      await (props.writeClipboard ?? writeBrowserClipboard)(props.command)
      setCopyState('copied')
    } catch {
      setCopyState('failed')
    }
  }
  return (
    <>
      <p>Open PowerShell in the folder containing the restoration files and paste the command.
        Keep both paired files together and preserve their position relative to the downloaded content.</p>
      <button type="button" disabled={copyState === 'copying'} onClick={() => { void copy() }}>
        {copyState === 'copying' ? 'Copying…' : 'Copy restoration command'}
      </button>
      <p role="status" aria-live="polite">
        {copyState === 'copied' && 'Restoration command copied.'}
        {copyState === 'failed' && 'Could not copy. Select and copy the command from the details below.'}
      </p>
      <details open={copyState === 'failed' ? true : undefined}>
        <summary>Command details</summary>
        <pre><code>{props.command}</code></pre>
        <p>The launch options apply only to this PowerShell process. No permanent execution-policy change is needed.</p>
        <p>If your environment already permits scripts, you can use this short command:</p>
        <pre><code>{props.shortCommand}</code></pre>
      </details>
      <details>
        <summary>Unable to run?</summary>
        <p>Use the complete command above in PowerShell from the restoration files' folder.
          Check that the matching .ps1 and .data files are both present.
          For an unfinished receive, the script asks you to confirm before restoring names.</p>
      </details>
    </>
  )
}

async function writeBrowserClipboard(text: string): Promise<void> {
  await navigator.clipboard.writeText(text)
}
