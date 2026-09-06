import type { ComponentProps } from 'react'
import type { V2ReceiverSnapshot, V2ShareIdentity } from '../v2-model'
import { formatBytes } from '../v2-progress-presentation'
import { Explorer } from './Explorer'
import { MediaPreview, type PreviewActions } from './MediaPreview'
import { ReceiverIcon, type ReceiverIconName } from '../receiver-presentation/ReceiverIcon'

function SingleFile({ share, preview, actions, openPreview }: {
  readonly share: Exclude<V2ShareIdentity, { kind: 'browser' }>
  readonly preview: V2ReceiverSnapshot['preview']
  readonly actions: PreviewActions
  readonly openPreview: (id: string) => void
}) {
  const kind = { photo: 'Photo', video: 'Video', file: 'File' }[share.kind]
  const icons: Readonly<Record<typeof share.kind, ReceiverIconName>> = { photo: 'image', video: 'video', file: 'file' }
  return <section className={`single-file single-file-${share.kind}`} aria-label="Shared file">
    <p className="single-file-metadata">{share.file.expectedSize === undefined ? 'Size unknown' : formatBytes(share.file.expectedSize)}
      {' · '}{kind}</p>
    {preview.state === 'idle' ? <div className="single-file-placeholder">
      <ReceiverIcon name={icons[share.kind]} className="single-file-icon" />
      <button type="button" onClick={() => openPreview(share.file.id)}>
        {share.kind === 'video' ? 'Preview a frame' : 'Preview'}
      </button>
    </div> : <MediaPreview preview={preview} actions={actions} inline />}
  </section>
}

export function ShareContent({ share, preview, previewActions, explorer }: {
  readonly share: V2ShareIdentity | null
  readonly preview: V2ReceiverSnapshot['preview']
  readonly previewActions: PreviewActions
  readonly explorer: ComponentProps<typeof Explorer>
}) {
  if (share !== null && share.kind !== 'browser') {
    return <SingleFile share={share} preview={preview} actions={previewActions} openPreview={explorer.actions.preview} />
  }
  return <Explorer {...explorer} />
}
