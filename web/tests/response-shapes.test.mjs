import assert from 'node:assert/strict';
import test from 'node:test';
import { createdAPIKeySecret, listResourceResponse } from '../app/lib/response-shapes.mjs';

test('unwraps dashboard lists using server response fields', () => {
  const cases = [
    ['sandboxes', { sandboxes: [{ id: 'sbx_1' }] }],
    ['templates', { namedSnapshots: [{ name: 'base', snapshotId: 'snap_1' }] }],
    ['environments', { environments: [{ id: 'env_1' }] }],
    ['api-keys', { apiKeys: [{ id: 'key_1', usage: { requests: 3, sandboxesCreated: 1 } }] }],
    ['webhooks', { webhooks: [{ id: 'wh_1' }] }],
  ];

  for (const [view, response] of cases) {
    assert.deepEqual(listResourceResponse(response, view), Object.values(response)[0], view);
  }
  assert.deepEqual(listResourceResponse({ snapshots: [] }, 'templates'), []);
  assert.deepEqual(listResourceResponse([], 'sandboxes'), []);
});

test('extracts the one-time API key from the server creation response', () => {
  assert.equal(createdAPIKeySecret({ apiKey: { id: 'key_1', key: 'bx_live_secret' } }), 'bx_live_secret');
  assert.equal(createdAPIKeySecret({ key: 'legacy-key' }), 'legacy-key');
  assert.equal(createdAPIKeySecret({ token: 'legacy-token' }), 'legacy-token');
});
