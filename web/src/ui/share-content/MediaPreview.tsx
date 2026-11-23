import { useEffect, type ReactNode } from 'react'
import { FailureNotice } from '../controls/FailureNotice'
import { receiverCopy } from '../locales/receiver'
import { useCopy } from '../preferences/use-preferences'
import type { PreviewSnapshot, ReceiverFailure } from '../model'
import { ReceiverIcon, type ReceiverIconName } from '../receiver-presentation/ReceiverIcon'
import { VideoFrame } from './VideoFrame'

export interface PreviewActions {
  readonly close: () => void
  readonly seek: (seconds: number) => void
  readonly presented: (presentationId: number) => void
  readonly failed: (presentationId: number) => void
}

export interface PreviewNavigation {
  readonly index: number
  readonly total: number
  readonly previous: () => void
  readonly next: () => void
}

export interface PreviewDownloadControl {
  readonly onDownload: () => void
  readonly downloading: boolean
  readonly started: boolean
  readonly disabledReason: string | null
}

function isEditableOrInteractiveInput(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false
  if (target.isContentEditable) return true
  const tag = target.tagName
  return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT'
}

function previewDownloadPresentation(
  fileName: string,
  download: PreviewDownloadControl,
  t: typeof receiverCopy.en.browse,
): Readonly<{
  label: string
  icon: ReceiverIconName
  disabled: boolean
  title: string
  ariaLabel: string
}> {
  if (download.downloading) {
    return {
      label: t.downloading,
      icon: 'clock',
      disabled: true,
      title: t.downloading,
      ariaLabel: t.downloading,
    }
  }
  if (download.started) {
    return {
      label: t.downloadStarted,
      icon: 'check',
      disabled: false,
      title: t.downloadItem(fileName),
      ariaLabel: t.downloadStarted,
    }
  }
  const title = download.disabledReason ?? t.downloadItem(fileName)
  const ariaLabel = download.disabledReason === null
    ? t.download
    : `${t.downloadItem(fileName)} — ${download.disabledReason}`
  return {
    label: t.download,
    icon: 'download',
    disabled: download.disabledReason !== null,
    title,
    ariaLabel,
  }
}

function PreviewModalFooter({
  fileName,
  navigation,
  download,
  downloadError,
  statusStrip,
}: {
  readonly fileName: string
  readonly navigation?: PreviewNavigation
  readonly download?: PreviewDownloadControl
  readonly downloadError: ReceiverFailure | null
  readonly statusStrip?: ReactNode
}) {
  const t = useCopy(receiverCopy).browse
  const hasNavigation = navigation !== undefined && navigation.total > 1 && navigation.index >= 0
  if (!hasNavigation && download === undefined && downloadError === null && statusStrip === undefined) {
    return null
  }
  const button = download === undefined ? null : previewDownloadPresentation(fileName, download, t)
  return <div className="media-preview-modal-footer">
    {(hasNavigation || button !== null) && <div className="media-preview-modal-bar">
      {hasNavigation && navigation !== undefined ? (
        <div className="media-preview-nav">
          <button type="button" className="quiet-action" disabled={navigation.index <= 0}
            aria-label={t.previousItem} title={t.previousItem} onClick={navigation.previous}>
            <ReceiverIcon name="chevron-left" />
            <span>{t.previousItem}</span>
          </button>
          <span className="media-preview-counter">{navigation.index + 1} / {navigation.total}</span>
          <button type="button" className="quiet-action" disabled={navigation.index + 1 >= navigation.total}
            aria-label={t.nextItem} title={t.nextItem} onClick={navigation.next}>
            <span>{t.nextItem}</span>
            <ReceiverIcon name="chevron-right" />
          </button>
        </div>
      ) : <span />}
      {button !== null && download !== undefined && (
        <button type="button" className="media-preview-download"
          disabled={button.disabled}
          title={button.title}
          aria-label={button.ariaLabel}
          data-started={download.started ? 'true' : undefined}
          data-downloading={download.downloading ? 'true' : undefined}
          onClick={download.onDownload}>
          <ReceiverIcon name={button.icon} />
          <span>{button.label}</span>
        </button>
      )}
    </div>}
    {downloadError !== null && <div className="media-preview-error" role="alert">
      <FailureNotice failure={downloadError} context="action" />
    </div>}
    {statusStrip}
  </div>
}

export function MediaPreview({
  preview, actions, inline = false, navigation, download, downloadError = null, statusStrip,
}: {
  readonly preview: PreviewSnapshot
  readonly actions: PreviewActions
  readonly inline?: boolean
  readonly navigation?: PreviewNavigation
  readonly download?: PreviewDownloadControl
  readonly downloadError?: ReceiverFailure | null
  readonly statusStrip?: ReactNode
}) {
  const t = useCopy(receiverCopy).browse
  const hasNavigation = !inline && navigation !== undefined && navigation.total > 1 && navigation.index >= 0

  useEffect(() => {
    if (!hasNavigation || navigation === undefined) return
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.defaultPrevented || event.altKey || event.ctrlKey || event.metaKey || event.shiftKey) return
      if (isEditableOrInteractiveInput(event.target)) return
      if (event.key === 'ArrowLeft' && navigation.index > 0) {
        event.preventDefault()
        navigation.previous()
      } else if (event.key === 'ArrowRight' && navigation.index + 1 < navigation.total) {
        event.preventDefault()
        navigation.next()
      }
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [hasNavigation, navigation])

  if (preview.state === 'idle') return null

  return <section className={`media-preview ${inline ? 'media-preview-inline' : ''}`} aria-label={t.filePreview}>
    {preview.state === 'loading' && <p className="preview-loading" role="status"><ReceiverIcon name="image" />{t.loadingPreview}</p>}
    {preview.state === 'error' && <div className="preview-failure">
      <ReceiverIcon name="file" />
      <div role="alert"><FailureNotice failure={preview.message} context="preview" /></div>
      <p>{t.downloadOriginalHint}</p>
    </div>}
    {preview.state === 'image' && <img className="preview-media" src={preview.url}
      alt={t.previewOf(preview.name)}
      onLoad={() => actions.presented(preview.presentationId)}
      onError={() => actions.failed(preview.presentationId)} />}
    {preview.state === 'video' && <VideoFrame key={preview.fileId} preview={preview} actions={actions} />}
    {inline && <div className="media-preview-footer">
      {preview.state === 'image' && <span className="preview-dimensions">{preview.width} × {preview.height}</span>}
      <button className="quiet-action" type="button" onClick={actions.close}><ReceiverIcon name="close" />{t.closePreview}</button>
    </div>}
    {!inline && <PreviewModalFooter
      fileName={preview.name}
      {...(navigation === undefined ? {} : { navigation })}
      {...(download === undefined ? {} : { download })}
      downloadError={downloadError}
      statusStrip={statusStrip}
    />}
  </section>
}
