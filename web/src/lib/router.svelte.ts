// Hash router. Routes look like "#/changes/c-k7m2qz/files?path=a.yaml".
// The route table is in src/routes.ts.
import type { Component } from 'svelte';
import type { FocusView } from './api/types';

export type Params = Record<string, string>;

export interface RouteDef {
  /** Unique name, for example "change.editor". */
  name: string;
  /** Pattern with :params, for example "/changes/:id/files". */
  pattern: string;
  /** Page title (the page may show a better one). */
  title: string;
  component: Component<{ params: Params; query: URLSearchParams }>;
  /** Left nav item that is active on this route. */
  nav?: NavKey;
  /** Show the change rail next to the page. */
  rail?: boolean;
  /** Focus view that this page reports to the bench. */
  view?: FocusView;
}

export type NavKey =
  | 'changes'
  | 'files'
  | 'assets'
  | 'kb'
  | 'work'
  | 'labs'
  | 'chat'
  | 'history'
  | 'clusters'
  | 'llm-log'
  | 'settings';

interface Compiled {
  def: RouteDef;
  re: RegExp;
  keys: string[];
}

function compile(def: RouteDef): Compiled {
  const keys: string[] = [];
  const src = def.pattern
    .split('/')
    .map((seg) => {
      if (seg.startsWith(':')) {
        keys.push(seg.slice(1));
        return '([^/]+)';
      }
      return seg.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
    })
    .join('/');
  return { def, re: new RegExp(`^${src}/?$`), keys };
}

function parseHash(): { path: string; query: URLSearchParams } {
  const h = window.location.hash.replace(/^#/, '') || '/';
  const i = h.indexOf('?');
  const path = i < 0 ? h : h.slice(0, i);
  const query = new URLSearchParams(i < 0 ? '' : h.slice(i + 1));
  return { path: path.startsWith('/') ? path : '/' + path, query };
}

class Router {
  path = $state('/');
  query = $state(new URLSearchParams());
  params = $state<Params>({});
  route = $state<RouteDef | null>(null);

  #routes: Compiled[] = [];
  #fallback = '/changes';

  /** Install the routes and follow the location hash. */
  start(routes: RouteDef[], fallback = '/changes') {
    this.#routes = routes.map(compile);
    this.#fallback = fallback;
    window.addEventListener('hashchange', () => this.#sync());
    this.#sync();
  }

  #sync() {
    const { path, query } = parseHash();
    for (const c of this.#routes) {
      const m = c.re.exec(path);
      if (m) {
        const params: Params = {};
        c.keys.forEach((k, i) => (params[k] = decodeURIComponent(m[i + 1])));
        this.path = path;
        this.query = query;
        this.params = params;
        this.route = c.def;
        return;
      }
    }
    if (path === '/' || this.route === null) {
      this.replace(this.#fallback);
      return;
    }
    this.path = path;
    this.query = query;
    this.params = {};
    this.route = null;
  }

  /** Go to a path such as "/changes/c-k7m2qz". */
  navigate(to: string) {
    const target = '#' + (to.startsWith('/') ? to : '/' + to);
    if (window.location.hash !== target) window.location.hash = target;
  }

  /** Go to a path without a new history entry. */
  replace(to: string) {
    const u = new URL(window.location.href);
    u.hash = to.startsWith('/') ? to : '/' + to;
    window.history.replaceState(null, '', u);
    this.#sync();
  }

  /** The current route with "?query", as the API stores it in Focus.route. */
  get current(): string {
    const q = this.query.toString();
    return q ? `${this.path}?${q}` : this.path;
  }
}

export const router = new Router();

/** href for a link: href('/changes/c-1') is "#/changes/c-1". */
export function href(path: string, query?: Record<string, string | undefined>): string {
  let p = '#' + (path.startsWith('/') ? path : '/' + path);
  if (query) {
    const q = new URLSearchParams();
    for (const [k, v] of Object.entries(query)) if (v !== undefined && v !== '') q.set(k, v);
    const s = q.toString();
    if (s) p += '?' + s;
  }
  return p;
}

/** Path segment encoder for ids and names. */
export const seg = (s: string) => encodeURIComponent(s);
