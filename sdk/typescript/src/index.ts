export type JSONValue = null | boolean | number | string | JSONValue[] | { [key: string]: JSONValue };
export interface Sandbox { id: string; type?: string; state: string; autoStop?: boolean; ttlSeconds?: number | null; idleStopSeconds?: number | null; expiresAt?: string | null; ttlRemainingSeconds?: number | null; lastSnapshotAt?: string | null; desktopAvailable?: boolean; runningSeconds?: number; setupStatus?: string; [key: string]: unknown }
export interface Machine { cpus: number; memoryBytes: number; memAvailableBytes: number; diskFreeBytes: number; swapFreeBytes: number; memoryPressureSomeAvg10: number; status: 'healthy' | 'unhealthy'; runningBoxes: number; lastSeen: string }
export interface Snapshot { id: string; boxId: string; resticId: string; sizeBytes: number; createdAt: string; [key: string]: unknown }
export interface Operation { id: string; status: 'pending' | 'done' | 'failed'; error?: string }
export interface CommandResult { processId?: string; running?: boolean; exitCode?: number | null; [key: string]: unknown }
export interface ActivityItem { id: string; time: string; actorType: string; actorLabel: string; category: string; action: string; detail: Record<string, unknown>; durationMs?: number; outcome?: string }
export interface BoxesOptions { baseUrl: string; apiKey: string; fetch?: typeof fetch; pollIntervalMs?: number }
export class BoxesError extends Error {
  constructor(public status: number, public code: string, message: string) { super(message); this.name = 'BoxesError'; }
}
type Query = Record<string, string | number | boolean | undefined | null>;
type Row = Record<string, any>;

export class Boxes {
  private readonly base: string;
  private readonly key: string;
  private readonly fetcher: typeof fetch;
  private readonly pollMs: number;

  constructor(options: BoxesOptions) {
    this.base = options.baseUrl.replace(/\/$/, '') + '/api/v1';
    this.key = options.apiKey;
    this.fetcher = options.fetch ?? fetch;
    this.pollMs = options.pollIntervalMs ?? 500;
  }

  private id(value: string): string { return encodeURIComponent(value); }
  private query(query?: Query): string {
    const params = new URLSearchParams();
    for (const [key, value] of Object.entries(query ?? {})) if (value !== undefined && value !== null) params.set(key, String(value));
    const encoded = params.toString(); return encoded ? `?${encoded}` : '';
  }
  private async request<T>(method: string, path: string, body?: unknown, headers: Record<string, string> = {}): Promise<T> {
    const requestHeaders = new Headers(headers);
    requestHeaders.set('Authorization', `Bearer ${this.key}`);
    const isStream = typeof ReadableStream !== 'undefined' && body instanceof ReadableStream;
    const isBinary = body instanceof Uint8Array || body instanceof ArrayBuffer || body instanceof Blob || isStream;
    let payload: BodyInit | undefined;
    if (body !== undefined) {
      if (typeof body === 'string' || isBinary || body instanceof FormData || body instanceof URLSearchParams) payload = body as BodyInit;
      else { requestHeaders.set('Content-Type', 'application/json'); payload = JSON.stringify(body); }
    }
    const response = await this.fetcher(this.base + path, {
      method, headers: requestHeaders, body: payload,
      ...(isStream ? { duplex: 'half' } as RequestInit : {}),
    });
    if (!response.ok) {
      let error: Row = {};
      try { error = await response.json() as Row; } catch { /* empty or non-JSON response */ }
      throw new BoxesError(response.status, error?.error?.code ?? 'http_error', error?.error?.message ?? response.statusText);
    }
    if (response.status === 204) return undefined as T;
    return (response.headers.get('content-type')?.includes('json') ? await response.json() : await response.arrayBuffer()) as T;
  }
  private async pollOperation(operation: Operation): Promise<Operation> {
    while (operation.status === 'pending') {
      await new Promise(resolve => setTimeout(resolve, this.pollMs));
      operation = (await this.request<{ operation: Operation }>('GET', `/operations/${this.id(operation.id)}`)).operation;
    }
    if (operation.status === 'failed') throw new BoxesError(500, 'operation_failed', operation.error ?? 'Operation failed');
    return operation;
  }

  machine = async () => (await this.request<{ machine: Machine }>('GET', '/machine')).machine;
  sandboxes = {
    create: (input: Record<string, unknown> = {}, idempotencyKey?: string) => this.request<Sandbox>('POST', '/sandboxes', input, idempotencyKey ? { 'Idempotency-Key': idempotencyKey } : {}),
    list: async (filter?: string) => (await this.request<{ sandboxes: Sandbox[] }>('GET', `/sandboxes${this.query({ filter })}`)).sandboxes,
    get: async (id: string) => (await this.request<{ sandbox: Sandbox }>('GET', `/sandboxes/${this.id(id)}`)).sandbox,
    update: async (id: string, input: Record<string, unknown>) => (await this.request<{ sandbox: Sandbox }>('PATCH', `/sandboxes/${this.id(id)}`, input)).sandbox,
    stop: (id: string) => this.request<Row>('POST', `/sandboxes/${this.id(id)}/stop`, {}),
    resume: async (id: string, input: Record<string, unknown> = {}) => {
      const result = await this.request<Row>('POST', `/sandboxes/${this.id(id)}/resume`, input);
      return (result.sandbox ?? result) as Sandbox;
    },
    fork: async (id: string, input: Record<string, unknown> = {}, idempotencyKey?: string) => {
      const result = await this.request<{ sandbox: Sandbox }>('POST', `/sandboxes/${this.id(id)}/fork`, input, idempotencyKey ? { 'Idempotency-Key': idempotencyKey } : {});
      return result.sandbox;
    },
    delete: async (id: string) => { const result = await this.request<{ operation?: Operation } | undefined>('DELETE', `/sandboxes/${this.id(id)}`); if (result?.operation) await this.pollOperation(result.operation); },
  };
  exec = {
    run: (id: string, cmd: string | string[], options: Record<string, unknown> = {}) => this.request<CommandResult>('POST', `/sandboxes/${this.id(id)}/commands`, { cmd, ...options }),
    detached: (id: string, cmd: string | string[], options: Record<string, unknown> = {}) => this.request<CommandResult>('POST', `/sandboxes/${this.id(id)}/commands`, { cmd, ...options, detached: true }),
    getCommand: (id: string, pid: string) => this.request<CommandResult>('GET', `/sandboxes/${this.id(id)}/commands/${this.id(pid)}`),
  };
  files = {
    read: (id: string, path: string) => this.request<ArrayBuffer>('GET', `/sandboxes/${this.id(id)}/files${this.query({ path })}`),
    write: (id: string, path: string, content: BodyInit) => this.request<void>('PUT', `/sandboxes/${this.id(id)}/files${this.query({ path })}`, content, { 'Content-Type': 'application/octet-stream' }),
  };
  push = (id: string, archive: BodyInit) => this.request<void>('PUT', `/sandboxes/${this.id(id)}/archive`, archive, { 'Content-Type': 'application/gzip' });
  pull = (id: string, path: string) => this.request<ArrayBuffer>('GET', `/sandboxes/${this.id(id)}/artifacts${this.query({ path })}`);
  prompt = (id: string, input: { prompt: string; provider: string; [key: string]: unknown }) => this.request<Row>('POST', `/sandboxes/${this.id(id)}/prompt`, input);
  events = async function* (this: Boxes, id: string, options: { after?: number; conversation?: string; signal?: AbortSignal } = {}): AsyncGenerator<Row> {
    let cursor = options.after ?? 0;
    while (!options.signal?.aborted) {
      const data = await this.request<{ events: Row[]; cursor: number }>('GET', `/sandboxes/${this.id(id)}/events${this.query({ after: cursor, wait: 30, conversation: options.conversation })}`);
      for (const event of data.events) { if (options.signal?.aborted) return; yield event; }
      cursor = Math.max(cursor, data.cursor ?? cursor);
    }
  };
  snapshots = {
    list: async (id: string) => (await this.request<{ snapshots: Snapshot[] }>('GET', `/sandboxes/${this.id(id)}/snapshots`)).snapshots,
    listAll: async (options: Omit<Query, 'cursor'> = {}) => {
      const snapshots: Snapshot[] = [];
      let cursor: string | undefined;
      do {
        const page = await this.request<{ snapshots: Snapshot[]; nextCursor: string }>('GET', `/snapshots${this.query({ ...options, cursor })}`);
        snapshots.push(...page.snapshots);
        cursor = page.nextCursor || undefined;
      } while (cursor);
      return snapshots;
    },
    latest: async (id: string) => (await this.request<{ snapshot: Snapshot | null }>('GET', `/sandboxes/${this.id(id)}/snapshots/latest`)).snapshot,
    delete: (id: string, snapshotId: string, force = false) => this.request<void>('DELETE', `/sandboxes/${this.id(id)}/snapshots/${this.id(snapshotId)}${this.query({ force: force || undefined })}`),
    pull: (snapshotId: string) => this.request<ArrayBuffer>('GET', `/snapshots/${this.id(snapshotId)}/download`),
  };
  namedSnapshots = {
    list: async () => (await this.request<{ namedSnapshots: Row[] }>('GET', '/named-snapshots')).namedSnapshots,
    create: async (input: Record<string, unknown>) => (await this.request<{ namedSnapshot: Row }>('POST', '/named-snapshots', input)).namedSnapshot,
    get: async (name: string) => (await this.request<{ namedSnapshot: Row }>('GET', `/named-snapshots/${this.id(name)}`)).namedSnapshot,
    delete: (name: string) => this.request<void>('DELETE', `/named-snapshots/${this.id(name)}`),
  };
  hosts = {
    expose: async (id: string, port: number, privateHost = false) => (await this.request<{ host: Row }>('POST', `/sandboxes/${this.id(id)}/hosts`, { port, private: privateHost })).host,
    list: async (id: string) => (await this.request<{ hosts: Row[] }>('GET', `/sandboxes/${this.id(id)}/hosts`)).hosts,
    remove: (id: string, port: number) => this.request<void>('DELETE', `/sandboxes/${this.id(id)}/hosts/${port}`),
  };
  desktop = { url: async (id: string) => (await this.request<{ url: string }>('GET', `/sandboxes/${this.id(id)}/desktop`)).url };
  usage = (id: string) => this.request<Row>('GET', `/sandboxes/${this.id(id)}/usage`);
  providerModels = () => this.request<{ providers: Row[] }>('GET', '/provider-models');
  environments = {
    list: async () => (await this.request<{ environments: Row[] }>('GET', '/environments')).environments,
    create: async (input: Record<string, unknown>) => (await this.request<{ environment: Row }>('POST', '/environments', input)).environment,
    get: async (id: string) => (await this.request<{ environment: Row }>('GET', `/environments/${this.id(id)}`)).environment,
    update: async (id: string, input: Record<string, unknown>) => (await this.request<{ environment: Row }>('PATCH', `/environments/${this.id(id)}`, input)).environment,
    delete: (id: string) => this.request<void>('DELETE', `/environments/${this.id(id)}`),
  };
  apiKeys = {
    list: async () => (await this.request<{ apiKeys: Row[] }>('GET', '/api-keys')).apiKeys,
    create: async (input: Record<string, unknown>) => (await this.request<{ apiKey: Row }>('POST', '/api-keys', input)).apiKey,
    delete: (id: string) => this.request<void>('DELETE', `/api-keys/${this.id(id)}`),
    usage: async (id: string) => (await this.request<{ usage: Row }>('GET', `/api-keys/${this.id(id)}/usage`)).usage,
  };
  webhooks = {
    list: async () => (await this.request<{ webhooks: Row[] }>('GET', '/webhooks')).webhooks,
    create: (input: Record<string, unknown>) => this.request<{ webhook: Row; secret: string }>('POST', '/webhooks', input),
    update: async (id: string, input: Record<string, unknown>) => (await this.request<{ webhook: Row }>('PATCH', `/webhooks/${this.id(id)}`, input)).webhook,
    delete: (id: string) => this.request<void>('DELETE', `/webhooks/${this.id(id)}`),
    rotateSecret: (id: string) => this.request<{ secret: string }>('POST', `/webhooks/${this.id(id)}/rotate-secret`, {}),
  };
}
