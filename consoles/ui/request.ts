const REQUEST_TIMEOUT_MS = 15_000

export class RequestError extends Error {
  constructor(readonly status: number, readonly code: string, message: string) {
    super(message)
  }
}

interface RequestOptions extends RequestInit {
  data?: unknown
  messages?: Record<string, string>
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
    throw new RequestError(response.status, code, messages?.[code] ?? 'The request failed. Please try again.')
  }
  return response.json() as Promise<T>
}

export function messageOf(error: unknown): string {
  if (error instanceof TypeError || (error instanceof DOMException && error.name === 'TimeoutError')) {
    return 'Could not reach the service. Check your connection and try again.'
  }
  return error instanceof Error ? error.message : 'The request failed. Please try again.'
}
