import { FocusPage } from '../../ui/FocusPage'
import { Frame, SessionWait } from '../../ui/layout'
import { useResource } from '../../ui/activity'
import { useCopy } from '../../ui/use-locale'
import { request } from '../../ui/request'
import { deviceCopy, deviceErrors } from '../../ui/locales/device'
import { takeFragment } from '../../ui/location'

const approval = takeFragment('approval') ?? ''
const redeem = (signal: AbortSignal) => request<{ ok: true }>('/api/enrollments/account', { signal, credentials: 'omit', data: { approval }, messages: deviceErrors })
export function CompletionPage() {
  const t = useCopy(deviceCopy)
  const receipt = useResource(redeem)
  return <Frame area={t.title}>{receipt.value
    ? <FocusPage icon="check" title={t.done}><p className="lead" role="status">{t.finish}</p></FocusPage>
    : <SessionWait error={receipt.error} retry={receipt.reload} />}</Frame>
}
