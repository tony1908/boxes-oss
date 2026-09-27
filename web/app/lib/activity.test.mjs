import test from 'node:test';
import assert from 'node:assert/strict';
import { activityPresentation } from './activity.mjs';

test('unwraps quoted and unquoted shell command wrappers', () => {
  for (const command of ['bash -lc df -h /', 'bash -lc "df -h /"', "sh -c 'node --version || true'", 'bash -lc node --version || true']) {
    const result = activityPresentation({ category: 'command', detail: { command } });
    assert.equal(result.code, command.includes('df -h /') ? 'df -h /' : 'node --version || true');
  }
});

test('presents file written and file read activity', () => {
  assert.deepEqual(activityPresentation({ action: 'file written', detail: { path: '/config/notes.txt', sizeBytes: 13 } }), {
    text: 'Wrote ', code: '/config/notes.txt', suffix: ' (13 B)',
  });
  assert.deepEqual(activityPresentation({ action: 'file read', detail: { path: '/config/notes.txt', sizeBytes: 13 } }), {
    text: 'Read ', code: '/config/notes.txt', suffix: ' (13 B)',
  });
});

test('presents error activity with a trimmed failed reason', () => {
  const reason = `  Snapshot failed: ${'restic backup failed because of a lock '.repeat(8)}  `;
  const result = activityPresentation({ action: 'error', detail: { reason } });
  assert.equal(result.text, reason.trim().slice(0, 140));
  assert.equal(result.result, 'Failed');
});

test('presents pushed file activity', () => {
  assert.deepEqual(activityPresentation({ action: 'files pushed', detail: { path: '/config/repo', sizeBytes: 1048576 } }), {
    text: 'Pushed files to /config/repo (1 MB)',
  });
});

test('presents downloaded artifact activity with its path', () => {
  assert.deepEqual(activityPresentation({ action: 'downloaded', detail: { path: '/config/report.csv' } }), { text: 'Downloaded /config/report.csv' });
});
