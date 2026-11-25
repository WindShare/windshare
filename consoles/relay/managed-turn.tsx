import { useAction, useResource } from '../ui/activity'
import { Notice, ReadError } from '../ui/layout'
import { useCopy } from '../ui/use-locale'
import { relayCopy } from './locales'
import type { RelayClient } from './api'

const CLOUDFLARE_BILLING_DASHBOARD_URL = 'https://dash.cloudflare.com/?to=/:account/billing'

export function ManagedTURN({ api }: { api: RelayClient }) {
  const t = useCopy(relayCopy)
  const status = useResource(api.turnStatus)
  const action = useAction()
  return (
    <section className="card managed-turn">
      <label>
        <input
          type="checkbox"
          role="switch"
          aria-describedby="turn-quota"
          checked={status.value?.enabled ?? false}
          disabled={action.busy || status.busy || status.value === undefined}
          onChange={(event) => {
            const enabled = event.currentTarget.checked
            void action.run(async () => {
              const value = await api.toggleTURN(enabled)
              status.update(() => value)
            })
          }}
        />{' '}
        {t.managedTURN}
      </label>
      <p id="turn-quota">
        {t.managedTURNHint}{' '}
        <a
          href={CLOUDFLARE_BILLING_DASHBOARD_URL}
          target="_blank"
          rel="noopener noreferrer"
        >
          {t.managedTURNBilling}
        </a>
      </p>
      <ReadError error={status.error} retry={status.retry} />
      <Notice feedback={action.feedback} />
    </section>
  )
}
