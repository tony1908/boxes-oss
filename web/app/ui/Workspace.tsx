'use client';

import Link from 'next/link';
import { FormEvent, ReactNode, useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { api, apiResponse, jsonBody } from '../lib/api';
import { autoStopLabel, extendTTLSeconds, timerTTLSeconds } from '../lib/lifecycle.mjs';
import { activityPresentation, usageCountsLabel } from '../lib/activity.mjs';
import { createdAPIKeySecret, listResourceResponse } from '../lib/response-shapes.mjs';
import { BrandMark } from './BrandMark';
import { Icon } from './icons';

type View = 'sandboxes' | 'sandbox' | 'templates' | 'environments' | 'api-keys' | 'webhooks' | 'settings';
type Tab = 'Overview' | 'Activity' | 'Desktop' | 'Snapshots' | 'Hosts';
type Sandbox = { id: string; state: string; type?: string; autoStop?: boolean; ttlSeconds?: number | null; idleStopSeconds?: number | null; ttlRemainingSeconds?: number; expiresAt?: string; createdAt?: string; lastSnapshotAt?: string; latestSnapshotAt?: string };
type Machine = { cpus: number; memoryBytes: number; memAvailableBytes: number; diskFreeBytes: number; swapFreeBytes: number; memoryPressureSomeAvg10: number; status: 'healthy' | 'unhealthy'; runningBoxes: number; lastSeen: string };
type Snapshot = { id?: string; snapshotId?: string; sizeBytes?: number; createdAt?: string };
type UserRecord = { id?: string; email?: string; role?: string };
type SnapshotStorage = { type: 'local' | 's3'; endpoint?: string; bucket?: string; prefix?: string };
type ActivityItem = { id: string; time: string; actorType: string; actorLabel: string; category: string; action: string; detail: Record<string, unknown>; durationMs?: number; outcome?: string };
type Usage = { runningSeconds: number; runningSecondsThisMonth: number; perDay: { date: string; runningSeconds: number }[]; commandsCount: number; agentRunsCount: number; desktopSessionsCount: number; snapshotsCount: number; snapshotsBytes: number; lastSnapshotAt?: string; estimatedCost?: { currency: string; amount: number; hourlyRate?: number } };
type Pricing = { currency: string; hourlyBySize: Record<string, number> };

const links: { label: string; href: string; icon: any; view: View }[] = [
  { label: 'Sandboxes', href: '/', icon: 'sandboxes', view: 'sandboxes' },
  { label: 'Templates', href: '/templates/', icon: 'templates', view: 'templates' },
  { label: 'Environments', href: '/environments/', icon: 'environments', view: 'environments' },
];
const settingsLinks: { label: string; href: string; view: View; icon: string }[] = [
  { label: 'API keys', href: '/api-keys/', view: 'api-keys', icon: 'api-keys' },
  { label: 'Webhooks', href: '/webhooks/', view: 'webhooks', icon: 'webhooks' },
  { label: 'Settings', href: '/settings/', view: 'settings', icon: 'settings' },
];
const titles: Record<View, { title: string; description: string }> = {
  sandboxes: { title: 'Sandboxes', description: '' },
  sandbox: { title: 'Sandbox', description: '' },
  templates: { title: 'Templates', description: '' },
  environments: { title: 'Environments', description: '' },
  'api-keys': { title: 'API keys', description: '' },
  webhooks: { title: 'Webhooks', description: '' },
  settings: { title: 'Settings', description: '' },
};


function SectionHeader({ title, description, actions }: { title: string; description?: string; actions?: ReactNode }) {
  return <div className="section-header"><div className="section-header-copy"><h2>{title}</h2>{description && <p>{description}</p>}</div>{actions && <div className="section-header-actions">{actions}</div>}</div>;
}

function LoadingState() {
  return <div className="loading-state"><BrandMark size={24} animated /><span>Loading…</span></div>;
}

function InlineForm({ children, className = '', onSubmit }: { children: ReactNode; className?: string; onSubmit: (event: FormEvent<HTMLFormElement>) => void }) {
  return <form className={`inline-form ${className}`} onSubmit={onSubmit}>{children}</form>;
}

function listFrom<T>(data: unknown, key?: string): T[] {
  if (Array.isArray(data)) return data as T[];
  if (data && typeof data === 'object' && key && Array.isArray((data as Record<string, unknown>)[key])) {
    return (data as Record<string, unknown>)[key] as T[];
  }
  return [];
}

function formatDate(value?: string) {
  if (!value) return '-';
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' });
}

function formatBytes(value?: number) {
  if (value == null || !Number.isFinite(value)) return '-';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  const step = Math.min(Math.floor(Math.log(value) / Math.log(1024)), units.length - 1);
  return `${(value / 1024 ** step).toFixed(step > 1 ? 1 : 0)} ${units[step]}`;
}
function sizeLabel(type?: string) { const value = (type ?? 'default').toLowerCase(); return value.charAt(0).toUpperCase() + value.slice(1); }
function sizeSpecs(type?: string) { const specs: Record<string, string> = { xsmall: ' (1 vCPU, 2 GB RAM)', small: ' (2 vCPU, 4 GB RAM)', default: ' (4 vCPU, 8 GB RAM)', large: ' (8 vCPU, 16 GB RAM)', xlarge: ' (16 vCPU, 32 GB RAM)' }; return specs[(type ?? 'default').toLowerCase()] ?? ''; }
function relativeTime(value?: string) { if (!value) return '-'; const ms = Date.parse(value); if (!Number.isFinite(ms)) return value; const minutes = Math.max(1, Math.floor((Date.now() - ms) / 60000)); if (minutes < 60) return `${minutes} minute${minutes === 1 ? '' : 's'} ago`; const hours = Math.floor(minutes / 60); if (hours < 24) return `${hours} hour${hours === 1 ? '' : 's'} ago`; const days = Math.floor(hours / 24); return `${days} day${days === 1 ? '' : 's'} ago`; }
function detailAutoStopLabel(sandbox: Sandbox, now: number) { const value = autoStopLabel(sandbox, now); if (value === 'Always on') return value; return value.replace(/^Auto-stops in /, 'Stops in ').replace(/(\d+)m$/, '$1 min').replace(/(\d+)h(?: (\d+)m)?$/, (_, h, m) => `${h} hr${m ? ` ${m} min` : ''}`); }
function idleStopLabel(seconds?: number | null) { if (!seconds) return ''; const minutes = Math.round(seconds / 60); return `Idle stop ${minutes < 60 ? `${minutes} min` : `${Math.floor(minutes / 60)} hr${minutes % 60 ? ` ${minutes % 60} min` : ''}`}`; }

export function Workspace({ view }: { view: View }) {
  const [items, setItems] = useState<unknown[]>([]);
  const [loading, setLoading] = useState(true);
  const [problem, setProblem] = useState('');
  const [notice, setNotice] = useState('');
  const [copiedID, setCopiedID] = useState(false);
  const [dialog, setDialog] = useState(false);
  const [timerDialog, setTimerDialog] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [createTTL, setCreateTTL] = useState('3600');
  const [timerSelection, setTimerSelection] = useState('3600');
  const [sandboxFilter, setSandboxFilter] = useState<'all' | 'r' | 's'>('all');
  const [now, setNow] = useState(() => Date.now());
  const [detailId, setDetailId] = useState('');
  const [sandbox, setSandbox] = useState<Sandbox | null>(null);
  const [account, setAccount] = useState<UserRecord | null>(null);
  const [snapshotStorage, setSnapshotStorage] = useState<SnapshotStorage | null>(null);
  const [desktopControl, setDesktopControl] = useState<Record<string, boolean>>({});
  const [tab, setTab] = useState<Tab>('Overview');
  const [desktopURL, setDesktopURL] = useState('');
  const [desktopExpiresAt, setDesktopExpiresAt] = useState(0);
  const [origin, setOrigin] = useState('');
  const [createdKey, setCreatedKey] = useState('');
  const [rotatedSecret, setRotatedSecret] = useState('');
  const [theme, setTheme] = useState('light');
  const [signingOut, setSigningOut] = useState(false);
  const heading = titles[view];

  useEffect(() => {
    const query = new URLSearchParams(window.location.search);
    setDetailId(query.get('id') ?? '');
    const requestedTab = query.get('tab');
    const tabNames: Record<string, Tab> = { overview: 'Overview', activity: 'Activity', desktop: 'Desktop', conversations: 'Activity', snapshots: 'Snapshots', hosts: 'Hosts' };
    if (requestedTab && tabNames[requestedTab]) setTab(tabNames[requestedTab]);
    if (requestedTab === 'conversations') { query.set('tab', 'activity'); window.history.replaceState({}, '', `${window.location.pathname}?${query.toString()}`); }
    setOrigin(window.location.origin);
    const savedTheme = window.localStorage.getItem('boxes-theme');
    if (savedTheme === 'light' || savedTheme === 'dark') document.documentElement.dataset.theme = savedTheme;
    setTheme(savedTheme === 'light' || savedTheme === 'dark' ? savedTheme : window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light');
  }, []);

  useEffect(() => {
    let alive = true;
    void api<{ user?: UserRecord }>('/me', { cache: 'no-store' }).then((profile) => {
      if (alive) setAccount(profile.user ?? null);
    }).catch(() => {});
    return () => { alive = false; };
  }, []);

  useEffect(() => {
    const media = window.matchMedia('(prefers-color-scheme: dark)');
    const updateTheme = (event: MediaQueryListEvent) => {
      if (!window.localStorage.getItem('boxes-theme')) setTheme(event.matches ? 'dark' : 'light');
    };
    media.addEventListener('change', updateTheme);
    return () => media.removeEventListener('change', updateTheme);
  }, []);

  useEffect(() => {
    const updateIndicators = () => document.querySelectorAll<HTMLElement>('.tab-bar,.sandbox-filter-tabs').forEach((group) => { const active = group.querySelector<HTMLElement>('[aria-selected="true"]'); if (active) { group.style.setProperty('--indicator-left', `${active.offsetLeft}px`); group.style.setProperty('--indicator-width', `${active.offsetWidth}px`); } });
    updateIndicators();
    window.addEventListener('resize', updateIndicators);
    return () => window.removeEventListener('resize', updateIndicators);
  }, [tab, sandboxFilter]);

  useEffect(() => {
    const interval = window.setInterval(() => setNow(Date.now()), 30_000);
    return () => window.clearInterval(interval);
  }, []);

  const endpoint = useMemo(() => {
    if (view === 'sandbox') return detailId ? `/sandboxes/${encodeURIComponent(detailId)}` : '';
    const collection: Partial<Record<View, string>> = {
      sandboxes: '/sandboxes', templates: '/named-snapshots', environments: '/environments',
      'api-keys': '/api-keys', webhooks: '/webhooks', settings: '/me',
    };
    return collection[view] ?? '';
  }, [view, detailId]);

  const refresh = useCallback(async () => {
    if (!endpoint) {
      setLoading(false);
      if (view === 'sandbox') setProblem('Choose a sandbox from the list to inspect its workspace.');
      return;
    }
    setLoading(true);
    setProblem('');
    try {
      const [result, profile] = await Promise.all([
        api<unknown>(`${endpoint}${view === 'sandboxes' && sandboxFilter !== 'all' ? `?filter=${sandboxFilter}` : ''}`, { cache: 'no-store' }),
        endpoint === '/me' ? Promise.resolve(null) : api<{ user?: UserRecord }>('/me', { cache: 'no-store' }),
      ]);
      const user = ((profile ?? result) as { user?: UserRecord } | null)?.user;
      setAccount(user ?? null);
      if (view === 'sandbox') {
        const payload = result && typeof result === 'object' && 'sandbox' in result ? (result as { sandbox: Sandbox }).sandbox : result as Sandbox;
        setSandbox(payload);
        setItems([]);
      } else if (view === 'settings') {
        setItems(user ? [user] : []);
      } else {
        setItems(listResourceResponse(result, view));
      }
    } catch (cause) {
      setItems([]);
      setProblem(cause instanceof Error ? cause.message : 'Could not reach the Boxes API.');
    } finally {
      setLoading(false);
    }
  }, [endpoint, view, sandboxFilter]);

  useEffect(() => { void refresh(); }, [refresh]);

  useEffect(() => {
    if (view !== 'settings' || account?.role !== 'owner') {
      setSnapshotStorage(null);
      return;
    }
    let alive = true;
    void api<SnapshotStorage>('/settings/storage', { cache: 'no-store' })
      .then((storage) => { if (alive) setSnapshotStorage(storage); })
      .catch(() => { if (alive) setSnapshotStorage(null); });
    return () => { alive = false; };
  }, [view, account?.role]);

  function changeTab(next: Tab) {
    setTab(next);
    const url = new URL(window.location.href);
    url.searchParams.set('id', detailId);
    url.searchParams.set('tab', next.toLowerCase());
    window.history.replaceState({}, '', url);
  }

  function flash(message: string) {
    setNotice(message);
    window.setTimeout(() => setNotice(''), 3500);
  }

  function toggleTheme() {
    const next = theme === 'dark' ? 'light' : 'dark';
    document.documentElement.dataset.theme = next;
    setTheme(next);
    window.localStorage.setItem('boxes-theme', next);
  }

  async function signOut() {
    setSigningOut(true);
    try {
      await api('/auth/logout', { method: 'POST' });
      window.location.assign('/login/');
    } catch (cause) {
      setProblem(cause instanceof Error ? cause.message : 'Could not sign out.');
      setSigningOut(false);
    }
  }

  async function createSandbox(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const data = new FormData(event.currentTarget);
    const selectedTTL = String(data.get('ttlPreset') ?? '3600');
    const ttlSeconds = timerTTLSeconds(selectedTTL, Number(data.get('customTTL')));
    try {
      const created = await api<{ id?: string; sandbox?: Sandbox }>('/sandboxes', {
        method: 'POST', headers: { 'Idempotency-Key': crypto.randomUUID() },
        body: jsonBody({ type: data.get('type'), ttlSeconds, ...(data.get('idleStop') ? { idleStopSeconds: Number(data.get('idleStop')) } : {}) }),
      });
      const createdSandbox = created.sandbox ?? created as Sandbox;
      setDialog(false);
      flash('Sandbox is provisioning.');
      if (createdSandbox.id) window.location.assign(`/sandbox/?id=${encodeURIComponent(createdSandbox.id)}`);
      else await refresh();
    } catch (cause) { setProblem(cause instanceof Error ? cause.message : 'Sandbox could not be created.'); }
  }

  async function sandboxAction(action: 'stop' | 'resume' | 'delete') {
    if (!sandbox || !detailId) return;
    try {
      await api(`/sandboxes/${encodeURIComponent(detailId)}${action === 'delete' ? '' : `/${action}`}`, { method: action === 'delete' ? 'DELETE' : 'POST', body: action === 'resume' ? jsonBody({}) : undefined });
      if (action === 'delete') window.location.assign('/');
      else { flash(`Sandbox ${action} requested.`); await refresh(); }
    } catch (cause) { setProblem(cause instanceof Error ? cause.message : `Could not ${action} sandbox.`); }
  }

  async function updateSandboxTimer(ttlSeconds: number | null) {
    if (!sandbox || !detailId) return;
    try {
      await api(`/sandboxes/${encodeURIComponent(detailId)}`, { method: 'PATCH', body: jsonBody({ ttlSeconds }) });
      setTimerDialog(false);
      flash(ttlSeconds === null ? 'Sandbox is now always on.' : 'Auto-stop timer updated.');
      await refresh();
    } catch (cause) { setProblem(cause instanceof Error ? cause.message : 'Sandbox timer could not be updated.'); }
  }

  async function updateIdleStop(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!sandbox || !detailId) return;
    const form = new FormData(event.currentTarget);
    const raw = String(form.get('idleStop') ?? '').trim();
    const idleStopSeconds = raw === '' || raw === 'off' ? null : Number(raw);
    if (idleStopSeconds !== null && (!Number.isInteger(idleStopSeconds) || idleStopSeconds < 300 || idleStopSeconds > 2592000)) { setProblem('Idle stop must be 300 to 2592000 seconds, or off.'); return; }
    try { await api(`/sandboxes/${encodeURIComponent(detailId)}`, { method: 'PATCH', body: jsonBody({ idleStopSeconds }) }); setTimerDialog(false); flash('Idle stop updated.'); await refresh(); }
    catch (cause) { setProblem(cause instanceof Error ? cause.message : 'Idle stop could not be updated.'); }
  }

  async function setSandboxTimer(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const data = new FormData(event.currentTarget);
    await updateSandboxTimer(timerTTLSeconds(String(data.get('ttlPreset') ?? '3600'), Number(data.get('customTTL'))));
  }

  async function extendSandboxTimer() {
    if (!sandbox) return;
    await updateSandboxTimer(extendTTLSeconds(sandbox, now));
  }

  async function createKey(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const data = new FormData(event.currentTarget);
    try {
      const result = await api<{ apiKey?: { key?: string }; key?: string; token?: string }>('/api-keys', { method: 'POST', body: jsonBody({ name: data.get('name'), scopes: ['*'] }) });
      const secret = createdAPIKeySecret(result);
      setDialog(false); setCreatedKey(secret ?? ''); flash(secret ? 'API key created. Copy it now; it will not be shown again.' : 'API key created.'); await refresh();
    } catch (cause) { setProblem(cause instanceof Error ? cause.message : 'API key could not be created.'); }
  }

  async function createWebhook(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const data = new FormData(event.currentTarget);
    try {
      await api('/webhooks', { method: 'POST', body: jsonBody({ url: data.get('url'), events: ['sandbox.ready', 'sandbox.restored', 'sandbox.error', 'sandbox.stopped', 'sandbox.deleted'] }) });
      setDialog(false); flash('Webhook created.'); await refresh();
    } catch (cause) { setProblem(cause instanceof Error ? cause.message : 'Webhook could not be created.'); }
  }

  async function rotateWebhook(id: string) {
    try { const result = await api<{ secret: string }>(`/webhooks/${encodeURIComponent(id)}/rotate-secret`, { method: 'POST', body: jsonBody({}) }); setRotatedSecret(result.secret); flash('Webhook secret rotated. Copy it now; it will not be shown again.'); }
    catch (cause) { setProblem(cause instanceof Error ? cause.message : 'Webhook secret could not be rotated.'); }
  }

  async function createTemplate(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const data = new FormData(event.currentTarget);
    try {
      await api('/named-snapshots', { method: 'POST', body: jsonBody({ name: data.get('name'), snapshotId: data.get('snapshotId') }) });
      setDialog(false); flash('Template saved.'); await refresh();
    } catch (cause) { setProblem(cause instanceof Error ? cause.message : 'Template could not be saved.'); }
  }

  async function createEnvironment(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const data = new FormData(event.currentTarget);
    const vars = Object.fromEntries(String(data.get('vars') ?? '').split('\n').flatMap((line) => {
      const separator = line.indexOf('=');
      return separator < 1 ? [] : [[line.slice(0, separator).trim(), line.slice(separator + 1).trim()]];
    }));
    try {
      await api('/environments', { method: 'POST', body: jsonBody({ name: data.get('name'), vars, setupScript: data.get('setupScript') }) });
      setDialog(false); flash('Environment created.'); await refresh();
    } catch (cause) { setProblem(cause instanceof Error ? cause.message : 'Environment could not be created.'); }
  }

  async function openDesktop(): Promise<boolean> {
    try {
      const result = await api<{ url?: string; expiresAt?: string; ready?: boolean }>(`/sandboxes/${encodeURIComponent(detailId)}/desktop`, { method: 'POST', body: jsonBody({}) });
      if (result.url) { setDesktopURL(result.url); setDesktopExpiresAt(result.expiresAt ? Date.parse(result.expiresAt) : Date.now() + 8 * 60 * 60 * 1000); }
      return result.ready !== false;
    } catch (cause) { setProblem(cause instanceof Error ? cause.message : 'Desktop could not be opened.'); throw cause; }
  }

  const activeNav = view === 'sandbox' ? 'sandboxes' : view;

  return (
    <div className="app-shell">
      <aside className="sidebar">
        <Link className="brand" href="/"><BrandMark size={18} animated={loading} /><span>boxes</span></Link>
        <nav aria-label="Workspace">
          {links.map((item) => <Link className={`nav-item ${activeNav === item.view ? 'active' : ''}`} href={item.href} key={item.view}><Icon name={item.icon} className="nav-icon" />{item.label}</Link>)}
          {settingsLinks.map((item) => <Link className={`nav-item ${activeNav === item.view ? 'active' : ''}`} href={item.href} key={item.view}><Icon name={item.icon as any} className="nav-icon" />{item.label}</Link>)}
        </nav>
        <div className="sidebar-bottom"><button className="theme-toggle" onClick={toggleTheme}>{theme === 'dark' ? 'Light theme' : 'Dark theme'}</button><div className="sidebar-account"><strong>{account?.email ?? ''}</strong><button type="button" className="text-link" onClick={() => void signOut()} disabled={signingOut}>{signingOut ? 'Signing out...' : 'Sign out'}</button></div></div>
      </aside>
      <main className="main-area">
        <header className="topbar"><Link className="brand mobile-brand" href="/"><BrandMark size={20} animated={loading} /><span>boxes</span></Link><button className="menu-toggle" onClick={() => document.body.classList.toggle('nav-open')}><Icon name="menu" size={18} />Menu</button></header>
        <div className="page-content">
          <div className="page-heading"><div>{view === 'sandbox' && sandbox ? <><h1 className="box-title"><code>{sandbox.id}</code><button className="copy-button" aria-label="Copy box ID" title="Copy box ID" onClick={() => { void navigator.clipboard?.writeText(sandbox.id); setCopiedID(true); window.setTimeout(() => setCopiedID(false), 1200); }}>{copiedID ? <Icon name="check" /> : <Icon name="copy" />}</button></h1><div className="box-meta"><StateBadge state={sandbox.state}/><span>{sizeLabel(sandbox.type)}</span><span>{detailAutoStopLabel(sandbox, now)}</span>{sandbox.idleStopSeconds ? <span>{idleStopLabel(sandbox.idleStopSeconds)}</span> : null}<button className="text-link" onClick={() => { setTimerSelection(sandbox.autoStop === false ? 'always' : String(sandbox.ttlSeconds ?? 3600)); setTimerDialog(true); }}>Change</button></div></> : <h1>{heading.title}</h1>}</div><div className="heading-actions">{view === 'sandbox' && sandbox && <><button className="button button-primary" onClick={() => changeTab('Desktop')}>Open desktop</button><button className="button button-secondary" onClick={() => void sandboxAction(sandbox.state === 'stopped' ? 'resume' : 'stop')}>{sandbox.state === 'stopped' ? 'Resume' : 'Stop'}</button><button className="button button-secondary danger-link" onClick={() => setConfirmDelete(true)}>Delete</button></>}{view === 'sandboxes' && <button className="button button-primary" onClick={() => setDialog(true)}><Icon name="plus" size={14} />New sandbox</button>}{view === 'templates' && <button className="button button-primary" onClick={() => setDialog(true)}><Icon name="plus" size={14} />Save template</button>}{view === 'environments' && <button className="button button-primary" onClick={() => setDialog(true)}><Icon name="plus" size={14} />New environment</button>}{view === 'api-keys' && <button className="button button-primary" onClick={() => setDialog(true)}><Icon name="plus" size={14} />Create key</button>}{view === 'webhooks' && <button className="button button-primary" onClick={() => setDialog(true)}><Icon name="plus" size={14} />Add endpoint</button>}</div></div>

          {problem && <div className="notice notice-error" role="status"><div><strong>Connection problem</strong><small>{problem}</small></div><button aria-label="Dismiss" onClick={() => setProblem('')}>Close</button></div>}
          {createdKey && <div className="created-secret"><p>Copy this key now. It will not be shown again.</p><code>{createdKey}</code><button className="button button-secondary" onClick={() => void navigator.clipboard?.writeText(createdKey)}>Copy key</button></div>}
          {rotatedSecret && <div className="created-secret"><p>Copy this signing secret now. It will not be shown again.</p><code>{rotatedSecret}</code><button className="button button-secondary" onClick={() => void navigator.clipboard?.writeText(rotatedSecret)}>Copy secret</button><button className="text-link" onClick={() => setRotatedSecret('')}>Close</button></div>}
          {notice && <div className="toast" role="status">{notice}</div>}
          {view === 'sandboxes' && <SandboxList items={items as Sandbox[]} loading={loading} now={now} filter={sandboxFilter} onFilter={setSandboxFilter} onCreate={() => setDialog(true)} />}
          {view === 'sandbox' && <SandboxDetail sandbox={sandbox} loading={loading} now={now} tab={tab} setTab={changeTab} desktopURL={desktopURL} setDesktopURL={setDesktopURL} desktopExpiresAt={desktopExpiresAt} setDesktopExpiresAt={setDesktopExpiresAt} openDesktop={openDesktop} detailId={detailId} items={items} refresh={refresh} action={sandboxAction} onDelete={() => setConfirmDelete(true)} desktopControl={Boolean(desktopControl[detailId])} setDesktopControl={(value) => setDesktopControl((current) => ({ ...current, [detailId]: value }))} />}
          {view === 'templates' && <ResourceList loading={loading} kind="templates" items={items} emptyTitle="No templates yet" emptyText="Save a named snapshot to reuse it." onCreate={() => setDialog(true)} action="Save template" />}
          {view === 'environments' && <EnvironmentList loading={loading} items={items} onCreate={() => setDialog(true)} />}
          {view === 'api-keys' && <APIKeyList loading={loading} items={items} onCreate={() => setDialog(true)} onDelete={async (id) => { try { await api(`/api-keys/${encodeURIComponent(id)}`, { method: 'DELETE' }); flash('Key revoked.'); await refresh(); } catch (cause) { setProblem(cause instanceof Error ? cause.message : 'Key could not be revoked.'); } }} />}
          {view === 'webhooks' && <ResourceList loading={loading} kind="webhooks" items={items} emptyTitle="No webhooks yet" emptyText="Add an endpoint to receive sandbox events." onCreate={() => setDialog(true)} action="Add endpoint" onRotate={rotateWebhook} />}
          {view === 'settings' && <SettingsPanel loading={loading} account={items[0]} origin={origin} storage={snapshotStorage} />}
        </div>
      </main>

      {dialog && <Dialog onClose={() => setDialog(false)} title={view === 'sandboxes' ? 'Create a sandbox' : view === 'templates' ? 'Save a template' : view === 'environments' ? 'Create an environment' : view === 'api-keys' ? 'Create an API key' : view === 'webhooks' ? 'Add a webhook' : 'New'}>
        {view === 'sandboxes' && <form className="form-stack" onSubmit={createSandbox}><p className="muted">A fresh Linux machine, ready in a few moments.</p><label>Machine size<select name="type" defaultValue="default"><option value="xsmall">XSmall / 1 CPU / 2 GB</option><option value="small">Small / 2 CPU / 4 GB</option><option value="default">Default / 4 CPU / 8 GB</option><option value="large">Large / 8 CPU / 16 GB</option><option value="xlarge">XLarge / 16 CPU / 32 GB</option></select></label><label>Auto-stop<select name="ttlPreset" value={createTTL} onChange={(event) => setCreateTTL(event.target.value)}><option value="3600">1 hour / task default</option><option value="14400">4 hours</option><option value="86400">24 hours</option><option value="custom">Custom</option><option value="always">Always on</option></select></label>{createTTL === 'custom' && <label>Timer duration <span className="optional">(seconds, 60-2592000)</span><input name="customTTL" type="number" min="60" max="2592000" defaultValue="3600" required /></label>}<label>Idle stop <span className="optional">(seconds, optional; minimum 300)</span><input name="idleStop" type="number" min="300" max="2592000" /></label><DialogActions onCancel={() => setDialog(false)} submit="Create sandbox" /></form>}
        {view === 'templates' && <form className="form-stack" onSubmit={createTemplate}><p className="muted">Pin a snapshot as a reusable starting point.</p><label>Template name<input name="name" placeholder="python-starter" required /></label><label>Snapshot ID<input name="snapshotId" placeholder="sn_..." required /></label><DialogActions onCancel={() => setDialog(false)} submit="Save template" /></form>}
        {view === 'environments' && <form className="form-stack" onSubmit={createEnvironment}><p className="muted">Add shared configuration and a first-boot setup script. Keep secrets in your encrypted environment configuration.</p><label>Environment name<input name="name" placeholder="web-app" required /></label><label>Variables <span className="optional">(one KEY=value per line)</span><textarea name="vars" rows={4} placeholder={'NODE_ENV=development\nPORT=3000'} /></label><label>Setup script <span className="optional">(optional)</span><textarea name="setupScript" rows={5} placeholder="npm install" /></label><DialogActions onCancel={() => setDialog(false)} submit="Create environment" /></form>}
        {view === 'api-keys' && <form className="form-stack" onSubmit={createKey}><p className="muted">The secret is shown once. Store it somewhere safe.</p><label>Key name<input name="name" placeholder="CI deploy key" required /></label><div className="field-note"><span className="status-dot" /> Full access <span>/</span> Never expires</div><DialogActions onCancel={() => setDialog(false)} submit="Create key" /></form>}
        {view === 'webhooks' && <form className="form-stack" onSubmit={createWebhook}><p className="muted">We'll sign each delivery with an HMAC-SHA256 signature.</p><label>Endpoint URL<input name="url" type="url" placeholder="https://example.com/hooks/boxes" required /></label><div className="event-chips"><span>sandbox.ready</span><span>sandbox.restored</span><span>sandbox.error</span><span>sandbox.stopped</span><span>sandbox.deleted</span></div><DialogActions onCancel={() => setDialog(false)} submit="Create webhook" /></form>}
      </Dialog>}
      {timerDialog && <Dialog onClose={() => setTimerDialog(false)} title="Set auto-stop timer"><form className="form-stack" onSubmit={setSandboxTimer}><p className="muted">Choose how long this sandbox should remain available from now.</p><label>Auto-stop<select name="ttlPreset" value={timerSelection} onChange={(event) => setTimerSelection(event.target.value)}><option value="3600">1 hour</option><option value="14400">4 hours</option><option value="86400">24 hours</option><option value="always">Always on</option></select></label><DialogActions onCancel={() => setTimerDialog(false)} submit="Save" /></form><form className="form-stack" onSubmit={updateIdleStop}><label>Idle stop <span className="optional">(seconds, 300–2592000; blank turns off)</span><input name="idleStop" type="number" min="300" max="2592000" defaultValue={sandbox?.idleStopSeconds ?? ""} /></label><DialogActions onCancel={() => setTimerDialog(false)} submit="Save idle stop" /></form></Dialog>}
      {confirmDelete && <Dialog onClose={() => setConfirmDelete(false)} title="Delete sandbox?"><p className="muted">Delete this sandbox permanently? Its container and saved snapshots will be removed.</p><div className="dialog-actions"><button className="button button-secondary" onClick={() => setConfirmDelete(false)}>Cancel</button><button className="button button-danger-outline" onClick={() => { setConfirmDelete(false); void sandboxAction('delete'); }}>Delete sandbox</button></div></Dialog>}
    </div>
  );
}

function SandboxList({ items, loading, now, filter, onFilter, onCreate }: { items: Sandbox[]; loading: boolean; now: number; filter: 'all' | 'r' | 's'; onFilter: (filter: 'all' | 'r' | 's') => void; onCreate: () => void }) {
  return <>
    <p className="page-summary">{loading ? 'Loading sandboxes...' : `${items.filter((box) => ['ready', 'idle'].includes(box.state)).length} running, ${items.filter((box) => box.state === 'stopped').length} stopped`}</p>
    <div className="list-controls"><div className="sandbox-filter-tabs" role="tablist" aria-label="Sandbox state filter">{([['r', 'Running'], ['s', 'Stopped'], ['all', 'All']] as const).map(([value, label]) => <button key={value} type="button" role="tab" aria-selected={filter === value} className={filter === value ? 'selected' : ''} onClick={() => onFilter(value)}>{label}</button>)}</div></div>
      <div className="table-scroll"><table><thead><tr><th>Name / ID</th><th>State</th><th>Size</th><th>Auto-stop</th><th /></tr></thead><tbody>
        {!loading && items.map((box) => <tr key={box.id}><td><Link className="sandbox-link" href={`/sandbox/?id=${encodeURIComponent(box.id)}`}><span><strong className="mono">{box.id}</strong><small>Created {formatDate(box.createdAt)}</small></span></Link></td><td><StateBadge state={box.state} /></td><td><span className="size-cell">{sizeLabel(box.type)}</span></td><td><span className="auto-stop-label">{autoStopLabel(box, now)}</span></td><td><Link className="row-arrow" href={`/sandbox/?id=${encodeURIComponent(box.id)}`} aria-label={`Open ${box.id}`}></Link></td></tr>)}
        {!loading && items.length === 0 && <tr><td colSpan={5}><EmptyState text="Create a sandbox to get started." /></td></tr>}
        {loading && <tr><td colSpan={5}><LoadingState /></td></tr>}
      </tbody></table></div>
  </>;
}


function durationLabel(ms?: number) {
  if (ms == null) return '';
  if (ms < 1000) return `${ms} ms`;
  const seconds = Math.round(ms / 1000);
  return seconds < 60 ? `${seconds} sec` : `${Math.floor(seconds / 60)} min${seconds % 60 ? ` ${seconds % 60} sec` : ''}`;
}
function runningLabel(seconds: number) {
  const hours = Math.floor(seconds / 3600), minutes = Math.floor((seconds % 3600) / 60);
  return `${hours} h ${minutes} min`;
}
function ActivityChart({ days }: { days: Usage['perDay'] }) {
  const values = days.slice(-30); const max = Math.max(1, ...values.map((day) => day.runningSeconds / 3600));
  const width = 720, height = 132, left = 42, right = 8, top = 12, bottom = 25;
  const slot = (width - left - right) / Math.max(1, values.length), barWidth = Math.max(2, slot * .68);
  const shortDate = (date: string) => new Date(`${date}T00:00:00`).toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
  return <svg className="activity-chart" viewBox={`0 0 ${width} ${height}`} role="img" aria-label="Running hours per day for the last 30 days">
    <line x1={left} x2={width-right} y1={height-bottom} y2={height-bottom} stroke="var(--line)" />
    <text x="0" y={top+4}>{max.toFixed(max % 1 ? 1 : 0)} h</text>
    {values.map((day, index) => { const hrs = day.runningSeconds / 3600, barHeight = hrs / max * (height-top-bottom); return <g key={day.date}><rect x={left+index*slot+(slot-barWidth)/2} y={height-bottom-barHeight} width={barWidth} height={Math.max(0,barHeight)} fill="var(--ink)" fillOpacity=".7"><title>{`${shortDate(day.date)}: ${hrs.toFixed(1)} running hours`}</title></rect>{(index === 0 || index === values.length-1) && <text x={left+index*slot} y={height-5}>{shortDate(day.date)}</text>}</g>; })}
  </svg>;
}
function ActivityPanel({ usage, items, loading, category, setCategory, cursor, onMore }: { usage: Usage | null; items: ActivityItem[]; loading: boolean; category: string; setCategory: (value: string) => void; cursor: string; onMore: () => void }) {
  const filters = [['','All'],['lifecycle','Lifecycle'],['commands','Commands'],['agents','Agents'],['access','Access']];
  return <section className="activity-panel">
    <SectionHeader title="Usage" />
    <p className="usage-line">{usage ? `Running ${runningLabel(usage.runningSecondsThisMonth)} this month${usage.estimatedCost ? `, estimated ${new Intl.NumberFormat(undefined,{style:'currency',currency:usage.estimatedCost.currency}).format(usage.estimatedCost.amount)}` : ''}` : 'Usage summary unavailable.'}</p>
    {usage && <ActivityChart days={usage.perDay ?? []} />}
    {usage && <p className="usage-counts">{usageCountsLabel(usage)}</p>}
    <div className="activity-filter" role="tablist" aria-label="Activity category">{filters.map(([value,label]) => <button type="button" role="tab" aria-selected={category === value} className={category === value ? 'selected' : ''} key={value} onClick={() => setCategory(value)}>{label}</button>)}</div>
    <div className="table-scroll"><table><thead><tr><th>Time</th><th>Actor</th><th>Action</th><th>Result</th></tr></thead><tbody>{items.map((item) => { const presentation = activityPresentation(item); const failed = presentation.result === 'Failed' || /error|fail/i.test(item.outcome ?? ''); return <tr key={item.id}><td><time title={new Date(item.time).toLocaleString()}>{relativeTime(item.time)}</time></td><td>{item.actorType === 'api_key' ? <code>{item.actorLabel}</code> : item.actorLabel}</td><td>{presentation.code ? <>{presentation.text}<code>{presentation.code}</code>{presentation.suffix}</> : presentation.text}</td><td>{item.outcome || presentation.result ? <><span className={`activity-result ${failed ? 'error' : 'ok'}`}>{failed ? 'Failed' : 'Succeeded'}</span>{item.durationMs != null && <small className="activity-duration">{durationLabel(item.durationMs)}</small>}</> : '—'}</td></tr>; })}{!loading && items.length === 0 && <tr><td colSpan={4}>No activity yet.</td></tr>}{loading && items.length === 0 && <tr><td colSpan={4}><LoadingState /></td></tr>}</tbody></table></div>
    {cursor && <div className="activity-more"><button className="button button-secondary" disabled={loading} onClick={onMore}>{loading ? 'Loading…' : 'Load more'}</button></div>}
  </section>;
}

function SandboxDetail({ sandbox, loading, now, tab, setTab, desktopURL, setDesktopURL, desktopExpiresAt, setDesktopExpiresAt, openDesktop, detailId, items, refresh, action, onDelete, desktopControl, setDesktopControl }: { sandbox: Sandbox | null; loading: boolean; now: number; tab: Tab; setTab: (tab: Tab) => void; desktopURL: string; setDesktopURL: (url: string) => void; desktopExpiresAt: number; setDesktopExpiresAt: (expiry: number) => void; openDesktop: () => Promise<boolean>; detailId: string; items: unknown[]; refresh: () => Promise<void>; action: (action: 'stop' | 'resume' | 'delete') => void; onDelete: () => void; desktopControl: boolean; setDesktopControl: (value: boolean) => void }) {
  const tabs: Tab[] = ['Overview', 'Activity', 'Desktop', 'Snapshots', 'Hosts'];
  const [hostPort, setHostPort] = useState('3000');
  const [hosts, setHosts] = useState<unknown[]>([]);
  const [snapshots, setSnapshots] = useState<Snapshot[]>([]);
  const [tree, setTree] = useState<unknown[]>([]);
  const [filePreview, setFilePreview] = useState('');
  const [hostError, setHostError] = useState('');
  const [tabLoading, setTabLoading] = useState(false);
  const [selectedSnapshot, setSelectedSnapshot] = useState('');
  const [desktopDisconnected, setDesktopDisconnected] = useState(false);
  const [desktopLoaded, setDesktopLoaded] = useState(false);
  const [desktopRenewing, setDesktopRenewing] = useState(false);
  const [desktopReload, setDesktopReload] = useState(0);
  const [activity, setActivity] = useState<ActivityItem[]>([]);
  const [activityCursor, setActivityCursor] = useState('');
  const [activityCategory, setActivityCategory] = useState('');
  const [activityLoading, setActivityLoading] = useState(false);
  const [usage, setUsage] = useState<Usage | null>(null);
  const desktopFrame = useRef<HTMLIFrameElement>(null);

  useEffect(() => {
    if (!detailId || !sandbox || !['Snapshots', 'Hosts'].includes(tab)) return;
    const path = `/sandboxes/${encodeURIComponent(detailId)}/${tab === 'Snapshots' ? 'snapshots' : 'hosts'}`;
    let alive = true;
    setTabLoading(true);
    setHostError('');
    void api<unknown>(path, { cache: 'no-store' }).then((result) => {
      if (!alive) return;
      if (tab === 'Snapshots') setSnapshots(listFrom<Snapshot>(result, 'snapshots'));
      else setHosts(listFrom<unknown>(result, 'hosts'));
    }).catch((cause: unknown) => { if (alive) setHostError(cause instanceof Error ? cause.message : 'Unable to load this section.'); })
      .finally(() => { if (alive) setTabLoading(false); });
    return () => { alive = false; };
  }, [detailId, tab, sandbox]);

  useEffect(() => {
    if (tab !== 'Activity' || !detailId) return;
    let alive = true;
    setActivityLoading(true);
    const query = new URLSearchParams({ limit: '50' });
    if (activityCategory) query.set('category', activityCategory);
    void Promise.all([
      api<{ items?: ActivityItem[]; nextCursor?: string }>(`/sandboxes/${encodeURIComponent(detailId)}/activity?${query}`, { cache: 'no-store' }),
      api<Usage>(`/sandboxes/${encodeURIComponent(detailId)}/usage`, { cache: 'no-store' }),
    ]).then(([result, usageResult]) => { if (alive) { setActivity(result.items ?? []); setActivityCursor(result.nextCursor ?? ''); setUsage(usageResult); } })
      .catch(() => { if (alive) { setActivity([]); setUsage(null); } })
      .finally(() => { if (alive) setActivityLoading(false); });
    return () => { alive = false; };
  }, [tab, detailId, activityCategory]);

  async function loadMoreActivity() {
    if (!activityCursor || activityLoading) return;
    setActivityLoading(true);
    try {
      const query = new URLSearchParams({ before: activityCursor, limit: '50' });
      if (activityCategory) query.set('category', activityCategory);
      const result = await api<{ items?: ActivityItem[]; nextCursor?: string }>(`/sandboxes/${encodeURIComponent(detailId)}/activity?${query}`, { cache: 'no-store' });
      setActivity((current) => [...current, ...(result.items ?? [])]); setActivityCursor(result.nextCursor ?? '');
    } finally { setActivityLoading(false); }
  }

  const openDesktopRef = useRef(openDesktop);
  openDesktopRef.current = openDesktop;
  const connectDesktop = () => {
    setDesktopDisconnected(false); setDesktopLoaded(false);
    void (async () => {
      try {
        for (let attempt = 0; attempt < 5; attempt++) {
          const ready = await openDesktopRef.current();
          if (ready) return;
          if (attempt === 4) return;
          setDesktopLoaded(false);
          await new Promise((resolve) => window.setTimeout(resolve, 1000));
          setDesktopReload((value) => value + 1);
        }
      } catch { setDesktopDisconnected(true); }
    })();
  };
  useEffect(() => { if (tab === 'Desktop' && !desktopURL && !desktopDisconnected) connectDesktop(); }, [tab, detailId]);
  useEffect(() => {
    if (!desktopURL || !desktopExpiresAt) return;
    const timer = window.setInterval(() => {
      if (desktopExpiresAt - Date.now() > 30 * 60 * 1000 || desktopRenewing) return;
      setDesktopRenewing(true);
      void (async () => {
        try {
          for (let attempt = 0; attempt < 5; attempt++) {
            const result = await api<{url?:string; expiresAt?:string; ready?:boolean}>(`/sandboxes/${encodeURIComponent(detailId)}/desktop`, { method:'POST', body:jsonBody({renewal:true}) });
            if (result.url && new URL(result.url).searchParams.get('token') !== new URL(desktopURL).searchParams.get('token')) { setDesktopLoaded(false); setDesktopURL(result.url); }
            if (result.expiresAt) setDesktopExpiresAt(Date.parse(result.expiresAt));
            if (result.ready !== false || attempt === 4) return;
            setDesktopLoaded(false);
            await new Promise((resolve) => window.setTimeout(resolve, 1000));
            setDesktopReload((value) => value + 1);
          }
        } catch { setDesktopDisconnected(true); }
        finally { setDesktopRenewing(false); }
      })();
    }, 30_000);
    return () => window.clearInterval(timer);
  }, [desktopURL, desktopExpiresAt, desktopRenewing, detailId]);

  async function addHost(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    try {
      await api(`/sandboxes/${encodeURIComponent(detailId)}/hosts`, { method: 'POST', body: jsonBody({ port: Number(hostPort), private: false }) });
      setHostError(''); setHosts(await api<unknown[]>(`/sandboxes/${encodeURIComponent(detailId)}/hosts`));
    } catch (cause) { setHostError(cause instanceof Error ? cause.message : 'Port could not be exposed.'); }
  }

  async function loadTree(snapshotId: string) {
    setSelectedSnapshot(snapshotId);
    try { setTree(await api<unknown[]>(`/snapshots/${encodeURIComponent(snapshotId)}/tree`)); }
    catch (cause) { setHostError(cause instanceof Error ? cause.message : 'Snapshot files could not be loaded.'); }
  }

  async function loadFile(snapshotId: string, path: string) {
    try {
      const response = await apiResponse(`/snapshots/${encodeURIComponent(snapshotId)}/file?path=${encodeURIComponent(path)}`);
      setFilePreview(await response.text());
    } catch (cause) { setHostError(cause instanceof Error ? cause.message : 'File could not be read.'); }
  }

  if (loading) return <div className="detail-loading"><BrandMark size={24} animated /><span>Loading…</span></div>;
  if (!sandbox) return <div className="panel detail-loading"><EmptyState text="Return to your sandboxes and choose a machine to inspect." action={<Link className="button button-primary" href="/">Back to sandboxes</Link>} /></div>;

  return <>
    <div className="tab-bar" role="tablist" aria-label="Sandbox sections">{tabs.map((name) => <button key={name} role="tab" aria-selected={tab === name} className={tab === name ? 'selected' : ''} onClick={() => setTab(name)}>{name}</button>)}</div>
    {tab === 'Overview' && <section className="panel detail-card overview-panel"><dl className="detail-list"><div><dt>State</dt><dd><StateBadge state={sandbox.state} /></dd></div><div><dt>Size</dt><dd>{sizeLabel(sandbox.type)}{sizeSpecs(sandbox.type)}</dd></div><div><dt>Auto-stop</dt><dd>{autoStopLabel(sandbox, now)}</dd></div><div><dt>Last snapshot</dt><dd>{relativeTime(sandbox.lastSnapshotAt ?? sandbox.latestSnapshotAt)}</dd></div><div><dt>Created</dt><dd>{formatDate(sandbox.createdAt)}</dd></div></dl></section>}
    <section className={`panel desktop-panel ${tab !== 'Desktop' ? 'desktop-hidden' : ''}`} aria-hidden={tab !== 'Desktop'}><SectionHeader title="Desktop" actions={<>{desktopURL && <a className="button button-secondary button-small" href={desktopURL} target="_blank" rel="noreferrer"><Icon name="external-link" />Open in new tab</a>}<button className="button button-secondary button-small" onClick={() => { setDesktopDisconnected(false); setDesktopLoaded(false); connectDesktop(); }}>Reconnect</button></>} />{desktopDisconnected ? <div className="desktop-disconnected"><p>Desktop disconnected.</p><button className="button button-secondary" onClick={() => { setDesktopDisconnected(false); connectDesktop(); }}>Reconnect</button></div> : <><div className="desktop-control-bar"><span>{desktopControl ? 'You have control' : 'View only'}</span><button className="button button-secondary button-small" onClick={() => { const next = !desktopControl; setDesktopControl(next); if (next) desktopFrame.current?.focus(); }}>{desktopControl ? 'Release control' : 'Take control'}</button></div>{desktopURL && <div className="desktop-viewport"><iframe ref={desktopFrame} key={desktopReload} title="Sandbox desktop" src={desktopURL} onLoad={() => setDesktopLoaded(true)} onError={() => setDesktopDisconnected(true)} className="desktop-frame" tabIndex={desktopControl ? 0 : -1} allow="clipboard-read; clipboard-write; fullscreen" />{!desktopControl && <div className="desktop-view-overlay" tabIndex={0} aria-label="Desktop is view only" onKeyDown={(event) => event.preventDefault()} />}</div>}{(!desktopURL || !desktopLoaded) && <div className="desktop-loading"><LoadingState /></div>}</>}</section>
    {tab === 'Activity' && <ActivityPanel usage={usage} items={activity} loading={activityLoading} category={activityCategory} setCategory={setActivityCategory} cursor={activityCursor} onMore={loadMoreActivity} />}

    {tab === 'Snapshots' && <section className="resource-panel"><SectionHeader title="Snapshots" description="Browse files or open a previous state." actions={<button className="button button-secondary button-small" onClick={() => void refresh()}>Refresh</button>} />{hostError && <p className="form-error">{hostError}</p>}{tabLoading ? <LoadingState /> : <div className="table-scroll"><table><thead><tr><th>Snapshot</th><th>Size</th><th>Created</th><th /></tr></thead><tbody>{snapshots.map((snapshot, index) => { const id = snapshot.snapshotId ?? snapshot.id ?? ''; return <tr key={id || index}><td><code>{id || '-'}</code></td><td>{formatBytes(snapshot.sizeBytes)}</td><td>{formatDate(snapshot.createdAt)}</td><td><button className="text-link" onClick={() => void loadTree(id)}>Browse files</button></td></tr>; })}{snapshots.length === 0 && <tr><td colSpan={4}><EmptyState text="Snapshots are saved as your machine works." /></td></tr>}</tbody></table></div>}{selectedSnapshot && <div className="snapshot-browser"><h3>Snapshot files <code>{selectedSnapshot}</code></h3><div className="file-tree">{tree.map((entry, index) => { const item = entry as { path?: string; size?: number; type?: string }; return <button key={item.path ?? index} onClick={() => void loadFile(selectedSnapshot, item.path ?? '')}><span>{item.type === 'dir' ? 'Directory' : 'File'}</span><code>{item.path}</code><small>{formatBytes(item.size)}</small></button>; })}{tree.length === 0 && <p className="muted">No files in this snapshot.</p>}</div>{filePreview && <pre className="code-block file-preview">{filePreview}</pre>}</div>}</section>}
    {tab === 'Hosts' && <section className="resource-panel"><SectionHeader title="Hosted ports" description="Expose an app running inside this sandbox." />{hostError && <p className="form-error">{hostError}</p>}<InlineForm onSubmit={addHost}><label>Container port<input value={hostPort} onChange={(event) => setHostPort(event.target.value)} inputMode="numeric" /></label><button className="button button-primary" type="submit"><Icon name="plus" size={14} />Expose port</button></InlineForm><div className="table-scroll"><table><thead><tr><th>Container port</th><th>Host</th><th>Access</th><th /></tr></thead><tbody>{hosts.map((entry, index) => { const item = entry as { port?: number; host?: string; private?: boolean }; return <tr key={item.port ?? index}><td>:{item.port}</td><td className="mono">{item.host ?? '-'}</td><td>{item.private ? 'Private' : 'Public'}</td><td>{item.host?.startsWith('http') && <a href={item.host} target="_blank" rel="noreferrer"><Icon name="external-link" />Open</a>}</td></tr>; })}{hosts.length === 0 && <tr><td colSpan={4}>Add a port to make your app reachable over HTTPS.</td></tr>}</tbody></table></div></section>}
  </>;
}

function EnvironmentList({ loading, items, onCreate }: { loading: boolean; items: unknown[]; onCreate: () => void }) {
  return <ResourceList loading={loading} kind="environments" items={items} emptyTitle="No environments yet" emptyText="Create an environment to share variables and setup scripts." onCreate={onCreate} action="New environment" />;
}

function APIKeyList({ loading, items, onCreate, onDelete }: { loading: boolean; items: unknown[]; onCreate: () => void; onDelete: (id: string) => void }) {
  return <section className="resource-panel"><SectionHeader title="API keys" /><div className="table-scroll"><table><thead><tr><th>Name</th><th>Secret</th><th>Created</th><th>Expires</th><th>Usage</th><th /></tr></thead><tbody>
    {items.map((raw, index) => {
      const key = raw as { id?: string; name?: string; createdAt?: string; expiresAt?: string; scopes?: string[]; usage?: { requests?: number; sandboxesCreated?: number } };
      return <tr key={key.id ?? index}><td><strong>{key.name ?? 'Unnamed key'}</strong><small>{key.scopes?.join(', ') ?? '-'}</small></td><td>Shown once at creation</td><td>{formatDate(key.createdAt)}</td><td>{formatDate(key.expiresAt)}</td><td>{(key.usage?.requests ?? 0).toLocaleString()} requests · {key.usage?.sandboxesCreated ?? 0} boxes, last 30 days</td><td><button className="text-link danger-link" onClick={() => key.id && onDelete(key.id)}>Revoke</button></td></tr>;
    })}
    {!loading && items.length === 0 && <tr><td colSpan={6}><EmptyState text="Create an API key to get started." /></td></tr>}
    {loading && items.length === 0 && <tr><td colSpan={6}><LoadingState /></td></tr>}
  </tbody></table></div></section>;
}

function ResourceList({ loading, kind, items, emptyTitle, emptyText, onCreate, action, onRotate }: { loading: boolean; kind: 'templates' | 'environments' | 'webhooks'; items: unknown[]; emptyTitle: string; emptyText: string; onCreate: () => void; action: string; onRotate?: (id: string) => void }) {
  const columns = kind === 'templates' ? ['Name', 'Snapshot', 'Created'] : kind === 'environments' ? ['Name', 'Current version', 'Updated'] : ['Endpoint', 'Events', 'Status'];
  const emptySentence = kind === 'templates' ? 'Save a template to reuse a snapshot.' : kind === 'environments' ? 'Create an environment to share configuration.' : 'Add an endpoint to receive sandbox events.';
  return <section className="resource-panel"><SectionHeader title={kind === 'templates' ? 'Templates' : kind === 'environments' ? 'Environments' : 'Webhooks'} /><div className="table-scroll"><table><thead><tr>{columns.map((column) => <th key={column}>{column}</th>)}<th /></tr></thead><tbody>{items.map((raw, index) => { const item = raw as Record<string, unknown>; return <tr key={String(item.id ?? item.name ?? index)}><td><div className="table-primary"><span><strong>{String(item.name ?? item.url ?? 'Unnamed')}</strong><small>{String(item.id ?? '-')}</small></span></div></td><td>{String(item.snapshotId ?? item.version ?? (Array.isArray(item.events) ? `${item.events.length} events` : '-'))}</td><td>{formatDate(String(item.createdAt ?? item.updatedAt ?? ''))}</td><td>{kind === 'webhooks' && onRotate ? <button className="text-link" onClick={() => onRotate(String(item.id))}>Rotate secret</button> : <button className="row-arrow" aria-label="Open details"></button>}</td></tr>; })}{!loading && items.length === 0 && <tr><td colSpan={columns.length + 1}><EmptyState text={emptySentence} /></td></tr>}{loading && items.length === 0 && <tr><td colSpan={columns.length + 1}><LoadingState /></td></tr>}</tbody></table></div></section>;
}

function SettingsPanel({ loading, account, origin, storage }: { loading: boolean; account: unknown; origin: string; storage: SnapshotStorage | null }) {
  const [machine, setMachine] = useState<Machine | null>(null);
  useEffect(() => { let alive = true; void api<{ machine: Machine }>('/machine', { cache: 'no-store' }).then((result) => { if (alive) setMachine(result.machine); }).catch(() => { if (alive) setMachine(null); }); return () => { alive = false; }; }, []);
  const user = account as { email?: string; role?: string; id?: string } | undefined;
  const sizes = ['xsmall','small','default','large','xlarge'];
  const [pricing,setPricing] = useState<Pricing>({currency:'USD',hourlyBySize:{xsmall:0,small:0,default:0,large:0,xlarge:0}});
  const [pricingMessage,setPricingMessage] = useState('');
  const [zeroRetention,setZeroRetention] = useState(false);
  const [retentionConfirm,setRetentionConfirm] = useState('');
  const [retentionMessage,setRetentionMessage] = useState('');
  useEffect(() => { if (user?.role === 'owner') void api<Pricing>('/settings/pricing',{cache:'no-store'}).then(setPricing).catch(()=>{}); },[user?.role]);
  useEffect(() => { if (user?.role === 'owner') void api<{enabled:boolean}>('/settings/retention',{cache:'no-store'}).then(value=>setZeroRetention(value.enabled)).catch(()=>{}); },[user?.role]);
  async function saveRetention(event: FormEvent<HTMLFormElement>) { event.preventDefault(); setRetentionMessage(''); try { const result=await api<{enabled:boolean}>('/settings/retention',{method:'PUT',body:jsonBody({enabled:zeroRetention,confirm:retentionConfirm})}); setZeroRetention(result.enabled); setRetentionConfirm(''); setRetentionMessage('Retention setting saved.'); } catch(cause) { setRetentionMessage(cause instanceof Error ? cause.message : 'Retention setting could not be saved.'); } }
  async function savePricing(event: FormEvent<HTMLFormElement>) {
    event.preventDefault(); setPricingMessage('');
    try { setPricing(await api<Pricing>('/settings/pricing',{method:'PUT',body:jsonBody(pricing)})); setPricingMessage('Pricing saved.'); }
    catch (e) { setPricingMessage(e instanceof Error ? e.message : 'Pricing could not be saved.'); }
  }
  const memoryUsed = machine ? Math.max(0, machine.memoryBytes - machine.memAvailableBytes) : 0;
  return <div className="settings-grid">
    <section className="panel detail-card settings-panel machine-panel"><SectionHeader title="Machine" actions={machine && <StateBadge state={machine.status} />} />{machine ? <dl className="detail-list machine-metrics"><div><dt>CPUs</dt><dd>{machine.cpus}</dd></div><div><dt>Memory</dt><dd>{formatBytes(memoryUsed)} used / {formatBytes(machine.memAvailableBytes)} available</dd></div><div><dt>Disk free</dt><dd>{formatBytes(machine.diskFreeBytes)}</dd></div><div><dt>Running boxes</dt><dd>{machine.runningBoxes}</dd></div><div><dt>Last seen</dt><dd>{formatDate(machine.lastSeen)}</dd></div></dl> : <p className="muted">Machine status {loading ? 'is loading…' : 'is unavailable.'}</p>}</section>
    <section className="panel detail-card settings-panel"><SectionHeader title="Account" /><dl className="detail-list"><div><dt>Email</dt><dd>{loading ? 'Loading...' : user?.email ?? '-'}</dd></div><div><dt>Role</dt><dd>{user?.role ?? '-'}</dd></div><div><dt>User ID</dt><dd className="mono">{user?.id ?? '-'}</dd></div></dl></section>
    <section className="panel detail-card settings-panel"><SectionHeader title="Server" /><dl className="detail-list"><div><dt>Dashboard URL</dt><dd className="mono">{origin || '-'}</dd></div><div><dt>API base</dt><dd className="mono">{origin ? `${origin}/api/v1` : '-'}</dd></div><div><dt>API keys</dt><dd><Link href="/api-keys/" className="text-link">Manage API keys</Link></dd></div></dl></section>
    {user?.role === 'owner' && <section className="panel detail-card settings-panel"><SectionHeader title="Data retention" /><form className="form-stack" onSubmit={saveRetention}><label><input type="checkbox" checked={zeroRetention} onChange={event=>setZeroRetention(event.target.checked)} /> Delete sandbox data when a box stops or is archived</label>{zeroRetention && <label>Type “delete archived sandbox data” to confirm<input value={retentionConfirm} onChange={event=>setRetentionConfirm(event.target.value)} /></label>}<small>Stopped boxes and their snapshots will be permanently deleted.</small>{retentionMessage && <small role="status">{retentionMessage}</small>}<button className="button button-primary" type="submit">Save retention</button></form></section>}
    {user?.role === 'owner' && <section className="panel detail-card settings-panel pricing-panel"><SectionHeader title="Pricing" /><form className="form-stack" onSubmit={savePricing}><label>Currency<select value={pricing.currency} onChange={e=>setPricing({...pricing,currency:e.target.value})}><option value="USD">USD</option><option value="EUR">EUR</option><option value="GBP">GBP</option><option value="MXN">MXN</option></select></label>{sizes.map(size=><label key={size}>{sizeLabel(size)} hourly price<input type="number" min="0" step="0.0001" value={pricing.hourlyBySize[size] ?? 0} onChange={e=>setPricing({...pricing,hourlyBySize:{...pricing.hourlyBySize,[size]:Number(e.target.value)}})} /></label>)}<small>Used only to estimate costs in Activity.</small>{pricingMessage && <small role="status">{pricingMessage}</small>}<button className="button button-primary" type="submit">Save</button></form></section>}
    {user?.role === 'owner' && storage && <section className="panel detail-card settings-panel"><SectionHeader title="Snapshot storage" /><dl className="detail-list"><div><dt>Type</dt><dd>{storage.type === 's3' ? 'S3-compatible' : 'Local disk'}</dd></div>{storage.endpoint && <div><dt>Endpoint</dt><dd className="mono">{storage.endpoint}</dd></div>}{storage.bucket && <div><dt>Bucket</dt><dd className="mono">{storage.bucket}</dd></div>}{storage.prefix && <div><dt>Prefix</dt><dd className="mono">{storage.prefix}</dd></div>}</dl></section>}
  </div>;
}

function StateBadge({ state, label }: { state: string; label?: string }) {
  const normalized = state.toLowerCase();
  return <span className={`state-badge state-${normalized}`}><i />{label ?? state.slice(0, 1).toUpperCase() + state.slice(1).toLowerCase()}</span>;
}

function EmptyState({ text, action }: { text: string; action?: ReactNode }) {
  return <div className="empty-state"><p>{text}</p>{action}</div>;
}

function Dialog({ title, children, onClose }: { title: string; children: ReactNode; onClose: () => void }) {
  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => { if (event.key === 'Escape') onClose(); };
    window.addEventListener('keydown', onKeyDown);
    return () => window.removeEventListener('keydown', onKeyDown);
  }, [onClose]);
  return <div className="dialog-backdrop" role="presentation" onMouseDown={(event) => { if (event.target === event.currentTarget) onClose(); }}><section className="dialog" role="dialog" aria-modal="true" aria-labelledby="dialog-title"><div className="dialog-heading"><div><h2 id="dialog-title">{title}</h2></div><button className="icon-button" aria-label="Close dialog" onClick={onClose}><Icon name="close" /></button></div>{children}</section></div>;
}

function DialogActions({ onCancel, submit }: { onCancel: () => void; submit: string }) {
  return <div className="dialog-actions"><button className="button button-secondary" type="button" onClick={onCancel}>Cancel</button><button className="button button-primary" type="submit">{ /^(Create|Add|New)\b/.test(submit) && <Icon name="plus" size={14} />}{submit}</button></div>;
}
