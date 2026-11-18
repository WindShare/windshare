const $ = (id) => document.getElementById(id);
let csrf = '';
let snapshot;
const initialization = new URLSearchParams(location.hash.slice(1)).get('initialize');
if (initialization) {
  $('login-title').textContent = 'Initialize administrator';
  $('login-submit').textContent = 'Create administrator';
  $('password').autocomplete = 'new-password';
  $('password').minLength = 12;
  $('login').action = '/v1/admin/initialize';
}
const encode = (bytes) => btoa(String.fromCharCode(...bytes)).replaceAll('+', '-').replaceAll('/', '_').replaceAll('=', '');
function decode(text, length) {
  const value = Uint8Array.from(atob(text.replaceAll('-', '+').replaceAll('_', '/')), (c) => c.charCodeAt(0));
  if (value.length !== length || encode(value) !== text) throw new Error('Enter a complete, valid ID.');
  return [...value];
}
function status(message, error = false) {
  $('status').textContent = message;
  $('status').classList.toggle('error', error);
}
async function api(path, body, method = 'POST') {
  const response = await fetch(path, { method, credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
    body: body === undefined ? undefined : JSON.stringify(body), signal: AbortSignal.timeout(15000) });
  const value = await response.json().catch(() => ({}));
  if (!response.ok) {
    if (value.reason === 'administrator_session') {
      $('console').hidden = true; $('login').hidden = false; $('logout').hidden = true;
    }
    throw new Error(value.reason ? value.reason.replaceAll('_', ' ') : 'Request failed. Please try again.');
  }
  return value;
}
function submit(id, action) {
  $(id).addEventListener('submit', async (event) => {
    event.preventDefault();
    const form = event.currentTarget;
    const buttons = [...form.querySelectorAll('button[type=submit]')];
    buttons.forEach((button) => { button.disabled = true; });
    try { await action(new FormData(form, event.submitter)); status('Saved.'); }
    catch (error) { status(error.message, true); }
    finally { buttons.forEach((button) => { button.disabled = false; }); }
  });
}
function button(label, action) {
  const button = document.createElement('button');
  button.type = 'button'; button.textContent = label; button.className = 'secondary';
  button.addEventListener('click', async () => {
    button.disabled = true;
    try { await action(); await load(); }
    catch (error) { status(error.message, true); }
    finally { button.disabled = false; }
  });
  return button;
}
function text(tag, content) {
  const node = document.createElement(tag); node.textContent = content; return node;
}
async function load() {
  snapshot = await api('/v1/admin/console', undefined, 'GET');
  csrf = snapshot.csrf;
  $('login').hidden = true; $('console').hidden = false; $('logout').hidden = false;
  $('account-count').textContent = new Intl.NumberFormat().format(snapshot.account_count) + ' accounts';
  $('reviews').textContent = snapshot.reviews.length ? 'Account milestone reached. Review registration policy and node capacity.' : '';
  const settings = snapshot.settings;
  $(settings.registration).checked = true;
  $('email').disabled = !snapshot.email_configured;
  $('pow-bits').value = settings.pow_bits;
  $('daily-bytes').value = settings.daily_bytes / 1024 ** 3;
  for (const scope of ['source', 'global']) {
    $(scope + '-burst').value = settings[scope + '_rate'].burst;
    $(scope + '-refill').value = settings[scope + '_rate'].refill_seconds;
  }
  $('nodes').replaceChildren();
  const publication = verifyPublished(snapshot.directory);
  for (const node of snapshot.nodes) {
    const row = document.createElement('div'); row.className = 'node';
    row.append(text('strong', node.region + ' · ' + node.zone), text('code', encode(node.key)),
      text('span', node.anonymous ? 'Anonymous allowed' : 'Account required'),
      button(node.anonymous ? 'Require account' : 'Allow anonymous', () => api('/v1/admin/nodes', { ...node, anonymous: !node.anonymous })),
      button(node.enabled ? 'Disable node' : 'Enable node', () => api('/v1/admin/nodes', { ...node, enabled: !node.enabled })));
    const listed = text('small', snapshot.directory ? 'Checking published directory…' : 'Directory unverified');
    row.append(text('small', 'Enrolled'), listed);
    void publication.then(directory => {
      listed.textContent = directory
        ? (directory.keys.includes(encode(node.key)) ? 'Listed' : 'Absent') + ' in directory ' + directory.generation + '.' + directory.serial
        : 'Directory unverified';
    });
    const latest = snapshot.load.find((entry) => entry.node === encode(node.key));
    const load = latest?.load;
    row.append(text('small', latest?.reported_at ? 'Last report ' + new Date(latest.reported_at * 1000).toLocaleString() : 'Awaiting first report'));
    if (latest) row.append(text('small', latest
      ? `${load.active_sessions} sessions · ${(load.forwarded_bytes / 1024 ** 3).toFixed(2)} GiB relayed · ${load.admission_refusals} refusals`
      : 'Awaiting first node report'));
    $('nodes').append(row);
  }
  if (!snapshot.nodes.length) $('nodes').append(text('p', 'No enrolled nodes.'));
  $('reports').replaceChildren();
  for (const report of snapshot.reports) {
    const row = document.createElement('li');
    row.append(text('code', encode(report.share)), text('p', report.reason),
      button('Review this share', async () => {
        $('block-share').checked = true; $('block-id').value = encode(report.share);
        $('block-id').focus(); $('blocks').scrollIntoView({ block: 'center' });
      }));
    $('reports').append(row);
  }
  if (!snapshot.reports.length) $('reports').append(text('li', 'No reports.'));
}
async function verifyPublished(expected) {
  if (!expected || Date.now() >= expected.expires * 1000) return null;
  try {
    const response = await fetch(expected.mirror, { mode: 'cors', credentials: 'omit', redirect: 'error',
      signal: AbortSignal.timeout(5000), cache: 'no-store' });
    const maximum = 512 * 1024;
    if (!response.ok || !response.body || Number(response.headers.get('Content-Length')) > maximum) return null;
    const reader = response.body.getReader(), chunks = [];
    let length = 0;
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      length += value.length;
      if (length > maximum) { await reader.cancel(); return null; }
      chunks.push(value);
    }
    const e = JSON.parse(await new Blob(chunks).text()), c = e.certificate;
    const signed = JSON.stringify([c.root, c.payload, c.signature, e.payload, e.signature]);
    const hash = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(signed));
    return encode(new Uint8Array(hash)) === expected.digest ? expected : null;
  } catch { return null; }
}
submit('login', async (data) => {
  const body = { username: data.get('username'), password: data.get('password') };
  const result = await api(initialization ? '/v1/admin/initialize' : '/v1/admin/login', initialization ? { ...body, token: initialization } : body);
  csrf = result.csrf; history.replaceState(null, '', '/console'); $('password').value = '';
  await load(); $('title').focus();
});
$('show-password').addEventListener('click', () => {
  const visible = $('password').type === 'password';
  $('password').type = visible ? 'text' : 'password';
  $('show-password').setAttribute('aria-pressed', String(visible));
  $('show-password').textContent = visible ? 'Hide password' : 'Show password';
});
submit('policy', async (data) => {
  await api('/v1/admin/settings', { registration: data.get('registration'), pow_bits: Number(data.get('pow_bits')),
    daily_bytes: Math.round(Number(data.get('daily_gib')) * 1024 ** 3),
    source_rate: { burst: Number(data.get('source_burst')), refill_seconds: Number(data.get('source_refill')) },
    global_rate: { burst: Number(data.get('global_burst')), refill_seconds: Number(data.get('global_refill')) } }, 'PUT');
});
submit('enroll', async (data) => {
  await api('/v1/admin/nodes', { key: decode(String(data.get('key')), 32), zone: data.get('zone'), region: data.get('region'),
    anonymous: data.get('zone') === 'global', enabled: true });
  await load(); $('enroll').reset();
});
submit('blocks', async (data) => {
  await api('/v1/admin/blocks', { kind: data.get('kind'), id: decode(String(data.get('id')), data.get('kind') === 'share' ? 12 : 16),
    blocked: data.get('action') === 'block' });
});
$('logout').addEventListener('click', async () => {
  try { await api('/v1/admin/logout', {}); location.assign('/console'); }
  catch (error) { status(error.message, true); }
});
if (!initialization) load().catch((error) => {
  if (!error.message.includes('administrator session')) status(error.message, true);
});
