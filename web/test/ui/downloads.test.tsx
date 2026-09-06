import { renderToString } from 'react-dom/server'
import { describe, expect, it, vi } from 'vitest'
import { Downloads } from '../../src/ui/downloads/Downloads'
import { presentTask } from '../../src/ui/tasks'
import { TASK_FIXTURES } from '../../src/ui/tasks/fixtures'

describe('Downloads inventory presentation', () => {
  function render(loading: boolean, error: string | null, populated = false) {
    return renderToString(<Downloads tasks={populated ? [presentTask(TASK_FIXTURES['saved-cleanup']!)] : []}
      actions={{ perform: vi.fn(), catchUp: vi.fn() }} loading={loading} error={error}
      busy={false} open onOpenChange={vi.fn()} onIntent={vi.fn()} />)
  }

  it('only explains empty history after inventory loading succeeds', () => {
    const ready = render(false, null)
    expect(ready).toContain('No downloads yet.')
    expect(ready).toContain('Downloads you start here will appear in this browser.')
    expect(ready).not.toContain('About retained downloads')
    const loading = render(true, null)
    expect(loading).toContain('Loading downloads…')
    expect(loading).not.toContain('No downloads yet.')
    const failed = render(false, 'Stored receive tasks could not be loaded.')
    expect(failed).toContain('role="alert"')
    expect(failed).toContain('Stored receive tasks could not be loaded.')
    expect(failed).not.toContain('No downloads yet.')
  })

  it.each([true, false])('keeps known records during unsettled inventory (loading=%s)', loading => {
    const html = render(loading, loading ? null : 'Stored receive tasks could not be loaded.', true)
    expect(html).toContain('class="task-card ')
    expect(html).toContain('About retained downloads')
    expect(html).not.toContain('No downloads yet.')
  })

  it('distinguishes dismissing the modal from leaving the current page', () => {
    const html = render(false, null)
    expect(html).toContain('aria-label="Close downloads"')
    expect(html).toContain('Tasks and saved records in this browser.')
    expect(html).not.toContain('Back to home')
    expect(html).not.toContain('Back to share')
  })
})
