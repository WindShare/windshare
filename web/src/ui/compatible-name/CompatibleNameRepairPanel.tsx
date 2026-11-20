import { receiverCopy } from '../locales/receiver'
import { useCopy } from '../preferences/use-preferences'
import { useState } from 'react'
import type { CompatibleNameRepairPresentation } from '../compatible-name-repair-presentation'

export function CompatibleNameRepairPanel(props: {
  readonly repair: CompatibleNameRepairPresentation | null
  readonly catchUp?: () => void
  readonly busy?: boolean
  readonly writeClipboard?: (text: string) => Promise<void>
}) {
  const t = useCopy(receiverCopy).restoration
  const repair = props.repair
  if (repair === null || repair.replacementCount === 0) return null
  return (
    <section
      className={`compatible-name-repair compatible-name-repair-${repair.actionMode}`}
      aria-label={t.panel}
    >
      <div role="status">
        <strong>{repair.noticeTitle}</strong>
        <p>{repair.noticeDescription}</p>
        <p>{repair.replacementCountLabel}</p>
      </div>
      {repair.visibility === 'notice' && <p>{repair.actionDescription}</p>}
      {repair.visibility === 'secondary' && (
          <details>
            <summary>{repair.scriptName === null ? t.mappings : t.restoreAfterStop}</summary>
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
  const t = useCopy(receiverCopy).restoration
  const { repair } = props
  return (
    <>
      <strong>{repair.actionTitle}</strong>
      <p>{repair.actionDescription}</p>
      {repair.actionMode === 'catch-up-required' && props.catchUp !== undefined && (
        <button type="button" disabled={props.busy} onClick={props.catchUp}>
          {t.finishSetup}
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
        <summary>{t.affected}</summary>
        {repair.logicalPathSample.length > 0 && (
          <ul>{repair.logicalPathSample.map(path => <li key={path}><code>{path}</code></li>)}</ul>
        )}
        {repair.omittedLogicalPathCount > 0 && (
          <p>{t.omitted(repair.omittedLogicalPathCount)}</p>
        )}
        <dl>
          {repair.scriptName !== null && <><dt>{t.script}</dt><dd><code>{repair.scriptName}</code></dd></>}
          <dt>{t.report}</dt><dd><code>{repair.sidecarName}</code></dd>
          <dt>{t.location}</dt><dd>{repair.placementLabel}</dd>
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
  const t = useCopy(receiverCopy).restoration
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
      <p>{t.runHint}</p>
      <button type="button" disabled={copyState === 'copying'} onClick={() => { void copy() }}>
        {copyState === 'copying' ? t.copying : t.copy}
      </button>
      <p role="status" aria-live="polite">
        {copyState === 'copied' && t.copied}
        {copyState === 'failed' && t.copyFailed}
      </p>
      <details open={copyState === 'failed' ? true : undefined}>
        <summary>{t.command}</summary>
        <pre><code>{props.command}</code></pre>
        <p>{t.policy}</p>
        <p>{t.shortCommand}</p>
        <pre><code>{props.shortCommand}</code></pre>
      </details>
      <details>
        <summary>{t.trouble}</summary>
        <p>{t.troubleHint}</p>
      </details>
    </>
  )
}

async function writeBrowserClipboard(text: string): Promise<void> {
  await navigator.clipboard.writeText(text)
}
