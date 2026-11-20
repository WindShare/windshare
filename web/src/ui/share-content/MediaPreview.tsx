import { FailureNotice } from '../controls/FailureNotice'
import { receiverCopy } from '../locales/receiver'
import { useCopy } from '../preferences/use-preferences'
import type { PreviewSnapshot } from '../model'
import { ReceiverIcon } from '../receiver-presentation/ReceiverIcon'
import { VideoFrame } from './VideoFrame'

export interface PreviewActions {
  readonly close: () => void
  readonly seek: (seconds: number) => void
  readonly presented: (presentationId: number) => void
  readonly failed: (presentationId: number) => void
}

export function MediaPreview({ preview, actions, inline = false }: {
  readonly preview: PreviewSnapshot
  readonly actions: PreviewActions
  readonly inline?: boolean
}) {
  const t = useCopy(receiverCopy).browse
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
  </section>
}
