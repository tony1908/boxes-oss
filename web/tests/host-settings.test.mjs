import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';

const root = new URL('../../', import.meta.url);

test('single-VM installer configures optional tools image and idempotent host swap', async () => {
  const source = await readFile(new URL('install/install.sh', root), 'utf8');
  const agentEnv = source.match(/cat > \/etc\/boxes\/agent\.env <<EOF([\s\S]*?)\nEOF/);
  assert.ok(agentEnv, 'installer writes the agent environment');
  assert.match(agentEnv[1], /BOXES_TOOLS_IMAGE=\$\{TOOLS_IMAGE\}/);
  assert.match(source, /swapon --show --noheadings/);
  assert.match(source, /size_mib > 8192/);
  assert.match(source, /vm\.swappiness = 10/);
});
