import { isIP } from 'node:net'

const MAXIMUM_ICE_URI_BYTES = 2_048

export function validNetworkMatrixIceUri(
  value: unknown,
  schemes: readonly string[],
): value is string {
  if (
    typeof value !== 'string' || value === '' ||
    Buffer.byteLength(value, 'utf8') > MAXIMUM_ICE_URI_BYTES ||
    hasForbiddenIceUriCharacter(value)
  ) return false
  const parsed = /^(stun|turn|turns):(\[[0-9a-f:.]+\]|[a-z0-9.-]+)(?::([1-9]\d{0,4}))?(?:\?transport=(udp|tcp))?$/u
    .exec(value)
  if (parsed === null || !schemes.includes(`${parsed[1]}:`)) return false
  const host = parsed[2]
  const port = parsed[3]
  const transport = parsed[4]
  if (host === undefined || !validIceHost(host)) return false
  if (port !== undefined && Number(port) > 65_535) return false
  if (parsed[1] === 'stun' && transport !== undefined) return false
  return parsed[1] !== 'turns' || transport === undefined || transport === 'tcp'
}

function hasForbiddenIceUriCharacter(value: string): boolean {
  return [...value].some((character) => {
    const codePoint = character.codePointAt(0) as number
    return character.trim() === '' || codePoint <= 31 || codePoint === 127
  })
}

function validIceHost(value: string): boolean {
  if (value.startsWith('[') && value.endsWith(']')) {
    if (isIP(value.slice(1, -1)) !== 6) return false
    try {
      return new URL(`http://${value}/`).hostname === value
    } catch {
      return false
    }
  }
  if (isIP(value) === 4) return true
  if (/^[\d.]+$/u.test(value)) return false
  if (value.length > 253 || !value.includes('.')) return false
  return value.split('.').every((label) =>
    label.length >= 1 && label.length <= 63 &&
    /^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/u.test(label))
}
