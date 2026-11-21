import { message, MessageError, type Message } from './locale'
import { sharedCopy } from './locales'

const requestFailed = message(sharedCopy, 'requestFailed')
const REQUEST_TIMEOUT_MS = 15_000

export class RequestError extends MessageError {
  constructor(readonly status: number, readonly code: string, description: Message) {
    super(description)
  }
}

export type FaultMessage = Message | ((fault: Readonly<Record<string, unknown>>) => Message)

interface RequestOptions extends RequestInit {
  data?: unknown
  messages?: Readonly<Record<string, FaultMessage>>
}

function resolveFaultMessage(
  messages: Readonly<Record<string, FaultMessage>> | undefined,
  code: string,
  fault: unknown,
): Message {
  if (!messages || !Object.hasOwn(messages, code)) return requestFailed
  const entry = messages[code]
  if (typeof entry !== 'function') return entry
  const payload = fault && typeof fault === 'object' ? fault as Readonly<Record<string, unknown>> : {}
  return entry(payload)
}

export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const { data, messages, signal, headers, ...init } = options
  const timeout = AbortSignal.timeout(REQUEST_TIMEOUT_MS)
  const response = await fetch(path, {
    credentials: 'same-origin',
    ...init,
    method: init.method ?? (data === undefined ? 'GET' : 'POST'),
    headers: { ...(data === undefined ? {} : { 'Content-Type': 'application/json' }), ...headers },
    body: data === undefined ? undefined : JSON.stringify(data),
    signal: signal ? AbortSignal.any([signal, timeout]) : timeout,
  })
  if (!response.ok) {
    const fault = await response.json().catch(() => null)
    const code = fault?.error ?? fault?.reason ?? fault?.kind ?? ''
    const description = resolveFaultMessage(messages, code, fault)
    throw new RequestError(response.status, code, description)
  }
  return response.json() as Promise<T>
}

export function messageOf(error: unknown): Message {
  if (error instanceof MessageError) return error.userMessage
  if (error instanceof TypeError || (error instanceof DOMException && error.name === 'TimeoutError')) {
    return message(sharedCopy, 'unreachable')
  }
  return requestFailed
}
