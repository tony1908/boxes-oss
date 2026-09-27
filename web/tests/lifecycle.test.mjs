import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
import { autoStopLabel, extendTTLSeconds, timerTTLSeconds } from '../app/lib/lifecycle.mjs';
import { activityPresentation, usageCountsLabel } from '../app/lib/activity.mjs';

const workspace = new URL('../app/ui/Workspace.tsx', import.meta.url);

test('activity rows use plain sentences and keep command and path values separate', () => {
  const show = (action, category = 'access', detail = {}) => activityPresentation({ action, category, detail });
  assert.deepEqual(show('file write', 'access', { path: '/config/notes.txt', sizeBytes: 13 }), { text: 'Wrote ', code: '/config/notes.txt', suffix: ' (13 B)' });
  assert.deepEqual(show('file read', 'access', { path: '/config/notes.txt', sizeBytes: 13 }), { text: 'Read ', code: '/config/notes.txt', suffix: ' (13 B)' });
  assert.deepEqual(show('exec', 'command', { command: "bash -lc 'df -h /'" }), { text: 'Ran ', code: 'df -h /', suffix: '' });
  assert.equal(show('desktop session reused').text, 'Opened desktop');
  assert.equal(show('periodic snapshots', 'snapshot', { count: 12 }).text, 'Took 12 periodic snapshots');
  assert.equal(show('host exposed', 'access', { port: 3000, public: true }).text, 'Exposed port 3000 (public)');
  assert.equal(show('ssh proxy session opened').text, 'Opened SSH session');
  assert.equal(show('Stopped after 30 min idle', 'lifecycle').text, 'Stopped after 30 min idle');
  assert.equal(show('unknown_action').text, 'Unknown action');
});

test('usage counts include correct singulars and latest snapshot size', () => {
  assert.equal(usageCountsLabel({ commandsCount: 4, agentRunsCount: 0, desktopSessionsCount: 1, snapshotsCount: 5, snapshotsBytes: 537 * 1024 ** 2 }), '4 commands, 0 agent runs, 1 desktop session, 5 snapshots kept (latest 537 MB)');
  assert.equal(usageCountsLabel({ commandsCount: 1, agentRunsCount: 1, desktopSessionsCount: 1, snapshotsCount: 1, snapshotsBytes: 13 }), '1 command, 1 agent run, 1 desktop session, 1 snapshot kept (latest 13 B)');
});

test('auto-stop label prefers the absolute expiry and falls back to remaining seconds', () => {
  const now = Date.parse('2026-09-24T12:00:00.000Z');
  assert.equal(autoStopLabel({ autoStop: true, expiresAt: '2026-09-24T12:42:00.000Z' }, now), 'Auto-stops in 42m');
  assert.equal(autoStopLabel({ autoStop: true, ttlRemainingSeconds: 3600 }, now), 'Auto-stops in 1h');
  assert.equal(autoStopLabel({ autoStop: false, expiresAt: '2026-09-24T12:42:00.000Z' }, now), 'Always on');
});

test('extending a timer adds one hour to the remaining TTL from now', () => {
  const now = Date.parse('2026-09-24T12:00:00.000Z');
  assert.equal(extendTTLSeconds({ autoStop: true, expiresAt: '2026-09-24T12:42:00.000Z' }, now), 6120);
  assert.equal(extendTTLSeconds({ autoStop: true, ttlRemainingSeconds: 300 }, now), 3900);
});

test('timer choices map custom durations to seconds and always-on to null', () => {
  assert.equal(timerTTLSeconds('3600'), 3600);
  assert.equal(timerTTLSeconds('custom', 5400), 5400);
  assert.equal(timerTTLSeconds('always'), null);
});

test('workspace presents inline delete confirmation and lifecycle controls', async () => {
  const source = await readFile(workspace, 'utf8');
  assert.match(source, /autoStopLabel\(box, now\)/);
  assert.match(source, /autoStopLabel\(sandbox, now\)/);
  assert.match(source, /Always on/);
  assert.match(source, /Set auto-stop/);
  assert.match(source, /Delete this sandbox permanently\?/);
  assert.match(source, /method: 'PATCH', body: jsonBody\(\{ ttlSeconds \}\)/);
  assert.match(source, /option value="14400">4 hours/);
  assert.match(source, /option value="always">Always on/);
  assert.doesNotMatch(source, /window\.(?:confirm|alert)\(/);
});

test('sandbox size picker offers xsmall alongside existing sizes', async () => {
  const source = await readFile(workspace, 'utf8');
  assert.match(source, /option value="xsmall">XSmall \/ 1 CPU \/ 2 GB/);
  for (const size of ['small', 'default', 'large', 'xlarge']) {
    assert.match(source, new RegExp(`option value="${size}"`));
  }
});
