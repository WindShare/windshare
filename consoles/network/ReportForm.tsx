import { useState } from 'react'
import { ValidatedForm } from '../ui/ValidatedForm'
import { useAction } from '../ui/activity'
import { Frame, Notice } from '../ui/layout'
import { EntryPage } from '../ui/FocusPage'
import { Icon } from '../ui/Icon'
import { message, MessageError } from '../ui/locale'
import { useCopy } from '../ui/use-locale'
import { reportCopy } from './locales/report'
import { request } from '../ui/request'
import { requireIdentity } from './identity'

const MAX_REASON_BYTES = 2000

export function ReportForm({ initialShare }: { initialShare: string }) {
  const t = useCopy(reportCopy)
  const [sent, setSent] = useState(false)
  const action = useAction()
  return <Frame area={t.title}><EntryPage icon={sent ? 'check' : 'report'} label={t.network} title={sent ? t.received : t.title}>
    {sent ? <p className="lead" role="status">{t.review}</p>
      : <ValidatedForm className="stack" onSubmit={event => {
        event.preventDefault()
        const data = new FormData(event.currentTarget)
        void action.run(async () => {
          const share = requireIdentity(String(data.get('share')), 'share')
          const reason = String(data.get('reason')).trim()
          if (!reason) throw new MessageError(message(reportCopy, 'missingReason'))
          if (new TextEncoder().encode(reason).byteLength > MAX_REASON_BYTES) throw new MessageError(message(reportCopy, 'longReason'))
          await request('/v1/reports', { credentials: 'omit', data: { share, reason },
            messages: { rate_limited: message(reportCopy, 'rateLimited') } })
          setSent(true)
        })
      }}>
        <div className="field"><label htmlFor="share">{t.publicShareId}</label>
          <input id="share" name="share" defaultValue={initialShare} autoCapitalize="none" spellCheck={false} required /></div>
        <div className="field"><label htmlFor="reason">{t.reason}</label>
          <textarea id="reason" name="reason" rows={5} maxLength={MAX_REASON_BYTES} required /></div>
        <Notice feedback={action.feedback} /><button disabled={action.busy}>{action.busy ? t.submitting : t.submit}<Icon name={action.busy ? 'loader' : 'arrow'} size={16} /></button>
      </ValidatedForm>}
  </EntryPage></Frame>
}
