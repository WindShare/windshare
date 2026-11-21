export const ADMIN_CSP = [
  "default-src 'none'",
  "script-src 'self'",
  "style-src 'self'",
  "connect-src 'self'",
  "img-src 'self'",
  "base-uri 'none'",
  "form-action 'self'",
  "frame-ancestors 'none'",
].join('; ')

export const ADMIN_HTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="robots" content="noindex, nofollow">
  <title>WindShare Donations Admin</title>
  <link rel="stylesheet" href="/admin/app.css">
</head>
<body>
  <main id="app" class="admin-shell">
    <header class="admin-header">
      <div>
        <h1>WindShare Donations</h1>
        <p id="status" class="muted">Checking session...</p>
      </div>
      <div id="header-actions" class="row hidden">
        <button id="sync-afdian-btn" type="button" class="btn">Sync Afdian</button>
        <button id="refresh-btn" type="button" class="btn">Refresh</button>
        <button id="logout-btn" type="button" class="btn btn-subtle">Sign out</button>
      </div>
    </header>

    <section id="login-card" class="card hidden">
      <form id="login-form" class="stack">
        <label for="password-input" class="label">Admin password</label>
        <input id="password-input" type="password" autocomplete="current-password" required class="input">
        <button type="submit" class="btn btn-primary">Sign in</button>
      </form>
    </section>

    <section id="console-view" class="stack hidden">
      <form id="config-form" class="card config-grid">
        <label class="field">
          <span class="label">Min public CNY (minor)</span>
          <input id="cfg-cny" type="number" min="0" step="100" class="input">
        </label>
        <label class="field">
          <span class="label">Min public USD (minor)</span>
          <input id="cfg-usd" type="number" min="0" step="100" class="input">
        </label>
        <label class="checkbox-field">
          <input id="cfg-moderation" type="checkbox">
          <span>Review before publish</span>
        </label>
        <label class="checkbox-field">
          <input id="cfg-crypto-paused" type="checkbox">
          <span>Pause Crypto checkout</span>
        </label>
        <div class="config-actions">
          <button type="submit" class="btn btn-primary">Save settings</button>
        </div>
      </form>

      <div id="filters" class="row wrap">
        <button type="button" class="chip active" data-filter="all">All</button>
        <button type="button" class="chip" data-filter="pending_review">Pending review</button>
        <button type="button" class="chip" data-filter="published">Published</button>
        <button type="button" class="chip" data-filter="below_threshold">Below threshold</button>
        <button type="button" class="chip" data-filter="hidden">Hidden</button>
        <button type="button" class="chip" data-filter="private_donor">Private</button>
      </div>

      <ul id="donation-list" class="donation-list"></ul>
      <div class="center">
        <button id="load-more-btn" type="button" class="btn hidden">Load more</button>
      </div>
    </section>
  </main>
  <script src="/admin/app.js" defer></script>
</body>
</html>
`

export const ADMIN_APP_CSS = `:root {
  color-scheme: light dark;
  --bg: light-dark(#f8f9f3, #0d1915);
  --surface: light-dark(#ffffff, #15261e);
  --inset: light-dark(#f3f6ef, #0f1d16);
  --border: light-dark(#d8e4d8, #304638);
  --text: light-dark(#173f35, #edf6ee);
  --muted: light-dark(#526c5e, #afc3b5);
  --accent: light-dark(#2e7555, #9ce8c4);
  --on-accent: light-dark(#fffef7, #143f32);
  --warn: light-dark(#9a5b13, #f0c27b);
  --danger: light-dark(#a94439, #efb7a8);
  font-family: Inter, ui-sans-serif, system-ui, -apple-system, sans-serif;
}
* { box-sizing: border-box; }
body {
  margin: 0;
  padding: 24px 16px 48px;
  background: var(--bg);
  color: var(--text);
}
.admin-shell { max-width: 960px; margin: 0 auto; display: flex; flex-direction: column; gap: 20px; }
.admin-header { display: flex; justify-content: space-between; align-items: flex-start; gap: 16px; flex-wrap: wrap; }
.admin-header h1 { margin: 0 0 4px; font-size: 22px; font-weight: 600; letter-spacing: -0.02em; }
.muted { margin: 0; color: var(--muted); font-size: 13px; }
.hidden { display: none !important; }
.row { display: flex; align-items: center; gap: 8px; }
.wrap { flex-wrap: wrap; }
.stack { display: flex; flex-direction: column; gap: 14px; }
.center { display: flex; justify-content: center; }
.card {
  padding: 20px;
  border: 1px solid var(--border);
  border-radius: 12px;
  background: var(--surface);
}
.config-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(180px, 1fr)); gap: 14px; align-items: end; }
.field { display: flex; flex-direction: column; gap: 6px; }
.label { font-size: 12px; color: var(--muted); font-weight: 500; }
.input {
  width: 100%;
  min-height: 38px;
  padding: 8px 10px;
  border: 1px solid var(--border);
  border-radius: 8px;
  background: var(--inset);
  color: var(--text);
  font: inherit;
  font-size: 13px;
}
.checkbox-field { display: flex; align-items: center; gap: 8px; min-height: 38px; font-size: 13px; cursor: pointer; }
.config-actions { display: flex; justify-content: flex-end; }
.btn, .chip {
  min-height: 36px;
  padding: 6px 12px;
  border: 1px solid var(--border);
  border-radius: 8px;
  background: var(--surface);
  color: var(--text);
  font: inherit;
  font-size: 13px;
  font-weight: 500;
  cursor: pointer;
}
.btn-primary { background: var(--accent); color: var(--on-accent); border-color: transparent; }
.btn-subtle { color: var(--muted); }
.btn-danger { color: var(--danger); }
.btn:disabled { opacity: 0.55; cursor: wait; }
.chip.active { background: var(--accent); color: var(--on-accent); border-color: transparent; }
.donation-list { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 10px; }
.donation-item {
  padding: 16px;
  border: 1px solid var(--border);
  border-radius: 12px;
  background: var(--surface);
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.donation-head { display: flex; justify-content: space-between; align-items: center; gap: 12px; flex-wrap: wrap; }
.donation-meta { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; font-size: 13px; }
.badge {
  display: inline-flex;
  align-items: center;
  padding: 2px 8px;
  border-radius: 999px;
  border: 1px solid var(--border);
  background: var(--inset);
  font-size: 11px;
  font-weight: 600;
}
.badge-pending { color: var(--warn); }
.badge-hidden { color: var(--danger); }
.donation-msg { margin: 0; font-size: 14px; line-height: 1.5; white-space: pre-wrap; word-break: break-word; }
.donation-actions { display: flex; gap: 8px; flex-wrap: wrap; align-items: center; }
.reason-input { max-width: 240px; min-height: 34px; }
`

export const ADMIN_APP_JS = `(() => {
  const state = { csrf: '', items: [], nextCursor: null, filter: 'all' }

  const el = id => document.getElementById(id)
  const setStatus = text => { el('status').textContent = text }
  const show = (node, visible) => node.classList.toggle('hidden', !visible)

  function formatMoney(item) {
    if (item.minorDigits === 0) return item.currency + ' ' + item.amountMinor
    const scale = 10 ** item.minorDigits
    const whole = Math.floor(item.amountMinor / scale)
    const frac = String(item.amountMinor % scale).padStart(item.minorDigits, '0')
    return item.currency + ' ' + whole + '.' + frac
  }

  function applyConfig(cfg) {
    if (!cfg) return
    el('cfg-cny').value = String(cfg.minPublicCnyMinor)
    el('cfg-usd').value = String(cfg.minPublicUsdMinor)
    el('cfg-moderation').checked = Boolean(cfg.moderationEnabled)
    el('cfg-crypto-paused').checked = Boolean(cfg.cryptoCheckoutPaused)
  }

  async function api(path, options = {}) {
    const headers = { ...(options.headers || {}) }
    if (options.body !== undefined) headers['Content-Type'] = 'application/json'
    if (options.method && options.method !== 'GET' && state.csrf) {
      headers['X-CSRF-Token'] = state.csrf
    }
    const res = await fetch(path, { ...options, headers, credentials: 'same-origin' })
    const data = await res.json().catch(() => ({}))
    return { ok: res.ok, status: res.status, data }
  }

  function renderItemActions(item) {
    const bar = document.createElement('div')
    bar.className = 'donation-actions'
    const kind = item.visibility.kind
    if (kind === 'private_donor') return bar

    if (kind === 'pending_review' || kind === 'below_threshold') {
      const btn = document.createElement('button')
      btn.type = 'button'
      btn.className = 'btn btn-primary'
      btn.textContent = 'Approve'
      btn.addEventListener('click', () => moderate(item.id, { action: 'approve' }))
      bar.appendChild(btn)
    }

    if (kind === 'hidden') {
      const btn = document.createElement('button')
      btn.type = 'button'
      btn.className = 'btn'
      btn.textContent = 'Restore'
      btn.addEventListener('click', () => moderate(item.id, { action: 'restore' }))
      bar.appendChild(btn)
    } else {
      const input = document.createElement('input')
      input.type = 'text'
      input.placeholder = 'Hide reason'
      input.className = 'input reason-input'
      const btn = document.createElement('button')
      btn.type = 'button'
      btn.className = 'btn btn-danger'
      btn.textContent = 'Hide'
      btn.addEventListener('click', () => {
        const reason = input.value.trim() || 'moderated'
        moderate(item.id, { action: 'hide', reason })
      })
      bar.appendChild(input)
      bar.appendChild(btn)
    }
    return bar
  }

  function renderList() {
    const list = el('donation-list')
    list.replaceChildren()
    const visible = state.filter === 'all'
      ? state.items
      : state.items.filter(item => item.visibility.kind === state.filter)

    if (visible.length === 0) {
      const empty = document.createElement('li')
      empty.className = 'donation-item muted'
      empty.textContent = 'No records in this view.'
      list.appendChild(empty)
    }

    for (const item of visible) {
      const li = document.createElement('li')
      li.className = 'donation-item'

      const head = document.createElement('div')
      head.className = 'donation-head'

      const meta = document.createElement('div')
      meta.className = 'donation-meta'
      const name = document.createElement('strong')
      name.textContent = item.displayName || 'Anonymous'
      const amount = document.createElement('span')
      amount.className = 'badge'
      amount.textContent = formatMoney(item)
      const provider = document.createElement('span')
      provider.className = 'badge'
      provider.textContent = item.provider
      const vis = document.createElement('span')
      vis.className = 'badge'
      if (item.visibility.kind === 'pending_review') vis.classList.add('badge-pending')
      if (item.visibility.kind === 'hidden') vis.classList.add('badge-hidden')
      vis.textContent = item.visibility.reason
        ? item.visibility.kind + ' (' + item.visibility.reason + ')'
        : item.visibility.kind
      meta.append(name, amount, provider, vis)

      const time = document.createElement('span')
      time.className = 'muted'
      time.textContent = new Date(item.donatedAt * 1000).toISOString()

      head.append(meta, time)
      li.appendChild(head)

      if (item.message) {
        const msg = document.createElement('p')
        msg.className = 'donation-msg'
        msg.textContent = item.message
        li.appendChild(msg)
      }

      li.appendChild(renderItemActions(item))
      list.appendChild(li)
    }

    show(el('load-more-btn'), state.nextCursor !== null)
  }

  async function loadDonations(reset) {
    const query = !reset && state.nextCursor ? '?after=' + encodeURIComponent(state.nextCursor) : ''
    const res = await api('/v1/admin/donations' + query)
    if (!res.ok) {
      if (res.status === 401) showLoggedOut('Session expired.')
      else setStatus('Failed to load donations.')
      return
    }
    applyConfig(res.data.config)
    state.items = reset ? res.data.items : state.items.concat(res.data.items)
    state.nextCursor = res.data.nextCursor
    renderList()
    setStatus('Loaded ' + state.items.length + ' records.')
  }

  async function moderate(id, command) {
    const res = await api('/v1/admin/donations/' + encodeURIComponent(id) + '/visibility', {
      method: 'POST',
      body: JSON.stringify(command),
    })
    if (!res.ok) {
      setStatus('Action failed: ' + (res.data.error || res.status))
      return
    }
    state.items = state.items.map(item => (item.id === id ? res.data.record : item))
    renderList()
    setStatus('Updated record ' + id + '.')
  }

  function showLoggedOut(message) {
    state.csrf = ''
    state.items = []
    state.nextCursor = null
    show(el('header-actions'), false)
    show(el('console-view'), false)
    show(el('login-card'), true)
    setStatus(message)
  }

  async function showLoggedIn(csrf, cfg) {
    state.csrf = csrf
    applyConfig(cfg)
    show(el('login-card'), false)
    show(el('header-actions'), true)
    show(el('console-view'), true)
    await loadDonations(true)
  }

  el('login-form').addEventListener('submit', async event => {
    event.preventDefault()
    const input = el('password-input')
    setStatus('Signing in...')
    const res = await api('/v1/admin/login', {
      method: 'POST',
      body: JSON.stringify({ password: input.value }),
    })
    input.value = ''
    if (!res.ok) {
      setStatus('Sign in failed: ' + (res.data.error || res.status))
      return
    }
    await showLoggedIn(res.data.csrfToken, null)
  })

  el('logout-btn').addEventListener('click', async () => {
    await api('/v1/admin/logout', { method: 'POST' })
    showLoggedOut('Signed out.')
  })

  el('refresh-btn').addEventListener('click', () => loadDonations(true))
  el('load-more-btn').addEventListener('click', () => loadDonations(false))

  el('sync-afdian-btn').addEventListener('click', async () => {
    setStatus('Syncing recent Afdian orders...')
    const res = await api('/v1/admin/donations/sync-afdian', { method: 'POST' })
    if (!res.ok) {
      setStatus('Sync failed: ' + (res.data.error || res.status))
      return
    }
    await loadDonations(true)
    const deferredSuffix = res.data.deferred > 0
      ? ', ' + res.data.deferred + ' remaining — sync again to continue'
      : ''
    setStatus(
      'Synced Afdian: +' + res.data.committed + ' new (' + res.data.deduplicated + ' existing' + deferredSuffix + ').',
    )
  })

  el('config-form').addEventListener('submit', async event => {
    event.preventDefault()
    const res = await api('/v1/admin/donations/config', {
      method: 'PATCH',
      body: JSON.stringify({
        minPublicCnyMinor: Number(el('cfg-cny').value),
        minPublicUsdMinor: Number(el('cfg-usd').value),
        moderationEnabled: el('cfg-moderation').checked,
        cryptoCheckoutPaused: el('cfg-crypto-paused').checked,
      }),
    })
    if (!res.ok) {
      setStatus('Failed to save settings: ' + (res.data.error || res.status))
      return
    }
    applyConfig(res.data.config)
    setStatus('Settings saved.')
  })

  el('filters').addEventListener('click', event => {
    const target = event.target
    if (!(target instanceof HTMLButtonElement) || !target.dataset.filter) return
    state.filter = target.dataset.filter
    for (const btn of el('filters').querySelectorAll('button')) {
      btn.classList.toggle('active', btn === target)
    }
    renderList()
  })

  api('/v1/admin/session').then(res => {
    if (res.ok && res.data.authenticated) {
      showLoggedIn(res.data.csrfToken, res.data.config)
    } else {
      showLoggedOut('Sign in to moderate donations.')
    }
  })
})()
`
