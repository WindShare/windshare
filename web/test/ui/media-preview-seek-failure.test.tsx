import { renderToString } from 'react-dom/server'
import { describe, expect, it } from 'vitest'
import type { PreviewSnapshot } from '../../src/ui/model'
import { MediaPreview, type PreviewActions } from '../../src/ui/share-content/MediaPreview'

const actions: PreviewActions = {
  close: () => undefined,
  seek: () => undefined,
  presented: () => undefined,
  failed: () => undefined,
}

function video(overrides: Partial<Extract<PreviewSnapshot, { state: 'video' }>>): PreviewSnapshot {
  return {
    state: 'video',
    fileId: 'file-1',
    name: 'clip.mp4',
    url: 'blob:clip',
    presentationId: 3,
    mimeType: 'video/mp4',
    width: 64,
    height: 64,
    durationSeconds: 2,
    positionSeconds: 0.5,
    frameSeconds: 0,
    seeking: false,
    failure: null,
    ...overrides,
  }
}

describe('video preview seek failure presentation', () => {
  it('keeps the frame, slider and download entry when one seek fails', () => {
    const html = renderToString(<MediaPreview preview={video({ failure: 'MP4 sample geometry exceeds the preview limits' })}
      actions={actions} inline />)
    expect(html).toContain('<p role="alert">MP4 sample geometry exceeds the preview limits</p>')
    expect(html).toContain('You can still download the original file.')
    expect(html).toContain('aria-label="Seek clip.mp4"')
    expect(html).toContain('canvas')
    expect(html).not.toContain('Loading frame…')
    expect(html).not.toContain('Choose a moment to preview.')
  })

  it('shows the loading status and seek hint while a frame is pending', () => {
    const html = renderToString(<MediaPreview preview={video({ seeking: true })} actions={actions} inline />)
    expect(html).toContain('Loading frame…')
    expect(html).toContain('Choose a moment to preview.')
    expect(html).not.toContain('role="alert"')
  })
})
