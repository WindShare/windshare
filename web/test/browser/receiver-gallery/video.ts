// Synthetic frames exercise the production video element without a real share.
export async function syntheticVideo(): Promise<{ url: string; mimeType: string }> {
  const canvas = document.createElement('canvas')
  canvas.width = 640
  canvas.height = 360
  const drawing = canvas.getContext('2d')!
  const stream = canvas.captureStream(30)
  const mimeType = MediaRecorder.isTypeSupported('video/mp4') ? 'video/mp4' : 'video/webm'
  const recorder = new MediaRecorder(stream, { mimeType })
  const chunks: Blob[] = []
  const complete = new Promise<{ url: string; mimeType: string }>((resolve, reject) => {
    recorder.ondataavailable = event => { if (event.data.size > 0) chunks.push(event.data) }
    recorder.onerror = () => reject(new Error('Synthetic video fixture could not be recorded'))
    recorder.onstop = () => resolve({ url: URL.createObjectURL(new Blob(chunks, { type: mimeType })), mimeType })
  })
  recorder.start()
  try {
    for (let frame = 0; frame < 12; frame += 1) {
      drawing.fillStyle = '#dce9df'
      drawing.fillRect(0, 0, canvas.width, canvas.height)
      drawing.fillStyle = '#237d57'
      drawing.fillRect(60 + frame * 4, 110, 180, 140)
      await new Promise<void>(resolve => requestAnimationFrame(() => resolve()))
    }
    recorder.stop()
    return await complete
  } finally {
    for (const track of stream.getTracks()) track.stop()
  }
}
