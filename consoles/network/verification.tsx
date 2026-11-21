import { useAction } from '../ui/activity'
import { DateStamp, Notice } from '../ui/layout'
import { useCopy } from '../ui/use-locale'
import type { AuthorityClient } from './api'
import { networkCopy } from './locales'
import type { RelayNode, TrafficStatus } from './model'

export function Verification({ node, api, refresh }: { node: RelayNode; api: AuthorityClient; refresh: () => Promise<void> }) {
  const t = useCopy(networkCopy)
  const action = useAction()
  const listing = node.listing
  if (!listing || listing.state.kind !== 'staging') return null
  const { spec, state, operation_id } = listing
  const { check } = state
  const passed = check.kind === 'passed'
  const promote = (status: TrafficStatus) => void action.run(async () => {
    await api.promote(spec.node_key, operation_id, status)
    await refresh()
  })
  const status = passed
    ? <>{t.states.passed} · <DateStamp millis={check.at * 1000} /></>
    : <>{t.states[check.kind]}</>
  return <section className="panel padded stack"><h2>{t.verification}</h2>
    <dl className="facts"><dt>{t.status}</dt><dd>{status}</dd></dl>
    {check.kind === 'failed' && <p role="status">{check.reason}</p>}
    <div className="actions">
      {passed && node.admission !== 'disabled' && <>
        <button type="button" disabled={action.busy} onClick={() => promote('active')}>{t.publishActive}</button>
        <button type="button" className="secondary" disabled={action.busy} onClick={() => promote('canary')}>{t.publishCanary}</button>
      </>}
      <button type="button" className="secondary" disabled={action.busy}
        onClick={() => void action.run(async () => {
          await api.cancelDraft(spec.node_key, operation_id)
          await refresh()
          location.hash = 'nodes'
        })}>{t.cancelDraft}</button>
    </div>
    <Notice feedback={action.feedback} />
  </section>
}
