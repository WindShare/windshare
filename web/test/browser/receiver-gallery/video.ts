// The committed 10 fps clip has distinct keyframes at 0, 0.1, and 0.2 seconds.
// MediaRecorder timestamps and dropped captures depend on runner scheduling,
// so runtime recording cannot guarantee different pixels at a fixed seek time.
export async function syntheticVideo() {
  const response = await fetch(new URL('./video-frames.webm', import.meta.url))
  if (!response.ok) throw new Error('Synthetic video fixture could not be loaded')
  const blob = await response.blob()
  return { url: URL.createObjectURL(blob), mimeType: 'video/webm',
    width: 640, height: 360, durationSeconds: 0.3 }
}
