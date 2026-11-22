import { renderToString } from 'react-dom/server'
import { describe, expect, it, vi } from 'vitest'
import { Downloads } from '../../src/ui/downloads/Downloads'
import { presentTask } from '../../src/ui/tasks'
import { TASK_FIXTURES } from '../../src/ui/tasks/fixtures'

describe('Downloads inventory presentation', () => {
  function render(inventory: 'empty' | 'loading' | 'failed', populated = false) {
    return renderToString(<Downloads tasks={populated ? [presentTask(TASK_FIXTURES['saved-cleanup']!)] : []}
      actions={{ perform: vi.fn() }} inventory={inventory}
      busy={false} open onOpenChange={vi.fn()} onIntent={vi.fn()} />)
  }

  it('only explains empty history after inventory loading succeeds', () => {
    const ready = render('empty')
    expect(ready).toContain('No downloads yet.')
    const loading = render('loading')
    expect(loading).toContain('Loading downloads…')
    expect(loading).not.toContain('No downloads yet.')
    const failed = render('failed')
    expect(failed).toContain('role="alert"')
    expect(failed).toContain('Stored receive tasks could not be loaded.')
    expect(failed).not.toContain('No downloads yet.')
  })

  it.each([true, false])('keeps known records during unsettled inventory (loading=%s)', loading => {
    const html = render(loading ? 'loading' : 'failed', true)
    expect(html).toContain('class="task-card ')
    expect(html).not.toContain('No downloads yet.')
  })
})
