const form = document.getElementById('report');
const share = document.getElementById('share');
share.value = new URLSearchParams(location.hash.slice(1)).get('share') ?? '';
history.replaceState(null, '', '/report');
form.addEventListener('submit', async (event) => {
  event.preventDefault();
  const button = form.querySelector('button');
  const status = document.getElementById('status');
  button.disabled = true;
  try {
    const id = Uint8Array.from(atob(share.value.replaceAll('-', '+').replaceAll('_', '/')), (c) => c.charCodeAt(0));
    if (id.length !== 12) throw new Error('Enter a valid Share ID.');
    const response = await fetch('/v1/reports', { method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ share: [...id], reason: document.getElementById('reason').value }),
      signal: AbortSignal.timeout(15000) });
    if (!response.ok) throw new Error(response.status === 429 ? 'Too many reports. Please try again later.' : 'Could not send report. Please try again.');
    status.textContent = 'Report received for manual review.'; form.hidden = true;
  } catch (error) { status.textContent = error.message; }
  finally { button.disabled = false; }
});
