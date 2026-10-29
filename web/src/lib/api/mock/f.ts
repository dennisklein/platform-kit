// Mock handlers of web part 2: assets, KB, work, history, clusters, rings,
// LLM (chat, advisors, log, search), agents, audit and the demo helpers.
import { on, reply, fail, clone, nowIso, mockId } from './registry';
import { seed } from './seed';
import entities from './fixtures/entities.json';
import entityMap from './fixtures/entity-map.json';
import racks from './fixtures/racks.json';
import kb from './fixtures/kb.json';
import articles from './fixtures/articles.json';
import work from './fixtures/work.json';
import workFiles from './fixtures/work-files.json';
import history from './fixtures/history.json';
import historyDetail from './fixtures/history-detail.json';
import refsFiles from './fixtures/refs-files.json';
import clusters from './fixtures/clusters.json';
import rings from './fixtures/rings.json';
import agents from './fixtures/agents.json';
import advisors from './fixtures/advisors.json';
import llmLog from './fixtures/llm-log.json';
import search from './fixtures/search.json';
import llm from './fixtures/llm.json';
import policy from './fixtures/policy.json';

const isChange = (ref: string | null) => !!ref && /^c-[a-z0-9]{6}$/.test(ref);

// ------------------------------------------------------------ assets
on('GET', '/entities', ({ query }) => {
  const kind = query.get('kind');
  return (entities as any[]).filter((e) => !kind || e.kind === kind);
});
on('GET', '/entities/{kind}/{name}', ({ params, query }) => {
  const e = (entityMap as Record<string, any>)[`${params.kind}/${params.name}`];
  if (!e) return fail(404, `${params.kind} ${params.name} not found`);
  const out = clone(e);
  if (!isChange(query.get('ref'))) {
    // New in the change: not on main yet.
    if (out.change?.status === 'added') return fail(404, `${params.kind} ${params.name} not found in main`);
    delete out.change;
    delete out.semantic;
    out.ref = 'main';
  }
  return out;
});
on('GET', '/racks/{name}', ({ params, query }) => {
  const r = (racks as Record<string, any>)[params.name.toUpperCase()];
  if (!r) return fail(404, `rack ${params.name} not found`);
  return isChange(query.get('ref')) ? r.change : r.main;
});

// ------------------------------------------------------------ kb
on('GET', '/kb', ({ query }) => {
  const entity = query.get('entity');
  const type = query.get('type');
  return (kb as any[]).filter((a) => (!entity || a.entities?.includes(entity)) && (!type || a.type === type));
});
on('GET', '/kb/article', ({ query }) => {
  const a = (articles as Record<string, any>)[query.get('path') ?? ''];
  return a ?? fail(404, `article ${query.get('path')} not found`);
});

// ------------------------------------------------------------ work
on('GET', '/work', () => {
  const w = clone(work as any);
  // The demo change links w-114 (step 2 of the story).
  const item = w.items.find((i: any) => i.id === 'w-114');
  const c = seed.changes.changes.find((x: any) => x.id === 'c-k7m2qz');
  if (item && c && item.changes.length === 0) {
    item.changes.push({ id: c.id, title: c.title, state: c.state, author: c.author, date: c.createdAt });
    item.status = 'in-progress';
    item.statusFrom = c.id;
  }
  return w;
});
on('GET', '/refs/{ref}/file', ({ params, query }) => {
  const path = query.get('path') ?? '';
  const conv = query.get('convert') === 'true' ? 'true' : 'false';
  const tt = refsFiles as Record<string, any>;
  const key = Object.keys(tt).find((k) => {
    const [sha, p, c] = k.split('|');
    return p === path && c === conv && (sha === params.ref || sha.startsWith(params.ref) || params.ref.startsWith(sha));
  });
  if (key) return tt[key];
  const wf = (workFiles as Record<string, any>)[path];
  if (wf) return { ...wf, ref: params.ref };
  const ent = Object.values(entityMap as Record<string, any>).find((e) => e.path === path);
  if (ent) {
    return {
      path,
      ref: params.ref,
      content: toYaml(ent.data),
      exists: true,
      readOnly: true,
      language: 'yaml',
      storedVersion: ent.storedVersion ?? ent.apiVersion,
      kind: { name: ent.kind, apiVersion: ent.apiVersion, objectName: ent.name },
      problems: [],
      secrets: [],
    };
  }
  return fail(404, `${path} not found at ${params.ref}`);
});

// Small YAML writer for the mock (plain maps, lists and scalars).
function toYaml(v: unknown, indent = ''): string {
  if (Array.isArray(v)) {
    return v
      .map((x) => {
        const s = toYaml(x, indent + '  ');
        return typeof x === 'object' && x !== null ? `${indent}- ${s.trimStart()}` : `${indent}- ${s.trim()}\n`;
      })
      .join('');
  }
  if (v && typeof v === 'object') {
    return Object.entries(v as Record<string, unknown>)
      .map(([k, x]) =>
        x && typeof x === 'object'
          ? `${indent}${k}:\n${toYaml(x, indent + '  ')}`
          : `${indent}${k}: ${String(x)}\n`,
      )
      .join('');
  }
  return `${String(v)}\n`;
}

// ------------------------------------------------------------ history
on('GET', '/history', ({ query }) => {
  const words = (query.get('q') ?? '').toLowerCase().split(/\s+/).filter(Boolean);
  const entity = query.get('entity');
  const path = query.get('path');
  const author = query.get('author');
  const kind = query.get('kind');
  const entries = (history as any).entries.filter((e: any) => {
    const hay = [e.title, e.intent, e.change, e.short, ...(e.trailers ?? []).map((t: any) => `${t.key}: ${t.value}`)]
      .join(' ')
      .toLowerCase();
    if (!words.every((w) => hay.includes(w))) return false;
    if (entity && !(e.entities ?? []).includes(entity)) return false;
    if (path && !(e.files ?? []).some((f: string) => f.startsWith(path.replace(/\*.*$/, '')))) return false;
    if (author && e.author !== author && e.agent !== author) return false;
    if (kind) {
      const k = kind.toLowerCase();
      const m: Record<string, RegExp> = {
        node: /control\/inventory\/(cn|cp|sn)-/,
        article: /^kb\//,
        workitem: /^work\//,
        helmrelease: /helmrelease/,
        ciliumbgppeeringpolicy: /bgp-/,
        deployment: /deployment/,
      };
      if (!(e.files ?? []).some((f: string) => (m[k] ?? new RegExp(k)).test(f))) return false;
    }
    return true;
  });
  return { entries, total: entries.length };
});
on('GET', '/history/{sha}', ({ params }) => {
  const d = historyDetail as Record<string, any>;
  const key = Object.keys(d).find((k) => k === params.sha || k.startsWith(params.sha));
  return key ? d[key] : fail(404, `commit ${params.sha} not found`);
});

// ------------------------------------------------------------ clusters, rings
const clusterState = clone(clusters as any[]);
const ringState = clone(rings as any[]);

on('GET', '/clusters', () => clusterState);
on('GET', '/rings', () => ringState);
const ringOp = (op: 'promote' | 'hold' | 'release') => ({ params }: { params: Record<string, string> }) => {
  const r = ringState.find((x) => x.ring === params.ring);
  if (!r) return fail(404, `ring ${params.ring} not found`);
  if (op === 'hold') Object.assign(r, { held: true, heldBy: seed.me.id, gate: 'none' });
  if (op === 'release') Object.assign(r, { held: false, heldBy: undefined });
  if (op === 'promote') {
    if (r.held) return fail(409, `ring ${r.ring} is held`, `held by ${r.heldBy}`);
    Object.assign(r, { tag: `ring/${r.ring}`, sha: r.candidate, change: r.candidateChange, following: `ring/${r.ring}`, gate: 'none', reason: undefined });
  }
  return r;
};
on('POST', '/rings/{ring}/promote', ringOp('promote'));
on('POST', '/rings/{ring}/hold', ringOp('hold'));
on('POST', '/rings/{ring}/release', ringOp('release'));

on('POST', '/demo/flux', ({ body }) => {
  const c = clusterState.find((x) => x.id === body?.cluster);
  if (!c) return fail(404, `cluster ${body?.cluster} not found`);
  c.failing = !!body.fail;
  c.healthy = !body.fail;
  c.message = body.fail
    ? `Health check failed: Machine/cn-036 not Ready (demo fault)`
    : `Applied revision main@sha1:${c.applied?.sha?.slice(0, 7)}, health checks pass`;
  for (const k of c.kustomizations ?? []) {
    if (k.name === 'infra-metal') {
      k.ready = !body.fail;
      k.message = body.fail ? 'Health check failed: Machine/cn-036 not Ready (demo fault)' : `Applied revision: ${k.revision}`;
    }
  }
  return c;
});
on('POST', '/demo/approve', ({ body }) => {
  const c = seed.changes.changes.find((x: any) => x.id === body?.changeId);
  if (!c) return fail(404, `change ${body?.changeId} not found`);
  if (!c.mr) return fail(409, 'no merge request yet', 'push the change first');
  c.approvals = { ...c.approvals, have: Math.min((c.approvals?.have ?? 0) + 1, c.approvals?.need ?? 2) };
  if (c.approvals.have >= c.approvals.need && c.state === 'review') c.state = 'approved';
  audit('demo.approve', c.id, `approved as ${body.as}`);
  return { user: body.as, at: nowIso(), counts: true };
});

// ------------------------------------------------------------ agents
const grants = clone(agents as any[]);
on('GET', '/agents', () => grants);
on('POST', '/agents/{id}/approve', ({ params, body }) => {
  const g = grants.find((x) => x.id === params.id);
  if (!g) return fail(404, `agent ${params.id} not found`);
  const hours = body?.hours ?? 8;
  const now = new Date();
  Object.assign(g, {
    active: true,
    mode: body?.mode ?? 'navigate',
    approvedBy: seed.me.id,
    approvedAt: now.toISOString(),
    expiresAt: new Date(now.getTime() + hours * 3600_000).toISOString(),
  });
  audit('agent.approve', g.id, `${hours}h, ${g.mode} mode`);
  const a = clone((llm as any).approval);
  const token = 'pk_agt_' + Array.from({ length: 24 }, () => Math.floor(Math.random() * 16).toString(16)).join('');
  const s = JSON.stringify({ ...a, grant: g }).replaceAll(a.token, token);
  return JSON.parse(s);
});
on('DELETE', '/agents/{id}/grant', ({ params }) => {
  const g = grants.find((x) => x.id === params.id);
  if (!g) return fail(404, `agent ${params.id} not found`);
  Object.assign(g, { active: false, expiresAt: undefined });
  audit('agent.revoke', g.id, '');
  return g;
});

// ------------------------------------------------------------ audit
const auditLog: any[] = [];
function audit(action: string, target: string, detail: string) {
  auditLog.unshift({ id: mockId('a'), time: nowIso(), actor: seed.me.id, actorKind: 'human', action, target, detail });
}
on('GET', '/audit', () => auditLog);

// Policy summary for the settings page (E may register a richer one later).
on('GET', '/policy', () => policy);

// ------------------------------------------------------------ llm
const log = clone((llmLog as any).entries as any[]);
const suggestions: any[] = [];

function logEntry(surface: string, prompt: string, answer: string, extra: Record<string, unknown> = {}) {
  const e = { id: mockId('l'), time: nowIso(), user: seed.me.id, surface, model: 'mock', mock: true, prompt, answer, durationMs: 180, ...extra };
  log.unshift(e);
  return e;
}

on('GET', '/advisors', () => advisors);
on('POST', '/changes/{id}/advisors/{recipe}', ({ params }) => {
  const fx = (llm as any).advisor[params.recipe] ?? (llm as any).advisor.risks;
  const r = clone(fx);
  const e = logEntry('advisor', `Recipe: ${r.title ?? params.recipe}`, r.points.map((p: any) => p.text).join('\n'), {
    recipe: params.recipe,
    changeId: params.id,
    sources: r.points.flatMap((p: any) => p.sources),
  });
  r.logId = e.id;
  r.changeId = params.id;
  r.createdAt = e.time;
  for (const s of r.suggestions ?? []) {
    s.id = mockId('s');
    s.changeId = params.id;
    s.logId = e.id;
    suggestions.push(s);
  }
  return r;
});

on('POST', '/chat', ({ body }) => {
  const msgs = body?.messages ?? [];
  const q: string = (msgs[msgs.length - 1]?.content ?? '').toLowerCase();
  const c = (llm as any).chat;
  let a: any;
  // The lookups the bench shows as steps of the answer.
  const step = (id: number, tool: string, label: string, summary = '') => ({ id, tool, label, summary, state: 'ok', ms: 40 });
  let steps: any[] = [step(1, 'search', `search '${q.split(/\s+/).slice(0, 3).join(' ')}'`, '4 hits')];
  // The secret guard answers without a model.
  let model = 'mock';
  if (/token|secret|password|key\b/.test(q)) {
    a = clone(c.secret);
    model = 'guard';
    steps = [step(1, 'search', "search 'hubble ui token'", '2 secret values removed')];
  } else if (/^why|port 37|cn-003/.test(q)) {
    a = clone(c.why);
    steps = [step(1, 'history', 'history cn-003', '4 merges'), step(2, 'history_entry', 'read 4 merge diffs', 'match c-2hd7vw')];
  }
  else if (/^add .*(bgp|polic)/.test(q)) {
    if (!body?.changeId) {
      a = { text: 'Open a change first. I make edits only as suggestions in a change.', sources: [], suggestions: [] };
    } else {
      a = clone(c.suggest);
      const node = /cn-\d{3}/.exec(q)?.[0] ?? 'cn-037';
      for (const s of a.suggestions) {
        s.id = mockId('s');
        s.changeId = body.changeId;
        s.status = 'pending';
        s.after = s.after.replaceAll('cn-037', node);
        s.diff = s.diff.replaceAll('cn-037', node);
        s.title = `Add ${node} to the BGP policy rack-6z`;
        suggestions.push(s);
      }
      a.text = `I suggest 1 line in bgp-rack-6z.yaml: it adds ${node} to rack-6z, so ${node} gets a BGP session. Review the diff below, then accept or reject it.`;
      steps = [
        step(1, 'search', `find BGP policies in ${body.changeId}`, 'rack-6z'),
        step(2, 'read_file', 'read bgp-rack-6z.yaml', '10 nodes selected'),
        step(3, 'suggest_edit', 'suggest bgp-rack-6z.yaml', '+1 line'),
      ];
    }
  } else a = clone(c.other);
  const e = logEntry('chat', msgs.map((m: any) => `${m.role}: ${m.content}`).join('\n'), a.text, {
    changeId: body?.changeId,
    sources: a.sources,
  });
  return {
    ...a,
    model,
    mock: true,
    fallback: model !== 'guard',
    // The asker chose the answer from project data over a slow model.
    fallbackReason: body?.builtin ? 'you asked for the answer from project data' : undefined,
    logId: e.id,
    steps,
    suggestions: (a.suggestions ?? []).map((s: any) => ({ ...s, logId: e.id })),
  };
});

on('GET', '/changes/{id}/suggestions', ({ params }) => suggestions.filter((s) => s.changeId === params.id));
const decide = (status: 'accepted' | 'rejected') => ({ params }: { params: Record<string, string> }) => {
  const s = suggestions.find((x) => x.id === params.sid);
  if (!s) return fail(404, `suggestion ${params.sid} not found`);
  if (s.status !== 'pending') return fail(409, `suggestion is ${s.status}`);
  Object.assign(s, { status, decidedBy: seed.me.id, decidedAt: nowIso() });
  return s;
};
on('POST', '/suggestions/{sid}/accept', decide('accepted'));
on('POST', '/suggestions/{sid}/reject', decide('rejected'));

on('GET', '/llm/log', ({ query }) => {
  const words = (query.get('q') ?? '').toLowerCase().split(/\s+/).filter(Boolean);
  const entries = log.filter((e) => {
    const hay = [e.user, e.surface, e.recipe, e.changeId, e.model, e.prompt, e.answer].join(' ').toLowerCase();
    return words.every((w) => hay.includes(w));
  });
  return { entries, total: entries.length };
});
on('GET', '/llm/log/export', () => reply(200, log.map((e) => JSON.stringify(e)).join('\n') + '\n', 'application/x-ndjson'));
on('DELETE', '/llm/log/{id}', ({ params }) => {
  const i = log.findIndex((e) => e.id === params.id);
  if (i < 0) return fail(404, `log entry ${params.id} not found`);
  log.splice(i, 1);
  audit('llm.delete', params.id, '');
  return reply(204);
});
on('POST', '/llm/log/{id}/rating', ({ params, body }) => {
  const e = log.find((x) => x.id === params.id);
  if (!e) return fail(404, `log entry ${params.id} not found`);
  e.rating = body?.rating;
  return e;
});

on('GET', '/search', ({ query }) => {
  const q = query.get('q') ?? '';
  const s = search as Record<string, any>;
  if (s[q]) return s[q];
  const words = q.toLowerCase().split(/\s+/).filter(Boolean);
  const seen = new Set<string>();
  const hits = Object.values(s)
    .flatMap((r: any) => r.hits)
    .filter((h: any) => {
      if (seen.has(h.ref)) return false;
      seen.add(h.ref);
      const hay = `${h.title} ${h.snippet} ${h.path ?? ''}`.toLowerCase();
      return words.some((w) => hay.includes(w));
    });
  return { query: q, mode: 'bm25', hits };
});
