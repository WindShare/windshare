const DAY_MS = 24 * 60 * 60 * 1000;
const GRACE_MS = 7 * DAY_MS;
const SKEW_MS = 5 * 60 * 1000;
const MAX_GRANT_LIFETIME_MS = 180 * DAY_MS;
const MAX_PAYLOAD_LIFETIME_MS = 90 * DAY_MS;
export const MAX_BYTES = 512 * 1024;
const utf8 = new TextDecoder('utf-8', { fatal: true });
const utf8Encoder = new TextEncoder();

function decode(encoded) {
  if (typeof encoded !== 'string') throw new TypeError('Invalid base64url field');
  return Uint8Array.from(atob(encoded.replaceAll('-', '+').replaceAll('_', '/')), c => c.charCodeAt(0));
}

function parseTimestamp(value) {
  return typeof value === 'string' ? Date.parse(value) : Number.NaN;
}

// Every UTF-16 code unit encodes to at least one UTF-8 byte, so strings longer
// than MAX_BYTES can be rejected without allocating an encoded copy.
export function exceedsWireLimit(raw) {
  return raw.length > MAX_BYTES || utf8Encoder.encode(raw).byteLength > MAX_BYTES;
}

// Both sources are controlled by the signed-release build or the authority publisher.
// Receivers independently verify signatures before trusting an advertised relay.
export async function directoryRevision(raw) {
  if (typeof raw !== 'string' || exceedsWireLimit(raw)) throw new Error('Directory exceeds wire limit');
  const envelope = JSON.parse(raw);
  const bytes = decode(envelope?.payload);
  const payload = JSON.parse(utf8.decode(bytes));
  const grant = JSON.parse(utf8.decode(decode(envelope?.certificate?.payload)));
  const { format, network, generation, serial } = payload ?? {};
  const grantNotBefore = parseTimestamp(grant?.not_before);
  const grantExpiresAt = parseTimestamp(grant?.expires_at);
  const issuedAt = parseTimestamp(payload?.issued_at);
  const expiresAt = parseTimestamp(payload?.expires_at);
  const validGrant = grant?.format === 1
    && typeof grant.network === 'string' && grant.network.length > 0
    && Number.isSafeInteger(grant.generation) && grant.generation > 0
    && grantExpiresAt > grantNotBefore
    && grantExpiresAt - grantNotBefore <= MAX_GRANT_LIFETIME_MS;
  const validPayload = format === 1
    && network === grant?.network
    && generation === grant?.generation
    && Number.isSafeInteger(serial) && serial > 0
    && issuedAt >= grantNotBefore
    && expiresAt > issuedAt
    && expiresAt - issuedAt <= MAX_PAYLOAD_LIFETIME_MS;
  const start = Math.max(grantNotBefore, issuedAt) - SKEW_MS;
  const until = Math.min(grantExpiresAt, expiresAt + GRACE_MS);
  if (!validGrant || !validPayload || !(until > start)) throw new Error('Invalid directory revision');
  const hash = await crypto.subtle.digest('SHA-256', bytes);
  const digest = btoa(String.fromCharCode(...new Uint8Array(hash))).replaceAll('+', '-').replaceAll('/', '_').replace(/=+$/, '');
  return { network, generation, serial, digest, start, until };
}

export function compare(a, b) {
  return a.generation - b.generation || a.serial - b.serial;
}
