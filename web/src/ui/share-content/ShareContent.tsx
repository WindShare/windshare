import { receiverCopy } from '../locales/receiver'
import { useCopy } from '../preferences/use-preferences'
import type { ComponentProps } from 'react'
import type { ReceiverSnapshot, ShareIdentity } from '../model'
import { formatBytes } from '../progress-presentation'
import { Explorer } from './Explorer'
import { fileDisplayInfo } from './file-presentation'
import { MediaPreview, type PreviewActions } from './MediaPreview'
import { ReceiverIcon } from '../receiver-presentation/ReceiverIcon'

function SingleFile({ share, preview, actions, openPreview }: {
  readonly share: Exclude<ShareIdentity, { kind: 'browser' }>
  readonly preview: ReceiverSnapshot['preview']
  readonly actions: PreviewActions
  readonly openPreview: (id: string, invoker: HTMLButtonElement) => void
}) {
  const t = useCopy(receiverCopy).browse
  const display = fileDisplayInfo(share.file.name, t)
  const size = share.file.expectedSize === undefined ? t.sizeUnknown : formatBytes(share.file.expectedSize)
  const previewable = share.kind === 'photo' || share.kind === 'video'

  if (!previewable && preview.state === 'idle') {
    return <section className="single-file single-file-static" aria-label={t.sharedFile}>
      <div className="single-file-card">
        <div className="single-file-hero">
          <div className={`single-file-emblem entry-icon-${display.icon}`}>
            <ReceiverIcon name={display.icon} className="single-file-emblem-icon" />
          </div>
          <div className="single-file-summary">
            <div className="single-file-meta-row">
              <span className="single-file-size">{size}</span>
              <span className="single-file-dot" aria-hidden="true">·</span>
              <span className="single-file-type">{display.typeLabel}</span>
            </div>
            {display.badge !== null && <span className="single-file-ext">{display.badge}</span>}
          </div>
        </div>
        <dl className="single-file-facts">
          <div className="single-file-fact">
            <dt>{t.size}</dt>
            <dd>{size}</dd>
          </div>
          <div className="single-file-fact">
            <dt>{t.format}</dt>
            <dd>{display.typeLabel}</dd>
          </div>
          <div className="single-file-fact">
            <dt>{t.protection}</dt>
            <dd>{t.encrypted}</dd>
          </div>
        </dl>
      </div>
    </section>
  }

  return <section className={`single-file single-file-${share.kind}`} aria-label={t.sharedFile}>
    <div className="single-file-stage">
      <div className="single-file-stage-bar">
        <p className="single-file-metadata">{size}{' · '}{display.typeLabel}</p>
        {display.badge !== null && <span className="single-file-ext">{display.badge}</span>}
      </div>
      {preview.state === 'idle' ? <div className="single-file-placeholder">
        <div className={`single-file-emblem entry-icon-${display.icon}`}>
          <ReceiverIcon name={display.icon} className="single-file-emblem-icon" />
        </div>
        <button type="button" className="preview-trigger-action"
          onClick={event => openPreview(share.file.id, event.currentTarget)}>
          <ReceiverIcon name={share.kind === 'video' ? 'play' : 'eye'} />
          {share.kind === 'video' ? t.previewFrame : t.preview}
        </button>
      </div> : <MediaPreview preview={preview} actions={actions} inline />}
    </div>
  </section>
}

export function ShareContent({ share, preview, previewActions, explorer }: {
  readonly share: ShareIdentity
  readonly preview: ReceiverSnapshot['preview']
  readonly previewActions: PreviewActions
  readonly explorer: ComponentProps<typeof Explorer>
}) {
  if (share.kind !== 'browser') {
    return <SingleFile share={share} preview={preview} actions={previewActions} openPreview={explorer.actions.preview} />
  }
  return <Explorer {...explorer} />
}
