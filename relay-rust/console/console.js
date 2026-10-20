const element = id => document.getElementById(id);
const messages = {
  login_required: 'Sign in to continue.', invalid_password: 'The password was not accepted.',
  slow_down: 'Please wait before trying again.', expired_token: 'This request expired. Start setup again on the device.',
  access_denied: 'This request is no longer available.', temporarily_unavailable: 'The relay is temporarily unavailable. Please try again.',
  credential_capacity: 'This relay has reached its credential limit.', invalid_name: 'Enter a short, descriptive name.',
};
async function api(path, body) {
  const response = await fetch('/api/' + path, {
    method: body === undefined ? 'GET' : 'POST', credentials: 'same-origin',
    headers: body === undefined ? {} : { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const value = await response.json();
  if (!response.ok) {
    if (response.status === 401) signedIn(false);
    throw new Error(messages[value.error] || 'The request could not be completed.');
  }
  return value;
}
function status(message = '', error = false) {
  element('status').textContent = message;
  element('status').classList.toggle('error', error);
}
function signedIn(value) {
  element('login-panel').hidden = value;
  element('console').hidden = !value;
  element('logout').hidden = !value;
  if (!value) {
    element('created').hidden = true;
    element('configuration').textContent = '';
  }
}
async function action(button, operation) {
  button.disabled = true; status();
  try { await operation(); } catch (error) { status(error.message, true); }
  finally { button.disabled = false; }
}
function node(tag, text, className) {
  const item = document.createElement(tag);
  if (text !== undefined) item.textContent = text;
  if (className) item.className = className;
  return item;
}
const date = value => value ? new Date(value).toLocaleString() : 'Not used yet';
let pending = [];
function renderEnrollments() {
  const root = element('enrollments'); root.replaceChildren();
  const code = element('code').value.trim().toUpperCase();
  const matches = pending.filter(request => !code || request.user_code === code);
  if (!matches.length) {
    root.append(node('p', code ? 'No pending request matches this code.' : 'No devices are waiting for approval.'));
    return;
  }
  for (const request of matches) {
    const row = node('div', undefined, 'row');
    const info = node('div'); info.append(node('strong', request.device_name), node('div', request.user_code, 'meta'));
    const approve = node('button', 'Approve device');
    approve.disabled = code !== request.user_code;
    approve.addEventListener('click', () => action(approve, async () => {
      await api('enrollments/approve', { enrollment_id: request.enrollment_id, user_code: code });
      await refresh(); status('Device approved. Setup will finish automatically on that device.');
    }));
    row.append(info, approve); root.append(row);
  }
}
function renderCredentials(credentials) {
  const root = element('credentials'); root.replaceChildren();
  if (!credentials.length) root.append(node('p', 'Approved devices will appear here.'));
  for (const credential of credentials) {
    const row = node('div', undefined, 'row');
    const info = node('div');
    info.append(node('strong', credential.name), node('div', 'Created ' + date(credential.created_at), 'meta'),
      node('div', 'Last use: ' + date(credential.last_used_at), 'meta'));
    if (credential.revoked) info.append(node('span', 'Revoked', 'badge revoked'));
    const actions = node('div', undefined, 'actions');
    const label = node('label'); const terminate = node('input'); terminate.type = 'checkbox';
    label.append(terminate, document.createTextNode('Also stop existing shares'));
    const revoke = node('button', credential.revoked ? 'Stop remaining shares' : 'Revoke', 'danger');
    revoke.addEventListener('click', () => action(revoke, async () => {
      await api('credentials/revoke', { credential_id: credential.credential_id, terminate_shares: credential.revoked || terminate.checked });
      await refresh(); status(credential.revoked || terminate.checked ? 'Access revoked. Existing relay shares are being stopped.' : 'Access revoked for new shares.');
    }));
    if (!credential.revoked) actions.append(label);
    actions.append(revoke); row.append(info, actions); root.append(row);
  }
}
async function refresh() {
  const [enrollments, credentials] = await Promise.all([api('enrollments'), api('credentials')]);
  pending = enrollments.enrollments; renderEnrollments(); renderCredentials(credentials.credentials);
}
element('code').value = new URL(location.href).searchParams.get('code') || '';
element('code').addEventListener('input', renderEnrollments);
element('login').addEventListener('submit', event => {
  event.preventDefault();
  action(event.submitter, async () => {
    await api('login', { password: element('password').value });
    element('password').value = ''; signedIn(true); await refresh();
  });
});
element('logout').addEventListener('click', event => action(event.target, async () => { await api('logout', {}); signedIn(false); }));
element('refresh').addEventListener('click', event => action(event.target, refresh));
element('create').addEventListener('submit', event => {
  event.preventDefault();
  action(event.submitter, async () => {
    const result = await api('credentials', { name: element('name').value });
    element('configuration').textContent = JSON.stringify(result.configuration, null, 2);
    element('created').hidden = false; element('name').value = ''; await refresh();
  });
});
element('copy').addEventListener('click', event => action(event.target, async () => {
  await navigator.clipboard.writeText(element('configuration').textContent); status('Configuration copied.');
}));
try { await api('session'); signedIn(true); await refresh(); } catch { signedIn(false); }
