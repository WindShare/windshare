import assert from 'node:assert/strict';
import test from 'node:test';
import { analyticsSecret } from './turn.mjs';

test('optional analytics cannot copy deployment authority into a Worker', () => {
  assert.deepEqual(analyticsSecret({ CLOUDFLARE_API_TOKEN: 'deployment' }), {});
  assert.deepEqual(analyticsSecret({ CLOUDFLARE_API_TOKEN: 'deployment', CLOUDFLARE_ANALYTICS_API_TOKEN: 'deployment' }), {});
  const value = analyticsSecret({ CLOUDFLARE_API_TOKEN: 'deployment',
    CLOUDFLARE_ANALYTICS_API_TOKEN: 'readonly', CLOUDFLARE_ACCOUNT_ID: 'account' });
  assert.deepEqual(JSON.parse(value.TURN_ANALYTICS), { api_token: 'readonly', account_id: 'account' });
});
