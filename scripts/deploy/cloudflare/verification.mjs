import { setTimeout as delay } from 'node:timers/promises';

export const VERIFICATION_DELAYS_MS = Object.freeze([500, 1000, 2000, 4000, 8000]);

export class PropagationPendingError extends Error {
  constructor(message, reason) {
    super(message);
    this.name = 'PropagationPendingError';
    this.reason = reason;
  }
}

// Cloudflare's control-plane read endpoints (/settings, /subdomain, /domains, /workers/routes)
// are eventually consistent immediately after Wrangler uploads a new version and deployment.
// Retry only propagation lag; ownership mismatches and namespace drift must still fail immediately.
export async function confirmConverged(check, {
  deploymentId,
  log = () => {},
  wait = delay,
  delays = VERIFICATION_DELAYS_MS,
} = {}) {
  for (let attempt = 1; ; attempt++) {
    try {
      return await check();
    } catch (error) {
      if (!(error instanceof PropagationPendingError) || attempt > delays.length) throw error;
      log('deployment_verification_retry', { deploymentId, attempt, reason: error.reason });
      await wait(delays[attempt - 1]);
    }
  }
}
