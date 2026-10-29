// Dev mock layer: answers API calls from fixtures of the seed story, so the
// UI works without a bench (or while an endpoint answers 501). Loaded only
// when the page URL has ?mock=1 (see main.ts); never part of the normal path.
import type { Middleware } from 'openapi-fetch';
import { api, BASE } from '../client';
import { match, MockReply } from './registry';
import './seed';
import './f';
import './e';

const delay = (ms: number) => new Promise((r) => setTimeout(r, ms));

const mock: Middleware = {
  async onRequest({ request }) {
    const url = new URL(request.url, window.location.origin);
    if (!url.pathname.startsWith(BASE)) return undefined;
    const path = url.pathname.slice(BASE.length) || '/';
    const m = match(request.method, path);
    if (!m) return undefined; // to the real bench
    let body: unknown;
    const text = await request.clone().text();
    if (text) {
      try {
        body = JSON.parse(text);
      } catch {
        body = text;
      }
    }
    await delay(120 + Math.random() * 180);
    try {
      const out = await m.fn({ method: request.method, path, params: m.params, query: url.searchParams, body });
      const r = out instanceof MockReply ? out : new MockReply(200, out);
      if (r.status === 204 || r.body === undefined) return new Response(null, { status: r.status === 200 ? 204 : r.status });
      const payload = typeof r.body === 'string' && r.contentType !== 'application/json' ? r.body : JSON.stringify(r.body);
      return new Response(payload, { status: r.status, headers: { 'Content-Type': r.contentType } });
    } catch (e) {
      return new Response(JSON.stringify({ error: 'mock handler failed', detail: String(e) }), {
        status: 500,
        headers: { 'Content-Type': 'application/json' },
      });
    }
  },
};

let installed = false;

/** Answer API calls from the mock handlers (called by main.ts with ?mock=1). */
export function install() {
  if (installed) return;
  installed = true;
  api.use(mock);
  document.documentElement.dataset.mock = '1';
  console.info('[pk] API mock layer active (?mock=1)');
}
