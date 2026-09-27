import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';

const sourceURL = new URL('../app/ui/Workspace.tsx', import.meta.url);

test('Nodes view provides owner autoscaling pool list and inline creation fields', async () => {
  const source = await readFile(sourceURL, 'utf8');
  assert.match(source, /Autoscaling pools/);
  assert.match(source, /\/pools/);
  for (const field of ['provider', 'region', 'size', 'image', 'minNodes', 'maxNodes', 'idleMinutes', 'credentials']) {
    assert.match(source, new RegExp(`name="${field}"`), `missing ${field} pool input`);
  }
  assert.match(source, /method: 'DELETE'/);
  assert.match(source, /\/pools\/\$\{/);
  assert.match(source, /Bake/);
  assert.match(source, /bakeStatus/);
  assert.match(source, /headroomSlots/);
  assert.match(source, /warmSchedule/);
  assert.match(source, /bootSeconds/);
  assert.match(source, /method: 'PATCH'/);
  assert.match(source, /method: 'POST', body: jsonBody\(\{\}\)/);
  assert.doesNotMatch(source, /window\.confirm/);
  assert.doesNotMatch(source, /window\.prompt|window\.alert/);
});
