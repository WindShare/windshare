import { describe, expect, it, vi } from 'vitest'
import { createPreferencesStore, parsePreferences } from '../../src/ui/preferences/model'

describe('UI preferences', () => {
  it('chooses a supported language without overriding the first browser preference', () => {
    expect(parsePreferences(null, ['zh-HK', 'en'])).toEqual({ language: 'zh-CN', appearance: 'system', motion: 'system' })
    expect(parsePreferences(null, ['en-US', 'zh-CN']).language).toBe('en')
    expect(parsePreferences(null, ['fr-FR']).language).toBe('en')
    expect(parsePreferences(null, []).language).toBe('en')
  })

  it('recovers from malformed storage and validates saved fields independently', () => {
    for (const raw of ['{broken', 'null', '[]', '42', '"dark"']) {
      expect(parsePreferences(raw, ['zh-CN'])).toEqual({ language: 'zh-CN', appearance: 'system', motion: 'system' })
    }
    expect(parsePreferences(JSON.stringify({ language: 'en', appearance: 'sepia', motion: 'reduced' }), ['zh-CN']))
      .toEqual({ language: 'en', appearance: 'system', motion: 'reduced' })
    expect(parsePreferences(JSON.stringify({ language: 'invalid', appearance: 'dark' }), ['en']))
      .toEqual({ language: 'en', appearance: 'dark', motion: 'system' })
  })

  it('merges local changes, notifies subscribers, and persists the complete preference', () => {
    const persist = vi.fn()
    const store = createPreferencesStore(parsePreferences(null, ['en']), persist)
    const notify = vi.fn()
    const unsubscribe = store.subscribe(notify)
    store.update({ appearance: 'dark' })
    store.update({ language: 'zh-CN' })
    expect(notify).toHaveBeenCalledTimes(2)
    expect(persist).toHaveBeenLastCalledWith({ language: 'zh-CN', appearance: 'dark', motion: 'system' })
    unsubscribe()
    store.update({ motion: 'reduced' })
    expect(notify).toHaveBeenCalledTimes(2)
  })

  it('accepts cross-tab snapshots without echoing writes or notifying unchanged subscribers', () => {
    const initial = parsePreferences(null, ['en'])
    const persist = vi.fn()
    const store = createPreferencesStore(initial, persist)
    const notify = vi.fn()
    store.subscribe(notify)
    store.replace({ ...initial })
    expect(store.getSnapshot()).toBe(initial)
    expect(notify).not.toHaveBeenCalled()
    store.replace({ ...initial, appearance: 'light' })
    expect(notify).toHaveBeenCalledOnce()
    expect(persist).not.toHaveBeenCalled()
  })
})
