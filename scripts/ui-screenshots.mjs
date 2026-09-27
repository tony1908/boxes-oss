#!/usr/bin/env node
/**
 * Capture dashboard screenshots using Chrome's built-in remote debugging protocol.
 *
 * Usage (Node 22, no extra dependencies):
 *   BASE_URL=http://127.0.0.1:18090 EMAIL=owner@example.test PASSWORD='long password here' \
 *     node scripts/ui-screenshots.mjs /path/to/output
 *
 * Chrome is launched headless with a temporary profile. The script signs in through
 * /api/v1/auth/login, installs the boxes_session cookie through CDP, and saves desktop
 * and mobile captures for the dashboard routes listed below.
 */
import { mkdtemp, mkdir, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawn } from 'node:child_process';
import net from 'node:net';
import { setTimeout as delay } from 'node:timers/promises';

const baseURL = process.env.BASE_URL;
const email = process.env.EMAIL;
const password = process.env.PASSWORD;
const outputDir = process.argv[2];
if (!baseURL || !email || !password || !outputDir) throw new Error('Set BASE_URL, EMAIL, PASSWORD and provide an output directory.');
const colorScheme = process.env.COLOR_SCHEME ?? 'dark';
if (!['light', 'dark'].includes(colorScheme)) throw new Error('COLOR_SCHEME must be light or dark.');
const chromePath = '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
const paths = ['/login/', '/', '/sandboxes/', '/nodes/', '/templates/', '/environments/', '/api-keys/', '/webhooks/', '/settings/', ...(process.env.EXTRA_PATHS ?? '').split(',').map((path) => path.trim()).filter(Boolean)];
const parsed = new URL(baseURL);
const tempDir = await mkdtemp(join(tmpdir(), 'boxes-ui-chrome-'));
await mkdir(outputDir, { recursive: true });
const server = net.createServer();
await new Promise((resolve, reject) => server.listen(0, '127.0.0.1', resolve).once('error', reject));
const port = server.address().port;
await new Promise((resolve) => server.close(resolve));
const chrome = spawn(chromePath, ['--headless=new', `--remote-debugging-port=${port}`, `--user-data-dir=${tempDir}`, '--no-first-run', '--no-default-browser-check', '--disable-gpu', 'about:blank'], { stdio: 'ignore' });
let ws;
try {
  let target;
  for (let i = 0; i < 100; i++) {
    try { const targets = await (await fetch(`http://127.0.0.1:${port}/json/list`)).json(); target = targets.find((item) => item.type === 'page'); if (target) break; } catch {}
    await delay(100);
  }
  if (!target) throw new Error('Chrome remote debugging endpoint did not start.');
  ws = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((resolve, reject) => { ws.addEventListener('open', resolve, { once: true }); ws.addEventListener('error', reject, { once: true }); });
  let nextID = 0;
  const pending = new Map();
  ws.addEventListener('message', (event) => {
    const message = JSON.parse(event.data);
    if (!message.id || !pending.has(message.id)) return;
    const { resolve, reject } = pending.get(message.id); pending.delete(message.id);
    if (message.error) reject(new Error(message.error.message)); else resolve(message.result);
  });
  const call = (method, params = {}) => new Promise((resolve, reject) => {
    const id = ++nextID; pending.set(id, { resolve, reject }); ws.send(JSON.stringify({ id, method, params }));
  });
  await call('Page.enable'); await call('Network.enable'); await call('Runtime.enable');
  await call('Emulation.setEmulatedMedia', { features: [{ name: 'prefers-color-scheme', value: colorScheme }] });
  const capture = async (path) => {
    for (const viewport of [{ name: 'desktop', width: 1440, height: 900 }, { name: 'mobile', width: 390, height: 844 }]) {
      await call('Emulation.setDeviceMetricsOverride', { width: viewport.width, height: viewport.height, deviceScaleFactor: 1, mobile: viewport.width < 640 });
      await call('Emulation.setEmulatedMedia', { features: [{ name: 'prefers-color-scheme', value: colorScheme }] });
      await call('Page.navigate', { url: new URL(path, baseURL).href }); await delay(Number(process.env.WAIT_MS || 1500));
      const shot = await call('Page.captureScreenshot', { format: 'png', fromSurface: true, captureBeyondViewport: false });
      const filename = `${path === '/' ? 'home' : path.replaceAll('/', '').replaceAll('-', '_')}-${colorScheme}-${viewport.name}.png`;
      await writeFile(join(outputDir, filename), Buffer.from(shot.data, 'base64')); console.log(join(outputDir, filename));
    }
  };
  await call('Page.navigate', { url: baseURL }); await delay(500);
  await call('Runtime.evaluate', { expression: "localStorage.removeItem('boxes-theme')" });
  await capture('/login/');
  await call('Page.navigate', { url: baseURL }); await delay(500);
  const login = await call('Runtime.evaluate', { awaitPromise: true, returnByValue: true, expression: `fetch(${JSON.stringify(new URL('/api/v1/auth/login', baseURL).href)}, {method:'POST', credentials:'include', headers:{'Content-Type':'application/json'}, body:JSON.stringify({email:${JSON.stringify(email)},password:${JSON.stringify(password)}})}).then(async r=>{const body=await r.json(); if(!r.ok) throw new Error(body.error?.message || 'Login failed'); return body.token || body.sessionToken})` });
  const token = login.result?.value;
  if (!token) throw new Error('Login did not return a session token.');
  await call('Network.setCookie', { name: 'boxes_session', value: token, url: baseURL, path: '/', httpOnly: true, secure: parsed.protocol === 'https:', sameSite: 'Lax' });
  for (const path of paths.slice(1)) await capture(path);
} finally {
  try { ws?.close(); } catch {}
  chrome.kill('SIGTERM');
  await delay(300);
  await rm(tempDir, { recursive: true, force: true });
}
