import { useEffect, useState } from 'react'
import { useAction, useResource } from '../ui/activity'
import { FocusPage, Notice, ReadError } from '../ui/layout'
import { RequestError } from '../ui/request'
import type { RelayClient } from './api'

export function CodeEntry() {
  return <section className="panel pairing-entry" aria-labelledby="pair-heading">
    <div><h2 id="pair-heading">Pair a device</h2><p>Enter the code shown in the CLI or desktop app.</p></div>
    <form action="/" method="get" className="inline-form">
      <label className="sr-only" htmlFor="pair-code">Device code</label>
      <input id="pair-code" name="code" className="code-input" placeholder="XXXX-XXXX" maxLength={9}
        autoComplete="off" autoCapitalize="characters" spellCheck={false} required />
      <button>Review device</button>
    </form>
  </section>
}

export function Pairing({ api, code }: { api: RelayClient; code: string }) {
  const requests = useResource(api.enrollments)
  const device = requests.value?.find(entry => entry.user_code === code)
  const [approved, setApproved] = useState(false)
  const [expired, setExpired] = useState(false)
  const action = useAction()
  useEffect(() => {
    if (!device) return
    const timer = window.setTimeout(() => setExpired(true), Math.max(0, device.expires_at - Date.now()))
    return () => window.clearTimeout(timer)
  }, [device])
  if (approved) return <FocusPage label="Device pairing" title="Device approved">
    <p className="lead" role="status">Return to your client. It will collect and save the credential automatically.</p>
    <a href="/">Back to node access</a>
  </FocusPage>
  if (expired && !action.busy) return <FocusPage label="Device pairing" title="Pairing request expired">
    <p className="lead">Start pairing again on the device to get a new code.</p><a href="/">Back to node access</a>
  </FocusPage>
  if (!device) return <FocusPage label="Device pairing" title={requests.value ? 'No pending request' : 'Looking up device'}>
    {requests.value && <p className="lead">Check the device code or return to your client to see its progress.</p>}
    <ReadError error={requests.error} retry={requests.reload} />
    {requests.value && !requests.error && <button className="secondary" disabled={requests.busy} onClick={requests.reload}>Check again</button>}
    <div className="foot-link"><a href="/">Enter a different code</a></div>
  </FocusPage>
  return <FocusPage label="Device pairing" title={device.device_name}>
    <p className="lead">Match this code with the one on your device.</p>
    <div className="pair-code"><span>Device code</span><strong>{device.user_code}</strong></div>
    <Notice feedback={action.feedback} />
    <button className="wide" disabled={action.busy} onClick={() => void action.run(async () => {
      try {
        await api.approve(device)
        setApproved(true)
      } catch (error) {
        if (error instanceof RequestError && error.code === 'expired_token') setExpired(true)
        else throw error
      }
    })}>{action.busy ? 'Approving…' : 'Approve device'}</button>
  </FocusPage>
}
