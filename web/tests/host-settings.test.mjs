import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';

const root = new URL('../../', import.meta.url);

test('both installers relax unprivileged user namespaces when the host exposes the sysctl', async () => {
  for (const script of ['install/install.sh', 'install/node-install.sh']) {
    const source = await readFile(new URL(script, root), 'utf8');
    assert.match(source, /\/proc\/sys\/kernel\/apparmor_restrict_unprivileged_userns/);
    assert.match(source, /kernel\.apparmor_restrict_unprivileged_userns = 0/);
    assert.match(source, /sysctl -w kernel\.apparmor_restrict_unprivileged_userns=0/);
    assert.match(source, /Sysbox uses user namespaces itself/);
    assert.match(source, /Boxes: relaxed the host AppArmor user-namespace restriction/);
  }
});

test('self-hosting documentation explains the host setting', async () => {
  const docs = await readFile(new URL('docs/self-hosting.md', root), 'utf8');
  assert.match(docs, /## Host settings/);
  assert.match(docs, /bwrap.*inside a box/);
});

test('both installers configure optional tools images and idempotent host swap', async () => {
  for (const script of ['install/install.sh', 'install/node-install.sh']) {
    const source = await readFile(new URL(script, root), 'utf8');
    const agentEnv = source.match(/cat > \/etc\/boxes\/agent\.env <<EOF([\s\S]*?)\nEOF/);
    assert.ok(agentEnv, `${script} writes the agent environment`);
    assert.match(agentEnv[1], /BOXES_TOOLS_IMAGE=\$\{TOOLS_IMAGE\}/);
    assert.match(source, /swapon --show --noheadings/);
    assert.match(source, /size_mib > 8192/);
    assert.match(source, /vm\.swappiness = 10/);
  }
});
