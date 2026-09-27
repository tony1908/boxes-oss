import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';

const sourceURL = new URL('../app/ui/Workspace.tsx', import.meta.url);

test('Nodes table shows drained status and offers undrain for drained nodes', async () => {
  const source = await readFile(sourceURL, 'utf8');
  assert.match(source, /drained\?: boolean/);
  assert.match(source, /node\.drained\s*&&\s*<StateBadge state="Drained"\s*\/>/);
  assert.match(source, /node\.drained\s*\?\s*'Undrain'\s*:\s*'Drain'/);
  assert.match(source, /node\.drained\s*\?\s*'undrain'\s*:\s*'drain'/);
});
