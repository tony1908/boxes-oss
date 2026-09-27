import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';

const output = new URL('../out/', import.meta.url);

test('static export includes the dashboard and all primary screens', async () => {
  const routes = [
    'index.html',
    'login/index.html',
    'signup/index.html',
    'sandboxes/index.html',
    'sandbox/index.html',
    'templates/index.html',
    'environments/index.html',
    'nodes/index.html',
    'api-keys/index.html',
    'webhooks/index.html',
    'settings/index.html',
  ];

  for (const route of routes) {
    await readFile(new URL(route, output), 'utf8');
  }

  const dashboard = await readFile(new URL('index.html', output), 'utf8');
  for (const label of ['Sandboxes', 'Templates', 'Environments', 'Nodes', 'API keys', 'Webhooks', 'Settings']) {
    assert.ok(dashboard.includes(label), `dashboard navigation should include ${label}`);
  }
});

test('sandbox detail screen exposes all planned workspace tabs', async () => {
  const detail = await readFile(new URL('sandbox/index.html', output), 'utf8');
  const scripts = [...detail.matchAll(/src="(\/_next\/static\/chunks\/[^\"]+\.js)"/g)];
  const chunks = await Promise.all(scripts.map(async ([, src]) => readFile(new URL(src.slice(1), output), 'utf8')));
  const appCode = chunks.join('\n');
  for (const tab of ['Overview', 'Activity', 'Desktop', 'Snapshots', 'Hosts']) {
    assert.ok(appCode.includes(tab), `sandbox detail should include ${tab}`);
  }
});
