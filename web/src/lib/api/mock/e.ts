// Mock handlers of web part 1 (changes, editor, review, lab). Area E owns
// this file; see registry.ts for on()/reply(). Fixtures are captured from a
// real bench run of the seed story (fixtures/e.json, demo change c-k7m2qz).
// State lives in memory: saves, commits, pushes and lab runs change it until
// the page reloads. Lab runs advance with time (no SSE in mock mode; the lab
// page polls while a run is running and events are not connected).
import { on, fail, reply, clone, nowIso, mockId, match, MockReply } from './registry';
import { seed } from './seed';
import fx from './fixtures/e.json';

const F = fx as any;
const DEMO = 'c-k7m2qz';
const DONE = ['merged', 'deployed', 'verified', 'closed'];
const STATES = ['draft', 'lab', 'review', 'approved', 'merged', 'deployed', 'verified'];

/** Per change: saved, uncommitted files and the content overlay. */
const worktrees: Record<string, { files: Record<string, string>; dirty: Set<string>; added: Set<string> }> = {};
const details: Record<string, any> = {};
const runs: Record<string, any> = {};

function wt(id: string) {
  return (worktrees[id] ??= { files: {}, dirty: new Set(), added: new Set() });
}

function summary(id: string): any | undefined {
  return seed.changes.changes.find((c: any) => c.id === id);
}

function patchSummary(id: string, p: Record<string, unknown>) {
  const s = summary(id);
  if (s) Object.assign(s, p, { updatedAt: nowIso() });
  // The detail keeps its commit list (the summary has a count).
  const { commits: _n, ...rest } = p;
  if (details[id]) Object.assign(details[id], rest, { updatedAt: nowIso() });
}

function lifecycle(c: any): any[] {
  const at = Math.max(0, STATES.indexOf(c.state));
  const label: Record<string, string> = {
    draft: c.local ? 'local' : 'branch',
    lab: c.labRun ? c.labRun.split('/').pop() : '',
    review: c.mr ? `${c.mr.draft ? 'draft ' : ''}MR` : '',
    approved: `${c.approvals?.have ?? 0} of ${c.approvals?.need ?? 2}`,
  };
  return STATES.map((s, i) => ({
    state: s,
    label: label[s] ?? '',
    status: c.state === 'closed' ? 'skipped' : i < at ? 'done' : i === at ? (s === 'lab' && c.labState === 'failed' ? 'fail' : 'now') : 'todo',
  }));
}

/** Detail of a change: captured fixture, else built from the summary. */
function detail(id: string): any | undefined {
  const s = summary(id);
  if (!s && !details[id]) return undefined;
  if (!details[id]) {
    const base = F.changes[id] ? clone(F.changes[id]) : {};
    details[id] = {
      changedFiles: [],
      children: [],
      labRuns: [],
      overlapsDetail: [],
      approvalsBy: [],
      meta: { id, title: s.title, intent: s.intent ?? '', author: s.author, target: s.target, workItems: s.workItems ?? [] },
      ...base,
      ...clone(s),
      commits: Array.isArray(base.commits) ? base.commits : [],
    };
    if (id === DEMO && !details[id].commits.length) details[id].commits = clone(F.commits);
  } else if (s) {
    const { commits: _c, ...rest } = s;
    Object.assign(details[id], rest);
  }
  const d = details[id];
  const w = worktrees[id];
  if (w) d.dirty = w.dirty.size > 0;
  d.lifecycle = lifecycle(d);
  return d;
}

// ------------------------------------------------------------ changes
on('GET', '/changes/{id}', ({ params }) => detail(params.id) ?? fail(404, `change ${params.id} not found`));

on('POST', '/changes', ({ body }) => {
  const id = mockId('c').slice(0, 8);
  const slug = String(body?.title ?? 'change')
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-|-$/g, '')
    .slice(0, 40);
  const target = body?.target && body.target !== 'main' ? body.target : 'main';
  const parent = target === 'main' ? undefined : target.replace(/^change\//, '').slice(0, 8);
  const c = {
    id,
    title: body?.title ?? 'New change',
    intent: body?.intent ?? '',
    branch: `change/${id}-${slug}`,
    target,
    parent,
    author: seed.me.id,
    authorKind: 'human',
    state: 'draft',
    labState: 'none',
    group: 'mine',
    local: true,
    dirty: false,
    pushed: false,
    aheadTarget: 1,
    behindTarget: 0,
    aheadRemote: 1,
    behindRemote: 0,
    files: 1,
    commits: 1,
    approvals: { have: 0, need: 1 },
    workItems: body?.workItems ?? [],
    createdAt: nowIso(),
    updatedAt: nowIso(),
  };
  seed.changes.changes.unshift(c);
  const metaPath = `.pk/changes/${id}.yaml`;
  const w = wt(id);
  w.files[metaPath] = `apiVersion: pk/v1alpha1\nkind: Change\nid: ${id}\ntitle: ${c.title}\nintent: ${c.intent}\nauthor: ${c.author}\ntarget: ${target}\n`;
  w.added.add(metaPath);
  const d = detail(id);
  d.worktree = `/home/dennis/pk/worktrees/${id}`;
  d.commits = [{ sha: mockSha(), short: '', subject: `chore(change): open ${id}`, author: c.author, date: nowIso() }];
  d.commits[0].short = d.commits[0].sha.slice(0, 7);
  return reply(201, d);
});

const sha = () => Array.from({ length: 40 }, () => '0123456789abcdef'[Math.floor(Math.random() * 16)]).join('');
const mockSha = sha;

function act(fn: (c: any, body: any) => unknown) {
  return ({ params, body }: { params: Record<string, string>; body: any }) => {
    const c = detail(params.id);
    if (!c) return fail(404, `change ${params.id} not found`);
    return fn(c, body);
  };
}

on('POST', '/changes/{id}/push', act((c) => {
  const created = !c.mr;
  const mr = c.mr ?? { number: 123, url: `http://gitea.local/pk/platform/pulls/123`, draft: true };
  patchSummary(c.id, { pushed: true, aheadRemote: 0, mr });
  return { change: summary(c.id) ?? c, mr, created, pushed: c.commits.slice(-2).map((x: any) => x.short) };
}));
on('POST', '/changes/{id}/ready', act((c) => {
  if (!c.mr) return fail(409, 'push first', 'The change has no merge request yet.');
  patchSummary(c.id, { mr: { ...c.mr, draft: false }, state: c.state === 'draft' || c.state === 'lab' ? 'review' : c.state });
  return detail(c.id);
}));
on('POST', '/changes/{id}/rebase', act((c) => {
  const from = c.headSha ?? sha();
  const to = sha();
  patchSummary(c.id, { behindTarget: 0, headSha: to });
  return { ok: true, change: summary(c.id) ?? c, from, to, impactUnchanged: true };
}));
on('POST', '/changes/{id}/approve', act((c) => {
  const have = Math.min((c.approvals?.have ?? 0) + 1, c.approvals?.need ?? 2);
  patchSummary(c.id, { approvals: { ...c.approvals, have }, state: have >= (c.approvals?.need ?? 2) ? 'approved' : c.state });
  return detail(c.id);
}));
on('POST', '/changes/{id}/merge', act((c) => {
  if (c.state !== 'approved') return fail(409, 'not mergeable', 'Gates are not met: approvals and lab gate first.');
  patchSummary(c.id, { state: 'merged', group: 'mine' });
  return { change: summary(c.id) ?? c, sha: sha(), message: `Merge change ${c.id}: ${c.title}\n\nPk-Change: ${c.id}` };
}));
on('POST', '/changes/{id}/close', act((c) => {
  patchSummary(c.id, { state: 'closed' });
  return detail(c.id);
}));
on('POST', '/changes/{id}/checkout', act((c) => {
  patchSummary(c.id, { local: true });
  const d = detail(c.id);
  d.worktree ??= `/home/dennis/pk/worktrees/${c.id}`;
  return d;
}));
on('POST', '/changes/{id}/revert', act((c) => {
  const id = mockId('c').slice(0, 8);
  seed.changes.changes.unshift({ ...clone(summary(c.id) ?? c), id, title: `Revert: ${c.title}`, state: 'draft', labState: 'none', local: true, pushed: false, mr: undefined });
  return reply(201, detail(id));
}));

// ------------------------------------------------------------ commits
on('GET', '/changes/{id}/commits', act((c) => c.commits));
on('POST', '/changes/{id}/commits', act((c, body) => {
  const w = wt(c.id);
  const paths: string[] = body?.paths?.length ? body.paths : [...w.dirty];
  if (!paths.length) return fail(409, 'nothing to commit');
  const [subject, , ...rest] = String(body?.message ?? '').split('\n');
  const s = sha();
  const commit = { sha: s, short: s.slice(0, 7), subject, body: rest.join('\n'), author: seed.me.id, date: nowIso() };
  c.commits = [commit, ...c.commits];
  paths.forEach((p) => w.dirty.delete(p));
  const sum = summary(c.id);
  patchSummary(c.id, { commits: (sum?.commits ?? 0) + 1, aheadRemote: (sum?.aheadRemote ?? 0) + 1, dirty: w.dirty.size > 0, headSha: s });
  return reply(201, commit);
}));
on('POST', '/changes/{id}/commit-message', act((c, body) => {
  const w = worktrees[c.id];
  const paths: string[] = body?.paths?.length ? body.paths : [...(w?.dirty ?? [])];
  const names = paths.map((p) => p.split('/').pop()!.replace(/\.ya?ml$/, ''));
  const scope = paths[0]?.includes('inventory') ? 'inventory' : paths[0]?.includes('cilium') ? 'cilium' : 'config';
  const subject = `feat(${scope}): update ${names.join(' and ') || c.title.toLowerCase()}`;
  const bullets = names.map((n) => `- ${n}: ${c.intent || 'see the change intent'}`).join('\n');
  return { message: `${subject}\n\n${bullets}`, subject, body: bullets, model: 'mock', mock: true, fallback: true, logId: mockId('l') };
}));

// ------------------------------------------------------------ files
function treeFor(id: string | null): any {
  const t = clone(F.tree);
  const w = id ? worktrees[id] : undefined;
  const strip = (ns: any[]) =>
    ns.forEach((n) => {
      if (!id) {
        delete n.status;
        delete n.changed;
      }
      if (n.children) strip(n.children);
    });
  strip(t.entries);
  if (id && id !== DEMO) strip(t.entries);
  if (w) {
    for (const p of [...w.dirty, ...w.added]) {
      if (!findNode(t.entries, p)) insertNode(t.entries, p);
      const n = findNode(t.entries, p);
      if (n && !n.status) n.status = w.added.has(p) ? 'added' : 'modified';
    }
    recount(t.entries);
  }
  t.ref = id ?? 'main';
  return t;
}
function findNode(ns: any[], p: string): any {
  for (const n of ns) {
    if (n.path === p) return n;
    if (n.children && p.startsWith(n.path + '/')) return findNode(n.children, p);
  }
  return undefined;
}
function insertNode(ns: any[], p: string) {
  const parts = p.split('/');
  let level = ns;
  for (let i = 0; i < parts.length; i++) {
    const path = parts.slice(0, i + 1).join('/');
    let n = level.find((x) => x.path === path);
    if (!n) {
      n = i === parts.length - 1 ? { path, name: parts[i], type: 'file' } : { path, name: parts[i], type: 'dir', children: [] };
      level.push(n);
    }
    level = n.children ?? [];
  }
}
function recount(ns: any[]): number {
  let total = 0;
  for (const n of ns) {
    if (n.type === 'dir') {
      const c = recount(n.children ?? []);
      if (c) n.changed = c;
      total += c;
    } else if (n.status) total++;
  }
  return total;
}

on('GET', '/changes/{id}/tree', act((c) => ({ ...treeFor(c.id), ref: c.id })));
on('GET', '/refs/{ref}/tree', ({ params }) => ({ ...treeFor(null), ref: params.ref }));
on('GET', '/changes/{id}/status', act((c) => {
  const w = worktrees[c.id];
  const files = w ? [...w.dirty].map((p) => ({ path: p, status: w.added.has(p) ? 'added' : 'modified' })) : [];
  return { clean: files.length === 0, head: c.headSha, files };
}));

/** Kind of a path from the fixtures, or from the kind globs. */
function kindOf(path: string): any {
  const f = F.files[path];
  if (f?.kind) return f.kind;
  for (const k of F.kinds as any[]) {
    if (k.paths.some((g: string) => globMatch(g, path))) {
      const name = path.split('/').pop()!.replace(/\.ya?ml$/, '');
      return { name: k.name, apiVersion: k.apiVersion, version: k.apiVersion.split('/').pop(), own: k.own, objectName: name };
    }
  }
  return undefined;
}
function globMatch(g: string, p: string): boolean {
  const re = new RegExp('^' + g.replace(/[.+^${}()|[\]\\]/g, '\\$&').replace(/\*\*/g, '§').replace(/\*/g, '[^/]*').replace(/§/g, '.*') + '$');
  return re.test(p);
}

/** Mock problems: a few checks that the demo shows. */
function problemsOf(path: string, content: string): any[] {
  const out: any[] = [];
  content.split('\n').forEach((l, i) => {
    const m = /^\s*(interval|timeout):\s*(\S+)\s*$/.exec(l);
    if (m && !/^\d+(\.\d+)?(ms|s|m|h)+$/.test(m[2]) && !/^\d+(ms|s|m|h)(\d+(ms|s|m|h))*$/.test(m[2])) {
      out.push({ path, line: i + 1, severity: 'error', source: 'schema', field: `spec.${m[1]}`, message: `spec.${m[1]} is not a duration` });
    }
    const port = /port:\s*(\d+)/.exec(l);
    if (port && path.includes('inventory') && Number(port[1]) === 1) {
      out.push({ path, line: i + 1, severity: 'error', source: 'reference', message: `port ${port[1]} in use by cn-003` });
    }
  });
  return out;
}

function fileOf(id: string | null, path: string): any {
  const w = id ? worktrees[id] : undefined;
  const base = F.files[path] ? clone(F.files[path]) : undefined;
  const content = w?.files[path] ?? base?.content;
  if (content === undefined) return undefined;
  const lang = /\.ya?ml$/.test(path) ? 'yaml' : /\.md$/.test(path) ? 'markdown' : /\.json$/.test(path) ? 'json' : 'text';
  return {
    secrets: [],
    applyPath: 'flux',
    truthClass: 'intent',
    ...base,
    path,
    ref: id ?? 'main',
    content,
    exists: true,
    readOnly: !id,
    language: lang,
    kind: kindOf(path),
    status: w?.added.has(path) ? 'added' : w?.dirty.has(path) ? 'modified' : id === DEMO ? base?.status : undefined,
    problems: problemsOf(path, content),
    sha: hash(content),
  };
}
function hash(s: string): string {
  let h = 0;
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) | 0;
  return (h >>> 0).toString(16).padStart(8, '0');
}

on('GET', '/changes/{id}/file', async ({ params, query }) => {
  const path = query.get('path') ?? '';
  const f = fileOf(params.id, path);
  if (f) return f;
  // Other files: the main version from F's handler, editable in the worktree.
  const m = match('GET', '/refs/main/file');
  if (!m) return fail(404, `${path} not found`);
  const r: any = await m.fn({ method: 'GET', path: '/refs/main/file', params: { ref: 'main' }, query, body: undefined });
  if (r instanceof MockReply) return r;
  return { ...r, ref: params.id, readOnly: DONE.includes(summary(params.id)?.state), kind: r.kind ?? kindOf(path), problems: r.problems ?? [], secrets: r.secrets ?? [] };
});
on('PUT', '/changes/{id}/file', ({ params, query, body }) => {
  const c = detail(params.id);
  if (!c) return fail(404, `change ${params.id} not found`);
  return writeFile(c, query.get('path') ?? '', body);
});
function writeFile(c: any, path: string, body: any) {
  if (!path) return fail(400, 'path missing');
  const cur = fileOf(c.id, path);
  if (body?.baseSha && cur && cur.sha !== body.baseSha) return fail(409, 'stale', 'The file changed since you read it.');
  const w = wt(c.id);
  let content: string = body?.content ?? cur?.content ?? '';
  // Masked secrets keep their stored values.
  if (cur && content.includes('••••••••')) content = unmask(content, cur.content);
  w.files[path] = content;
  if (!cur && !findNode(F.tree.entries, path)) w.added.add(path);
  w.dirty.add(path);
  patchSummary(c.id, { dirty: true });
  return { file: fileOf(c.id, path), impact: impactSummary() };
}
function unmask(next: string, _stored: string): string {
  return next; // the stored fixture content is masked already
}
function impactSummary() {
  return clone(F.impact.summary);
}

on('DELETE', '/changes/{id}/file', ({ params, query }) => {
  const c = detail(params.id);
  if (!c) return fail(404, 'not found');
  const path = query.get('path') ?? '';
  const w = wt(c.id);
  delete w.files[path];
  w.added.delete(path);
  w.dirty.delete(path);
  return { file: { path, content: '', exists: false, readOnly: false, language: 'text', problems: [], secrets: [] }, impact: impactSummary() };
});

on('POST', '/changes/{id}/file/new', act((c, body) => {
  const kind = (F.kinds as any[]).find((k) => k.name === body?.kind);
  if (!kind) return fail(400, `unknown kind ${body?.kind}`);
  const name = String(body?.name ?? 'new');
  const path = body?.path ?? (kind.paths[0] ?? `${name}.yaml`).replace('**/', '').replace('*', name);
  const schema = F.schemas[kind.name];
  const content =
    kind.name === 'Node'
      ? `apiVersion: ${kind.apiVersion}\nkind: Node\nmetadata:\n  name: ${name}\nspec:\n  cluster: defiant\n  role: worker\n  location:\n    rack: GC5A-6Z\n    unit: 10\n    size: 2\n  model: ""\n  serial: ""\n  bmc:\n    mac: ""\n  ports: []\n`
      : `apiVersion: ${kind.apiVersion}\nkind: ${kind.name}\nmetadata:\n  name: ${name}\n${schema ? 'spec: {}\n' : ''}`;
  const w = wt(c.id);
  w.files[path] = content;
  w.added.add(path);
  w.dirty.add(path);
  return reply(201, fileOf(c.id, path));
}));

on('GET', '/changes/{id}/problems', act((c) => {
  const w = worktrees[c.id];
  const problems = w ? Object.entries(w.files).flatMap(([p, s]) => problemsOf(p, s)) : [];
  return { problems, errors: problems.filter((p) => p.severity === 'error').length, warnings: 0 };
}));
on('GET', '/kinds', () => F.kinds);
on('GET', '/kinds/{kind}/schema', ({ params }) => F.schemas[params.kind] ?? fail(404, `no schema for ${params.kind}`));

// ------------------------------------------------------------ review
const forDemo = (id: string, v: unknown, empty: unknown) => (id === DEMO ? clone(v) : empty);
on('GET', '/changes/{id}/impact', act((c) => {
  if (c.id === DEMO) return { ...clone(F.impact), changeId: c.id };
  const ci = c.impact ?? { clusters: [], added: 0, changed: 0, deleted: 0, text: 'no change' };
  return {
    changeId: c.id,
    base: { ref: 'main' },
    head: { ref: c.id },
    clusters: [],
    semantic: [],
    overlaps: [],
    openSteps: [],
    summary: ci,
    risk: { level: c.risk ?? 'low', reasons: [] },
    problems: [],
  };
}));
on('GET', '/changes/{id}/diff', ({ params, query }) => {
  const v = (query.get('view') ?? 'semantic') as 'semantic' | 'source' | 'rendered';
  return forDemo(params.id, F.diff[v], { view: v, semantic: [], files: [], objects: [] });
});
on('GET', '/changes/{id}/gates', act((c) => {
  const g = c.id === DEMO ? clone(F.gates) : clone(c.gates ?? F.gates);
  g.approvals = { ...g.approvals, have: c.approvals?.have ?? 0, need: c.approvals?.need ?? g.approvals.need };
  g.rebased = { ...g.rebased, ok: !c.behindTarget, behind: c.behindTarget ?? 0 };
  const last = c.labRun ? runs[c.labRun] : undefined;
  if (last) g.labGate = { ...g.labGate, run: last.id, status: last.result === 'pass' ? 'met' : last.result === 'running' ? 'running' : 'failed' };
  return g;
}));

// ------------------------------------------------------------ labs
const STEP_MS = 1100;
function advance(r: any) {
  if (r.result !== 'running') return r;
  const elapsed = Date.now() - Date.parse(r.startedAt);
  const n = Math.floor(elapsed / STEP_MS);
  r.steps.forEach((s: any, i: number) => {
    if (i < n) {
      s.status = 'pass';
      s.output = s._output;
      s.checks = s._checks;
      s.durationMs ??= STEP_MS;
    } else if (i === n) {
      s.status = 'running';
      s.output = (s._output ?? []).slice(0, 1 + Math.floor(((elapsed % STEP_MS) / STEP_MS) * (s._output?.length ?? 1)));
    } else s.status = 'pending';
  });
  if (n >= r.steps.length) {
    r.result = 'pass';
    r.finishedAt = nowIso();
    r.durationMs = elapsed;
    r.checksPassed = r.checksTotal;
    r.summary = `${r.checksTotal} of ${r.checksTotal} checks pass`;
    r.report = `kb/labs/${r.id.replace('/', '-')}.md`;
    patchSummary(r.changeId, { labState: 'passed', labRun: r.id });
  }
  return r;
}
function view(r: any) {
  const v = clone(advance(r));
  v.steps.forEach((s: any) => {
    delete s._output;
    delete s._checks;
  });
  return v;
}
// The captured run of the demo change.
const first = clone(F.labrun);
runs[first.id] = first;

on('POST', '/changes/{id}/labs', act((c, body) => {
  const number = Object.values(runs).filter((r: any) => r.changeId === c.id).length + 1;
  const id = `${c.id}/run-${number}`;
  const t = clone(F.labrun);
  const r = {
    ...t,
    id,
    changeId: c.id,
    number,
    lab: id.replace('/', '-'),
    sha: c.headSha,
    scenario: body?.scenario ?? t.scenario,
    result: 'running',
    startedAt: nowIso(),
    finishedAt: undefined,
    durationMs: undefined,
    checksPassed: 0,
    report: undefined,
    summary: undefined,
    by: seed.me.id,
    steps: t.steps.map((s: any) => ({ ...s, status: 'pending', _output: s.output, _checks: s.checks, output: [], checks: undefined, durationMs: undefined })),
  };
  runs[id] = r;
  patchSummary(c.id, { labState: 'running', labRun: id, state: c.state === 'draft' ? 'lab' : c.state });
  return reply(202, view(r));
}));
on('GET', '/labs/runs/{run}', ({ params }) => {
  const r = runs[params.run];
  return r ? view(r) : fail(404, `lab run ${params.run} not found`);
});
on('GET', '/labs', ({ query }) => {
  const id = query.get('changeId');
  const list = Object.values(runs).filter((r: any) => !id || r.changeId === id).map(view);
  return { definitions: clone(F.labs.definitions), scenarios: clone(F.labs.scenarios ?? []), runs: list };
});
