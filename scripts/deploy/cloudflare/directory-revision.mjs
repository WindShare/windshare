const GRACE_MS = 7 * 24 * 60 * 60 * 1000;
const SKEW_MS = 5 * 60 * 1000;
const MAX_BYTES = 512 * 1024;
const utf8 = new TextDecoder('utf-8', { fatal: true });
function decode(encoded) {
  return Uint8Array.from(atob(encoded.replaceAll('-', '+').replaceAll('_', '/')), c => c.charCodeAt(0));
}

// Both sources are controlled by the signed-release build or the authority publisher.
// Receivers independently verify signatures before trusting an advertised relay.
export async function directoryRevision(raw) {
  if (new TextEncoder().encode(raw).byteLength > MAX_BYTES) throw new Error('Directory exceeds wire limit');
  const envelope = JSON.parse(raw);
  const bytes = decode(envelope.payload);
  const payload = JSON.parse(utf8.decode(bytes));
  const grant = JSON.parse(utf8.decode(decode(envelope.certificate.payload)));
  const { generation, serial } = payload;
  const start = Math.max(Date.parse(grant.not_before), Date.parse(payload.issued_at)) - SKEW_MS;
  const until = Math.min(Date.parse(grant.expires_at), Date.parse(payload.expires_at) + GRACE_MS);
  if (![generation, serial].every(n => Number.isSafeInteger(n) && n > 0) || !(until > start)) throw new Error('Invalid directory revision');
  const hash = await crypto.subtle.digest('SHA-256', bytes);
  const digest = btoa(String.fromCharCode(...new Uint8Array(hash))).replaceAll('+', '-').replaceAll('/', '_').replace(/=+$/, '');
  return { generation, serial, digest, start, until };
}

export function compare(a, b) {
  return a.generation - b.generation || a.serial - b.serial;
}
