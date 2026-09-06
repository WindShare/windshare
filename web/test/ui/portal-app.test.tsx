import { renderToString } from 'react-dom/server'
import { describe, expect, it } from 'vitest'
import App from '../../src/App'
import { experienceController, experienceSnapshot } from './receiver-experience-fixture'

describe('Portal and receiver experience', () => {
  it('keeps Downloads available on the landing portal without promoting unrelated history', () => {
    const html = renderToString(<App controller={experienceController(experienceSnapshot())} />)
    expect(html).toContain('无需上传云端')
    expect(html).toContain('Downloads')
    expect(html).not.toContain('一键恢复任务')
  })

  it('gives authenticated share identity the main heading and keeps browsing available during local work', () => {
    const html = renderToString(<App controller={experienceController(experienceSnapshot({
      phase: 'browsing',
      share: { kind: 'browser', shareInstance: 'share', name: 'Holiday photos', homeDirectoryId: 'root', singleFolder: true },
      breadcrumbs: [{ id: 'root', name: 'Holiday photos' }, { id: 'child', name: 'Day one' }],
      rows: [{ id: 'album', kind: 'directory', name: 'Album', selection: 'mixed' }],
      draft: { mode: 'selection', scope: 'selected', label: 'Album', summary: '1 folder selected, excluding 1 item', empty: false },
      startAdmission: { allowed: false, reason: 'Finish saving the current result first.', canReleaseCurrent: false },
      retained: { kind: 'ready', operations: [], error: null, pending: { operationId: 'task', action: 'save' } },
    }))} />)
    expect(html).toContain('Holiday photos</h1>')
    expect(html).toContain('Finish saving the current result first.')
    expect(html).toMatch(/<button[^>]*title="Holiday photos"[^>]*>Holiday photos<\/button>/)
    expect(html).not.toMatch(/<button[^>]*disabled[^>]*>Album<\/button>/)
    expect(html).not.toMatch(/<input[^>]*disabled/)
    expect(html).toContain('aria-checked="mixed"')
    expect(html).not.toContain('Stored receive tasks')
    expect(html).not.toContain('Receive as</h2>')
  })

  it('keeps empty explicit selection distinct from Download all', () => {
    const html = renderToString(<App controller={experienceController(experienceSnapshot({
      phase: 'browsing', breadcrumbs: [{ id: 'root', name: 'Shared files' }],
      draft: { mode: 'selection', scope: 'selected', label: 'Selected items', summary: 'No items selected', empty: true },
    }))} />)
    expect(html).toMatch(/<button[^>]*disabled[^>]*>Download selected<\/button>/)
    expect(html).toContain('Select items to download')
    expect(html).toContain('Select this page')
    expect(html).toContain('Done selecting')
    expect(html).not.toContain('>Download all<')
  })

  it('keeps admitted local finalization visible as the current task without duplicating its history row', () => {
    const html = renderToString(<App controller={experienceController(experienceSnapshot({
      phase: 'browsing', breadcrumbs: [{ id: 'root', name: 'Shared files' }],
      retained: { kind: 'ready', error: null, pending: { operationId: 'local-task', action: 'continue' }, operations: [{
        operationId: 'local-task', receiveIntentDigest: 'intent', lifecycleGeneration: 1n,
        display: { objectLabel: 'Summer photos', createdAtMilliseconds: 1000 },
        lifecycle: { kind: 'resumable-package', operationId: 'local-task', receiveIntentDigest: 'intent', generation: 1n,
          sealedMaterializationDigest: 'materialization', tempCleanupProofDigest: 'cleanup' },
        continuation: 'resume-local-finalization', actions: ['continue'],
      }] },
    }))} />)
    expect(html).toContain('Finishing locally')
    expect(html).toContain('Download: Summer photos')
    expect(html.match(/class="task-card /g)).toHaveLength(1)
    expect(html).not.toMatch(/<button[^>]*>Finish and save<\/button>/)
  })

  it('uses an inline sole-file preview and preserves the original action after preview failure', () => {
    const html = renderToString(<App controller={experienceController(experienceSnapshot({
      phase: 'browsing',
      share: { kind: 'photo', shareInstance: 'share', name: 'Portrait.jpg',
        file: { id: 'photo', name: 'Portrait.jpg', kind: 'file', expectedSize: 1024n, selection: 'selected' } },
      preview: { state: 'error', fileId: 'photo', name: 'Portrait.jpg', message: 'Preview is too large.' },
    }))} />)
    expect(html).toContain('Portrait.jpg</h1>')
    expect(html).toContain('Preview is too large.')
    expect(html).toContain('You can still download the original file.')
    expect(html).toContain('Download original')
    expect(html).not.toContain('explorer-list')
  })
})
