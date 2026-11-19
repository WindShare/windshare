export async function runAbortableConnection<T>(
  signal: AbortSignal,
  connect: () => Promise<T>,
  close: (value: T) => Promise<void>,
): Promise<T> {
  signal.throwIfAborted()
  let interrupt!: () => void
  const interrupted = new Promise<never>((_resolve, reject) => {
    interrupt = () => reject(signal.reason)
    signal.addEventListener('abort', interrupt, { once: true })
  })
  try {
    const work = (async () => {
      const value = await connect()
      // The caller may already have started a new attempt when a loser settles.
      if (signal.aborted) {
        await close(value).catch(() => undefined)
        throw signal.reason
      }
      return value
    })()
    return await Promise.race([work, interrupted])
  } finally {
    signal.removeEventListener('abort', interrupt)
  }
}
