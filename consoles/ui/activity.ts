import { useCallback, useEffect, useRef, useState } from 'react'
import type { Message } from './locale'
import { messageOf } from './request'

type Read<T> = (signal: AbortSignal) => Promise<T>

export function useResource<T>(read: Read<T>) {
  const active = useRef<AbortController | null>(null)
  const failed = useRef<{ read: Read<T>; merge?: (previous: T, next: T) => T } | null>(null)
  const [value, setValue] = useState<T>()
  const [error, setError] = useState<Message | null>(null)
  const [busy, setBusy] = useState(true)
  const start = useCallback((readValue: Read<T>, merge?: (previous: T, next: T) => T): Promise<void> => {
    active.current?.abort()
    failed.current = null
    const controller = new AbortController()
    active.current = controller
    return readValue(controller.signal).then(result => {
      if (!controller.signal.aborted) {
        setValue(previous => previous === undefined || !merge ? result : merge(previous, result))
        setError(null)
      }
    }).catch(reason => {
      if (!controller.signal.aborted) {
        failed.current = { read: readValue, merge }
        setError(messageOf(reason))
      }
    }).finally(() => {
      if (!controller.signal.aborted) setBusy(false)
    })
  }, [])
  function run(operation: () => Promise<void>): Promise<void> {
    setBusy(true)
    setError(null)
    return operation()
  }
  const reload = useCallback(() => run(() => start(read)), [read, start])
  const retry = () => run(() => start(failed.current?.read ?? read, failed.current?.merge))
  const extend = (readMore: Read<T>, merge: (previous: T, next: T) => T) =>
    run(() => start(readMore, merge))
  useEffect(() => {
    void start(read)
    return () => active.current?.abort()
  }, [read, start])

  // A read started before a committed write must never overwrite the receipt.
  function update(merge: (previous: T) => T) {
    active.current?.abort()
    failed.current = null
    setValue(previous => previous === undefined ? previous : merge(previous))
    setBusy(false)
    setError(null)
  }
  return { value, busy, error, reload, retry, extend, update }
}

export interface Feedback {
  kind: 'success' | 'warning' | 'error'
  message: Message
}

export function useAction() {
  const [busy, setBusy] = useState(false)
  const [feedback, setFeedback] = useState<Feedback | null>(null)
  const running = useRef(false)
  async function run(operation: () => Promise<Message | void>) {
    if (running.current) return
    running.current = true
    setBusy(true)
    setFeedback(null)
    try {
      const message = await operation()
      if (message) setFeedback({ kind: 'success', message })
    } catch (error) {
      setFeedback({ kind: 'error', message: messageOf(error) })
    } finally {
      running.current = false
      setBusy(false)
    }
  }
  return { busy, feedback, run }
}
