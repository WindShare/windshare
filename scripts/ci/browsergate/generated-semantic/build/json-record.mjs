export function encodeCanonicalJsonLine(value) {
  return `${JSON.stringify(value)}\n`
}

export function parseCanonicalJsonLine(encoded, label) {
  const text = decodeUtf8(encoded, label)
  if (text.length < 3 || text.endsWith('\n') === false || text.indexOf('\n') !== text.length - 1) {
    throw new Error(`${label} must contain exactly one newline-terminated JSON record`)
  }
  if (text.includes('\r')) throw new Error(`${label} must use UTF-8 JSON with an LF terminator`)
  const line = text.slice(0, -1)
  let value
  try {
    value = JSON.parse(line)
  } catch (cause) {
    throw new Error(`${label} is malformed JSON`, { cause })
  }
  if (JSON.stringify(value) !== line) throw new Error(`${label} is not canonical JSON`)
  return value
}

export function encodeCanonicalJsonValue(value) {
  return JSON.stringify(value)
}

export function parseCanonicalJsonValue(encoded, label) {
  if (typeof encoded !== 'string' || encoded.length === 0 || encoded.includes('\n') || encoded.includes('\r')) {
    throw new Error(`${label} must be one canonical JSON value`)
  }
  let value
  try {
    value = JSON.parse(encoded)
  } catch (cause) {
    throw new Error(`${label} is malformed JSON`, { cause })
  }
  if (JSON.stringify(value) !== encoded) throw new Error(`${label} is not canonical JSON`)
  return value
}

function decodeUtf8(value, label) {
  if (typeof value === 'string') return value
  if (!(value instanceof Uint8Array)) throw new TypeError(`${label} must be text or bytes`)
  try {
    return new TextDecoder('utf-8', { fatal: true }).decode(value)
  } catch (cause) {
    throw new Error(`${label} is not valid UTF-8`, { cause })
  }
}
