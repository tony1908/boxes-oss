const { spawn } = require('node:child_process');
const readline = require('node:readline');

const child = spawn('boxd', ['mcp'], { stdio: ['pipe', 'pipe', 'inherit'] });
const reader = readline.createInterface({ input: child.stdout });
const pending = new Map();

reader.on('line', (line) => {
  let message;
  try {
    message = JSON.parse(line);
  } catch (error) {
    process.stderr.write(`invalid MCP response: ${error.message}\n`);
    child.kill();
    process.exitCode = 1;
    return;
  }
  const resolve = pending.get(message.id);
  if (resolve) {
    pending.delete(message.id);
    resolve(message);
  }
});

function request(id, method, params) {
  const response = new Promise((resolve, reject) => {
    const timeout = setTimeout(() => {
      pending.delete(id);
      reject(new Error(`MCP ${method} timed out`));
    }, 120_000);
    pending.set(id, (message) => {
      clearTimeout(timeout);
      resolve(message);
    });
  });
  child.stdin.write(`${JSON.stringify({ jsonrpc: '2.0', id, method, params })}\n`);
  return response.then((message) => {
    process.stdout.write(`${JSON.stringify(message)}\n`);
    if (message.error) throw new Error(`MCP ${method} failed: ${message.error.message}`);
    return message;
  });
}

async function main() {
  await request(1, 'initialize', {
    protocolVersion: '2025-03-26',
    capabilities: {},
    clientInfo: { name: 'paddock-e2e', version: '1.0' },
  });
  child.stdin.write(`${JSON.stringify({ jsonrpc: '2.0', method: 'notifications/initialized' })}\n`);
  await request(2, 'tools/list', {});
  await request(3, 'tools/call', { name: 'screenshot', arguments: {} });
  child.stdin.end();
  await new Promise((resolve, reject) => {
    child.once('exit', (code) => code === 0 ? resolve() : reject(new Error(`boxd mcp exited ${code}`)));
    child.once('error', reject);
  });
}

main().catch((error) => {
  process.stderr.write(`${error.message}\n`);
  child.kill();
  process.exitCode = 1;
});
