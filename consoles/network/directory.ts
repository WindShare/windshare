import { useEffect, useState } from 'react'
import { encode } from './identity'
import type { Publication } from './model'

const MAX_MIRROR_BYTES = 512 * 1024
const MIRROR_TIMEOUT_MS = 5000
const MAX_TIMER_MS = 2 ** 31 - 1

async function verify(expected: Publication, signal: AbortSignal) {
  const response = await fetch(expected.mirror, {
    signal, mode: 'cors', credentials: 'omit', redirect: 'error', cache: 'no-store',
  })
  if (!response.ok || !response.body || Number(response.headers.get('Content-Length')) > MAX_MIRROR_BYTES) return false
  const reader = response.body.getReader()
  const chunks: Uint8Array<ArrayBuffer>[] = []
  let length = 0
  for (;;) {
    const { value, done } = await reader.read()
    if (done) break
    length += value.byteLength
    if (length > MAX_MIRROR_BYTES) {
      await reader.cancel()
      return false
    }
    chunks.push(value)
  }
  // The deploy-time release verifier binds the list of relay keys to this digest.
  const envelope = JSON.parse(await new Blob(chunks).text())
  const cert = envelope.certificate
  const signed = JSON.stringify([cert.root, cert.payload, cert.signature, envelope.payload, envelope.signature])
  const hash = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(signed))
  return encode(new Uint8Array(hash)) === expected.digest && Date.now() < expected.expires * 1000
}

export function useDirectory(publication: Publication | null) {
  const [checked, setChecked] = useState<{ source: Publication; verified: boolean } | null>(null)
  useEffect(() => {
    if (!publication) return
    const abort = new AbortController()
    let timer: number | undefined
    const markExpired = () => {
      if (!abort.signal.aborted) setChecked({ source: publication, verified: false })
    }
    const armTimer = (remaining: number) => {
      timer = window.setTimeout(() => {
        const left = publication.expires * 1000 - Date.now()
        if (left <= 0) markExpired()
        else armTimer(left)
      }, Math.min(remaining, MAX_TIMER_MS))
    }
    const remaining = publication.expires * 1000 - Date.now()
    if (remaining <= 0) {
      void Promise.resolve().then(markExpired)
    } else {
      armTimer(remaining)
      void verify(publication, AbortSignal.any([abort.signal, AbortSignal.timeout(MIRROR_TIMEOUT_MS)]))
        .catch(() => false).then(verified => {
          if (!abort.signal.aborted) setChecked({ source: publication, verified })
        })
    }
    return () => {
      abort.abort()
      if (timer !== undefined) window.clearTimeout(timer)
    }
  }, [publication])
  if (!publication) return null
  if (checked?.source !== publication) return undefined
  return checked.verified ? publication : null
}
