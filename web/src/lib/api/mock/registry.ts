// Route table of the dev mock layer. Handlers return the JSON body, or
// reply(status, body) for other status codes.
export interface MockCtx {
  method: string;
  /** Path below /api/v1, for example /changes/c-k7m2qz/impact. */
  path: string;
  params: Record<string, string>;
  query: URLSearchParams;
  body: any;
}

export class MockReply {
  constructor(
    public status: number,
    public body: unknown,
    public contentType = 'application/json',
  ) {}
}

/** Answer with a status code (errors use the Error schema). */
export function reply(status: number, body?: unknown, contentType?: string): MockReply {
  return new MockReply(status, body, contentType);
}

/** Error answer as the bench sends it. */
export function fail(status: number, error: string, detail?: string): MockReply {
  return new MockReply(status, detail ? { error, detail } : { error });
}

export type MockHandler = (ctx: MockCtx) => unknown | Promise<unknown>;

interface Route {
  method: string;
  re: RegExp;
  keys: string[];
  fn: MockHandler;
}

const routes: Route[] = [];

/** Register a handler; the pattern uses OpenAPI style params: /changes/{id}. */
export function on(method: string, pattern: string, fn: MockHandler) {
  const keys: string[] = [];
  const src = pattern.replace(/[.*+?^$()|[\]\\]/g, '\\$&').replace(/\{(\w+)\}/g, (_m, k: string) => {
    keys.push(k);
    return '([^/]+)';
  });
  // Later registrations win, so a file can override a shared handler.
  routes.unshift({ method: method.toUpperCase(), re: new RegExp(`^${src}$`), keys, fn });
}

export function match(method: string, path: string): { fn: MockHandler; params: Record<string, string> } | null {
  for (const r of routes) {
    if (r.method !== method.toUpperCase()) continue;
    const m = r.re.exec(path);
    if (!m) continue;
    const params: Record<string, string> = {};
    r.keys.forEach((k, i) => (params[k] = decodeURIComponent(m[i + 1])));
    return { fn: r.fn, params };
  }
  return null;
}

/** Deep copy, so that handlers can change fixtures without side effects. */
export const clone = <T>(v: T): T => JSON.parse(JSON.stringify(v));

export const nowIso = () => new Date().toISOString();

let seq = 0;
export const mockId = (prefix: string) => `${prefix}-mock${(++seq).toString(36)}${Date.now().toString(36).slice(-4)}`;
