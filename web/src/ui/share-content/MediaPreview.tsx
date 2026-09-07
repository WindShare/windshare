import type { V2PreviewSnapshot } from '../v2-model'
import { ReceiverIcon } from '../receiver-presentation/ReceiverIcon'
import { VideoFrame } from './VideoFrame'

export interface PreviewActions {
  readonly close: () => void
  readonly seek: (seconds: number) => void
  readonly presented: (presentationId: number) => void
  readonly failed: (presentationId: number) => void
}

export function MediaPreview({ preview, actions, inline = false }: {
  readonly preview: V2PreviewSnapshot
  readonly actions: PreviewActions
  readonly inline?: boolean
}) {
  if (preview.state === 'idle') return null
  return <section className={`media-preview ${inline ? 'media-preview-inline' : ''}`} aria-label="File preview">
    {preview.state === 'loading' && <p className="preview-loading" role="status"><ReceiverIcon name="image" />Opening preview…</p>}
    {preview.state === 'error' && <div className="preview-failure">
      <ReceiverIcon name="file" /><p role="alert">{preview.message}</p><p>You can still download the original file.</p>
    </div>}
    {preview.state === 'image' && <img className="preview-media" src={preview.url}
      alt={`Preview of ${preview.name}`}
      onLoad={() => actions.presented(preview.presentationId)}
      onError={() => actions.failed(preview.presentationId)} />}
    {preview.state === 'video' && <VideoFrame key={preview.fileId} preview={preview} actions={actions} />}
    {inline && <button className="quiet-action" type="button" onClick={actions.close}><ReceiverIcon name="close" />Close preview</button>}
  </section>
}
