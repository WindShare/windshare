import type { DonationsEnv } from './env'
import { handleWorkerFetch } from './router'

export { DonationWall } from './wall-do'

const worker: ExportedHandler<DonationsEnv> = {
  fetch(request, env) {
    return handleWorkerFetch(request, env)
  },
}

export default worker
