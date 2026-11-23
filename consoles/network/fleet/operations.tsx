import { useState } from 'react'
import { useAction } from '../../ui/activity'
import { Notice } from '../../ui/layout'
import { Confirm } from '../../ui/forms'
import { RequestError } from '../../ui/request'
import { useCopy } from '../../ui/use-locale'
import { networkCopy } from '../locales'
import type { AuthorityClient } from '../api'
import { servesRelay, type FleetAction, type NodeDetails } from '../model'

type PendingAction = { kind: 'disable' | 'remove'; earlyCutoff: boolean; node: NodeDetails }

function confirmDetails(t: typeof networkCopy.en, pending: PendingAction) {
  if (pending.kind === 'remove') {
    return {
      title: t.confirmRemove,
      confirm: t.removeNode,
      body: pending.earlyCutoff ? t.removeEarlyEffect : t.removeEffect,
    }
  }
  return pending.earlyCutoff
    ? { title: t.confirmCutoff, confirm: t.disable, body: t.cutoffEffect }
    : { title: t.confirmDisable, confirm: t.disable, body: t.disableEffect }
}

function canEscalateDrainPending(
  error: unknown,
  command: FleetAction,
): command is { kind: 'disable' | 'remove'; early_cutoff: false } {
  return error instanceof RequestError && error.code === 'drain_pending' &&
    (command.kind === 'disable' || command.kind === 'remove') && !command.early_cutoff
}

function listingToken(node: Pick<NodeDetails, 'state' | 'version'>): string {
  return `${node.state}:${node.version}`
}

export function FleetControls({ node, api, refresh }: { node: NodeDetails; api: AuthorityClient; refresh: () => Promise<void> }) {
  const t = useCopy(networkCopy)
  const action = useAction()
  const [pending, setPending] = useState<PendingAction | null>(null)
  const [feedbackListing, setFeedbackListing] = useState<string | null>(null)
  const currentListing = listingToken(node)
  const disabled = node.state === 'disabled'
  const canDrain = servesRelay(node) && (node.state === 'canary' || node.state === 'active')
  const details = pending && confirmDetails(t, pending)
  function run(command: FleetAction, observed = node) {
    void action.run(async () => {
      setFeedbackListing(listingToken(observed))
      try {
        await api.manage(observed, command)
      } catch (error) {
        if (canEscalateDrainPending(error, command)) {
          // Escalate a stale-snapshot drain conflict straight into the early-cutoff confirmation
          // without leaving a raw error banner behind the dialog.
          setPending({ kind: command.kind, earlyCutoff: true, node: observed })
          return
        }
        throw error
      }
      if (command.kind === 'remove') location.hash = 'nodes'
      await refresh()
    })
  }
  const next = disabled ? 'remove' : 'disable'
  // Manage preconditions bind to (state, version), so errors expire once a refresh advances either.
  const feedback = feedbackListing === currentListing ? action.feedback : null
  return <section className="panel padded stack"><h2>{t.fleetOperations}</h2>
    <div className="actions">
      {node.state !== 'active' && <button type="button" disabled={action.busy}
        onClick={() => run({ kind: 'activate' })}>{t.activate}</button>}
      {canDrain && <button type="button" className="secondary" disabled={action.busy}
        onClick={() => run({ kind: 'drain' })}>{t.drain}</button>}
      <button type="button" className="secondary danger-text" disabled={action.busy}
        onClick={() => setPending({ kind: next, earlyCutoff: !node.cutoff_ready, node })}>{disabled ? t.removeNode : t.disable}</button>
    </div>
    {pending && details && <Confirm title={details.title} confirm={details.confirm}
      onCancel={() => setPending(null)} onConfirm={() => {
        setPending(null)
        run({ kind: pending.kind, early_cutoff: pending.earlyCutoff }, pending.node)
      }}><p>{details.body}</p></Confirm>}
    <Notice feedback={feedback} />
  </section>
}
