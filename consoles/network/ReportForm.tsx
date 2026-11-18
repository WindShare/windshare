import { useState } from 'react'
import { useAction } from '../ui/activity'
import { FocusPage, Frame, Notice } from '../ui/layout'
import { request } from '../ui/request'
import { decode, SHARE_BYTES } from './identity'

const MAX_REASON_BYTES = 2000

export function ReportForm({ initialShare }: { initialShare: string }) {
  const [sent, setSent] = useState(false)
  const action = useAction()
  return <Frame area="Report a share"><FocusPage label="Official relay network" title={sent ? 'Report received' : 'Report a share'}>
    {sent ? <p className="lead" role="status">A network administrator will review your report.</p>
      : <form className="stack" onSubmit={event => {
        event.preventDefault()
        const data = new FormData(event.currentTarget)
        void action.run(async () => {
          const share = decode(String(data.get('share')), SHARE_BYTES, 'share ID')
          const reason = String(data.get('reason')).trim()
          if (!reason) throw new Error('Enter a reason for your report.')
          if (new TextEncoder().encode(reason).byteLength > MAX_REASON_BYTES) throw new Error('Shorten the reason to 2,000 UTF-8 bytes or fewer.')
          await request('/v1/reports', { credentials: 'omit', data: { share, reason },
            messages: { rate_limited: 'Too many reports. Please try again later.' } })
          setSent(true)
        })
      }}>
        <div className="field"><label htmlFor="share">Public share ID</label>
          <input id="share" name="share" defaultValue={initialShare} autoCapitalize="none" spellCheck={false} required /></div>
        <div className="field"><label htmlFor="reason">Reason for reporting</label>
          <textarea id="reason" name="reason" rows={5} maxLength={MAX_REASON_BYTES} required /></div>
        <Notice feedback={action.feedback} /><button disabled={action.busy}>{action.busy ? 'Submitting…' : 'Submit report'}</button>
      </form>}
  </FocusPage></Frame>
}
