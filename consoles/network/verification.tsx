import { useAction } from '../ui/activity'
import { DateStamp, Notice } from '../ui/layout'
import { useCopy } from '../ui/use-locale'
import type { AuthorityClient } from './api'
import { networkCopy } from './locales'
import type { RelayNode, TrafficStatus } from './model'

export function Verification({ node, api, refresh }: { node: RelayNode; api: AuthorityClient; refresh: () => Promise<void> }) {
  const t = useCopy(networkCopy)
  const action = useAction()
  if (!node.listing || node.listing.state.kind === 'disabled') return null
  const { spec, state, operation_id } = node.listing
  const passed = state.kind === 'staging' && state.check.kind === 'passed'
  const promotable = passed || state.kind === 'listed' && state.status === 'canary'
  const promote = (status: TrafficStatus) => void action.run(async () => {
    await api.promote(spec.node_key, operation_id, status)
    await refresh()
  })
  let status = state.kind === 'listed' ? <>{t.states[state.status]}</> : <>{t.states[state.check.kind]}</>
  if (state.kind === 'staging' && state.check.kind === 'passed') {
    status = <>{t.states.passed} · <DateStamp millis={state.check.at * 1000} /></>
  }
  return <section className="panel padded stack"><h2>{t.verification}</h2>
    <dl className="facts"><dt>{t.endpoint}</dt><dd>{spec.relay}</dd><dt>{t.provider}</dt><dd>{spec.provider}</dd>
      <dt>{t.services}</dt><dd>{spec.roles.map(role => role === 'stun' ? 'STUN' : t.publicRelay).join(', ')}</dd>
      <dt>{t.capacity}</dt><dd>{spec.capacity_mbps}</dd><dt>{t.status}</dt><dd>{status}</dd></dl>
    {state.kind === 'staging' && state.check.kind === 'failed' && <p role="status">{state.check.reason}</p>}
    {promotable && node.admission !== 'disabled' && <div className="actions">
      {passed && <button type="button" className="secondary" disabled={action.busy} onClick={() => promote('canary')}>{t.publishCanary}</button>}
      <button type="button" disabled={action.busy} onClick={() => promote('active')}>{t.publishActive}</button>
    </div>}
    {state.kind === 'staging' && <div><button type="button" className="secondary" disabled={action.busy}
      onClick={() => void action.run(async () => {
        await api.cancelDraft(spec.node_key, operation_id)
        await refresh()
        location.hash = 'nodes'
      })}>{t.cancelDraft}</button></div>}
    <Notice feedback={action.feedback} />
  </section>
}
