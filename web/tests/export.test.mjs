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
    'api-keys/index.html',
    'webhooks/index.html',
    'settings/index.html',
  ];

  for (const route of routes) {
    await readFile(new URL(route, output), 'utf8');
  }

  await assert.rejects(readFile(new URL('nodes/index.html', output)));
  const dashboard = await readFile(new URL('index.html', output), 'utf8');
  for (const label of ['Sandboxes', 'Templates', 'Environments', 'API keys', 'Webhooks', 'Settings']) {
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


test('Settings fetches and displays the local machine record', async () => {
  const workspace = await readFile(new URL('../app/ui/Workspace.tsx', import.meta.url), 'utf8');
  assert.match(workspace, /api<\{ machine: Machine \}>\('\/machine'/);
  for (const field of ['cpus', 'memoryBytes', 'memAvailableBytes', 'diskFreeBytes', 'runningBoxes', 'status', 'lastSeen']) {
    assert.ok(workspace.includes(`machine.${field}`), `Machine panel should display ${field}`);
  }
  assert.doesNotMatch(workspace, /['"]\/(nodes|pools|settings\/registry)/);
});
