<script lang="ts">
  // Review a change by its effect: impact per cluster (grouped by
  // Kustomization, with links to the source lines), the diff as semantic,
  // source or rendered view, overlaps, the risk with the policy rules that
  // matched, the gates, and the advisor buttons (LLM, advisory only).
  import { onMount, tick, untrack } from 'svelte';
  import type { Params } from '../../lib/router.svelte';
  import { href, router } from '../../lib/router.svelte';
  import { api, must, errorText } from '../../lib/api/client';
  import type {
    AdvisorRecipe,
    AdvisorResult as Result,
    Diff,
    DiffViewKind,
    Gates,
    Impact,
    ObjectChange,
    SemanticLine,
  } from '../../lib/api/types';
  import { change } from '../../lib/stores/change.svelte';
  import { events } from '../../lib/stores/events.svelte';
  import { presenceFor } from '../../lib/change/presence.svelte';
  import { timeAgo, shortSha, baseName } from '../../lib/change/format';
  import StateChip from '../../lib/components/StateChip.svelte';
  import Icon from '../../lib/components/Icon.svelte';
  import OverlapCard from '../../lib/components/OverlapCard.svelte';
  import SemanticDiff from '../../lib/components/SemanticDiff.svelte';
  import UnifiedDiff from '../../lib/components/UnifiedDiff.svelte';
  import AdvisorResult from '../../lib/components/advisor/AdvisorResult.svelte';
  import AdvisorWorking from '../../lib/components/advisor/AdvisorWorking.svelte';
  import { RECIPE_TITLES, RECIPE_TOOLS, minWorkingMs, readingSteps, reducedMotion } from '../../lib/f/advisor';
  import AgentBanner from '../../lib/components/AgentBanner.svelte';
  import ChangeActions from '../changes/ChangeActions.svelte';
  import GatesCard from '../changes/GatesCard.svelte';

  let { params, query }: { params: Params; query: URLSearchParams } = $props();

  const id = $derived(params.id);
  $effect(() => change.use(id));
  const c = $derived(change.data?.id === id ? change.data : null);
  const presence = $derived(presenceFor(id, c?.presence));

  const VIEWS: DiffViewKind[] = ['semantic', 'source', 'rendered'];
  const tabParam = $derived(query.get('tab') ?? '');
  const view = $derived<DiffViewKind>(VIEWS.includes(tabParam as DiffViewKind) ? (tabParam as DiffViewKind) : 'semantic');

  let impact = $state<Impact | null>(null);
  let impactError = $state<string | null>(null);
  let gates = $state<Gates | null>(null);
  let diffs = $state<Partial<Record<DiffViewKind, Diff>>>({});
  let diffError = $state<string | null>(null);
  let recipes = $state<AdvisorRecipe[]>([]);
  // Answers, newest first; key remounts the card (and its reveal) on a new run.
  let results = $state<{ key: number; r: Result; by?: string }[]>([]);
  let nextKey = 0;
  // The recipe that runs now, with what it reads and who asked.
  let running = $state<{ recipe: string; title: string; tools: string[]; by?: string; ctl: AbortController } | null>(null);
  // The recipe asked last: its button stays pressed.
  let lastRecipe = $state<string | null>(null);
  let advisorError = $state<string | null>(null);
  let openObjects = $state(new Set<string>());

  async function loadImpact() {
    const i = id;
    try {
      const r = await must(api.GET('/changes/{id}/impact', { params: { path: { id: i } } }));
      if (i === id) {
        impact = r;
        impactError = null;
      }
    } catch (e) {
      if (i === id) impactError = errorText(e);
    }
  }

  async function loadGates() {
    const i = id;
    // Gates exist for open changes only; wait for the change to know.
    const cur = change.data?.id === i ? change.data : null;
    if (!cur || ['merged', 'deployed', 'verified', 'closed'].includes(cur.state)) {
      gates = null;
      return;
    }
    try {
      const g = await must(api.GET('/changes/{id}/gates', { params: { path: { id: i } } }));
      if (i === id) gates = g;
    } catch {
      if (i === id) gates = null;
    }
  }

  async function loadDiff(v: DiffViewKind) {
    const i = id;
    diffError = null;
    try {
      const d = await must(api.GET('/changes/{id}/diff', { params: { path: { id: i }, query: { view: v } } }));
      if (i === id) {
        diffs = { ...diffs, [v]: d };
        // Show one diff right away: the first rendered object.
        const first = v === 'rendered' ? d.objects?.find((o) => o.diff)?.key : undefined;
        if (first && openObjects.size === 0) openObjects = new Set([first]);
      }
    } catch (e) {
      if (i === id) diffError = errorText(e);
    }
  }

  // Load on open and when the change reloads (push, rebase, lab, render).
  $effect(() => {
    void id;
    void change.version;
    untrack(() => {
      void loadImpact();
      void loadGates();
      diffs = {};
    });
  });

  $effect(() => {
    const v = view;
    if (!diffs[v]) untrack(() => void loadDiff(v));
  });

  onMount(() => {
    void must(api.GET('/advisors'))
      .then((r) => (recipes = r.filter((x) => (x.scope ?? 'change') === 'change' && x.output === 'points')))
      .catch(() => (recipes = []));
    return events.on<{ changeId?: string }>('render.updated', (d) => {
      if (d.changeId === id) void loadImpact();
    });
  });

  // Answers belong to one change.
  let resultsOf = '';
  $effect(() => {
    if (resultsOf !== id) {
      resultsOf = id;
      untrack(() => {
        running?.ctl.abort();
        running = null;
        results = [];
        lastRecipe = null;
        advisorError = null;
      });
    }
  });

  // ?advisor=risks runs that recipe once (the "Ask advisor: risks" button,
  // or an agent's pk_advise with &by=dennis/agent).
  // The parameters go from the URL once the run starts, so a reload does
  // not ask again and the next pk_advise moves the screen again.
  let autoRan = '';
  $effect(() => {
    const r = query.get('advisor');
    const by = query.get('by') ?? undefined;
    const key = `${id}:${r}:${by ?? ''}`;
    if (!r) {
      autoRan = '';
    } else if (autoRan !== key) {
      autoRan = key;
      untrack(() => {
        void ask(r, by);
        const q = new URLSearchParams(query);
        q.delete('advisor');
        q.delete('by');
        const rest = q.toString();
        router.replace(`/changes/${id}/review${rest ? `?${rest}` : ''}`);
      });
    }
  });

  const sleep = (ms: number) => new Promise((res) => setTimeout(res, ms));

  /** Bring the advisor card to the top of the page, where the answer comes. */
  async function showAdvisor() {
    await tick();
    document.getElementById('advisor')?.scrollIntoView({ behavior: reducedMotion() ? 'auto' : 'smooth', block: 'start' });
  }

  async function ask(recipe: string, by?: string) {
    running?.ctl.abort();
    const rec = recipes.find((x) => x.id === recipe);
    const tools = rec?.tools ?? RECIPE_TOOLS[recipe] ?? [];
    const ctl = new AbortController();
    const cid = id;
    running = { recipe, title: rec?.title ?? RECIPE_TITLES[recipe] ?? recipe, tools, by, ctl };
    lastRecipe = recipe;
    advisorError = null;
    void showAdvisor();
    // The working card stays long enough to read what the advisor reads.
    const min = minWorkingMs(readingSteps(tools).length, reducedMotion());
    try {
      const [r] = await Promise.all([
        must(api.POST('/changes/{id}/advisors/{recipe}', { params: { path: { id: cid, recipe } }, signal: ctl.signal })),
        sleep(min),
      ]);
      if (ctl.signal.aborted || cid !== id) return;
      results = [{ key: ++nextKey, r, by }, ...results.filter((x) => x.r.recipe !== recipe)];
      void showAdvisor();
    } catch (e) {
      if (ctl.signal.aborted || cid !== id) return;
      advisorError = errorText(e);
    } finally {
      if (running?.ctl === ctl) running = null;
    }
  }

  function cancelAsk() {
    running?.ctl.abort();
    running = null;
  }

  function setView(v: DiffViewKind) {
    const q = new URLSearchParams(query);
    q.set('tab', v);
    q.delete('advisor');
    q.delete('by');
    router.replace(`/changes/${id}/review?${q}`);
  }

  // --- impact -----------------------------------------------------------------
  const changedClusters = $derived((impact?.clusters ?? []).filter((x) => x.kustomizations.some((k) => k.objects.length)));
  const quiet = $derived((impact?.clusters ?? []).filter((x) => !x.kustomizations.some((k) => k.objects.length)).map((x) => x.cluster));
  const SYM: Record<string, string> = { add: '+', change: '~', delete: '−' };
  function opCounts(cl: Impact['clusters'][number]): { op: string; n: number }[] {
    const n: Record<string, number> = { add: 0, change: 0, delete: 0 };
    for (const k of cl.kustomizations) for (const o of k.objects) n[o.op] = (n[o.op] ?? 0) + 1;
    return ['add', 'change', 'delete'].filter((op) => n[op] > 0).map((op) => ({ op, n: n[op] }));
  }
  /** Level of a risk reason ("A change to a node is high.") for its icon colour. */
  function reasonLevel(reason: string): string {
    return /\b(high|medium|low)\b/i.exec(reason)?.[1].toLowerCase() ?? impact?.risk.level ?? 'medium';
  }

  function srcHref(o: { source?: { path: string; line?: number } }): string | undefined {
    if (!o.source?.path) return undefined;
    return href(`/changes/${id}/files`, { path: o.source.path, line: o.source.line ? String(o.source.line) : undefined });
  }
  function lineHref(l: SemanticLine): string | undefined {
    if (!l.path) return undefined;
    return href(`/changes/${id}/files`, { path: l.path, line: l.line ? String(l.line) : undefined });
  }
  function toggleObject(key: string) {
    const s = new Set(openObjects);
    if (s.has(key)) s.delete(key);
    else s.add(key);
    openObjects = s;
  }

  // Source view: small diffs open; else the first file that is not a
  // record of the change (.pk/changes, lab reports). "Expand all" opens all.
  let sourceAll = $state(false);
  const RECORD = /^(\.pk\/changes\/|kb\/labs\/)/;
  const firstSource = $derived.by(() => {
    const fs = diffs.source?.files ?? [];
    const i = fs.findIndex((f) => !RECORD.test(f.path));
    return i < 0 ? 0 : i;
  });
  function expandAll(on: boolean) {
    if (view === 'source') sourceAll = on;
    else openObjects = on ? new Set((diffs.rendered?.objects ?? []).map((o) => o.key)) : new Set();
  }
  const allOpen = $derived(
    view === 'source' ? sourceAll : (diffs.rendered?.objects ?? []).every((o) => openObjects.has(o.key)) && openObjects.size > 0,
  );

  // Rendered view grouped like the impact: cluster → kustomization.
  const renderedGroups = $derived.by(() => {
    const objs = diffs.rendered?.objects ?? [];
    const g = new Map<string, ObjectChange[]>();
    for (const o of objs) {
      const k = `${o.cluster} · ${o.kustomization ?? '(none)'}`;
      g.set(k, [...(g.get(k) ?? []), o]);
    }
    return [...g.entries()];
  });

  const RISK_SRC: Record<string, string> = { rule: 'path rule', riskRule: 'risk rule', minimum: 'minimum' };
  const objectReasons = $derived((impact?.risk.reasons ?? []).filter((r) => r.source !== 'rule'));
  const rules = $derived(gates?.matchedRules ?? []);

  // A draft whose lab passed on the head: the change page says "lab passed".
  const labPassed = $derived(!!c && c.state === 'draft' && c.labState === 'pass');

  /** A path in parts that may break after each "/", never inside a name. */
  const pathParts = (p: string) => p.split(/(?<=\/)/);
</script>

<div class="page review">
  {#if change.error && !c}
    <div class="error-box">{change.error}</div>
  {:else if !c}
    <p class="muted">Loading change…</p>
  {:else}
    {#if presence}<AgentBanner {presence} />{/if}
    <header class="head">
      <div class="eyebrow">Review · <a href={href(`/changes/${c.id}`)} class="mono">{c.id}</a></div>
      <div class="titlerow">
        <h1>{c.title}</h1>
        {#if labPassed}
          <span class="labpass" title="The lab passed on the head. Next: push and mark ready.">lab passed</span>
        {:else}
          <StateChip state={c.state} labState={c.labState} />
        {/if}
        {#if impact?.risk}<span class="risk r-{impact.risk.level}">risk: {impact.risk.level}</span>{/if}
      </div>
      <div class="meta muted">
        <span class="mono">{c.branch}</span>
        {#if c.mr}<a href={c.mr.url} target="_blank" rel="noreferrer">MR !{c.mr.number}{c.mr.draft ? ' draft' : ''}</a>{/if}
        <span>{c.files} files</span>
        <span>by {c.author}{c.agent ? ' + agent' : ''}</span>
      </div>
      <ChangeActions {c} />
    </header>

    <div class="grid">
      <div class="maincol">
        <!-- impact -->
        <section class="card impact" id="impact">
          <div class="cardhead">
            <h2>Impact <span class="mono muted">{c.id}</span></h2>
            {#if impact}
              <span class="muted small">
                {impact.base.ref} {shortSha(impact.base.sha)} → {shortSha(impact.head.sha)}{impact.renderedAt ? ` · rendered ${timeAgo(impact.renderedAt)}` : ''}
              </span>
            {/if}
          </div>
          {#if impactError}
            <div class="error-box">{impactError}</div>
          {:else if !impact}
            <p class="muted">Rendering both sides…</p>
          {:else}
            {#each impact.problems as p, i (i)}
              <div class="callout bad"><Icon name="x" size={16} /><span>Render: {p.message} <span class="mono small">{p.path}</span></span></div>
            {/each}
            {#each changedClusters as cl (cl.cluster)}
              <div class="cluster">
                <div class="clname">
                  <span class="mono strong">{cl.cluster}</span>
                  <span class="eyebrow">cluster</span>
                  <span class="counts" aria-label="Object changes">
                    {#each opCounts(cl) as x (x.op)}<span class="cnt op-{x.op}">{SYM[x.op]}{x.n}</span>{/each}
                  </span>
                  <span class="objs">{cl.objectsBase} → {cl.objectsHead} objects{cl.ring ? ` · ring ${cl.ring}` : ''}</span>
                </div>
                <div class="ks">
                  {#each cl.kustomizations.filter((k) => k.objects.length) as k (k.name)}
                    {#each k.objects as o, i (o.key)}
                      <div class="obj">
                        <span class="mono kname" title={k.path}>{i === 0 ? k.name : ''}</span>
                        <span class="sym op-{o.op}">{SYM[o.op]}</span>
                        <span class="mono oname">{o.kind}/{o.name}{o.namespace ? ` (${o.namespace})` : ''}</span>
                        {#if srcHref(o)}
                          <a class="src mono small" href={srcHref(o)} title="Open the source in the editor">{baseName(o.source!.path)}{o.source!.line ? `:${o.source!.line}` : ''}</a>
                        {/if}
                      </div>
                    {/each}
                  {/each}
                </div>
              </div>
            {:else}
              <p class="muted">No rendered object changes.</p>
            {/each}
            {#if quiet.length}<p class="muted quiet">{quiet.join(', ')}: no change</p>{/if}
            {#if impact.openSteps.length}
              <div class="callout">
                <Icon name="info" size={16} />
                <div>
                  <strong>After merge, not by Flux:</strong>
                  <ul class="steps">
                    {#each impact.openSteps as s (s.kind + s.name)}<li>{s.text} <span class="muted small">({s.applyPath})</span></li>{/each}
                  </ul>
                </div>
              </div>
            {/if}
          {/if}

          <!-- diff views -->
          <div class="tabs" role="tablist">
            {#each VIEWS as v (v)}
              <button role="tab" aria-selected={view === v} class:on={view === v} onclick={() => setView(v)}>{v[0].toUpperCase() + v.slice(1)}</button>
            {/each}
            {#if view !== 'semantic' && diffs[view]}
              <button class="btn ghost small expand" onclick={() => expandAll(!allOpen)}>{allOpen ? 'Collapse all' : 'Expand all'}</button>
            {/if}
          </div>
          {#if diffError}
            <div class="error-box">{diffError}</div>
          {:else if !diffs[view]}
            <p class="muted">Loading {view} diff…</p>
          {:else if view === 'semantic'}
            {@const lines = diffs.semantic?.semantic ?? impact?.semantic ?? []}
            {#if lines.length}
              <SemanticDiff {lines} fileHref={lineHref} />
            {:else}
              <p class="muted">No semantic changes.</p>
            {/if}
          {:else if view === 'source'}
            {#each diffs.source?.files ?? [] as f, i (f.path)}
              <details class="filediff" open={sourceAll || (diffs.source?.files?.length ?? 0) <= 4 || i === firstSource}>
                <summary>
                  <span class="flag f-{f.status}">{f.status === 'added' ? '+' : f.status === 'deleted' ? '−' : 'M'}</span>
                  <span class="mono">{f.oldPath ? `${f.oldPath} → ` : ''}{f.path}</span>
                  <a class="small" href={href(`/changes/${id}/files`, { path: f.path })} onclick={(e) => e.stopPropagation()}>open</a>
                </summary>
                <UnifiedDiff diff={f.unified} />
              </details>
            {:else}
              <p class="muted">No file changes.</p>
            {/each}
          {:else}
            {#each renderedGroups as [group, objs] (group)}
              <div class="rgroup">
                <div class="eyebrow">{group}</div>
                {#each objs as o (o.key)}
                  <div class="robj">
                    <button class="rhead" onclick={() => toggleObject(o.key)} aria-expanded={openObjects.has(o.key)}>
                      <Icon name={openObjects.has(o.key) ? 'chevron-down' : 'chevron-right'} size={14} />
                      <span class="sym op-{o.op}">{SYM[o.op]}</span>
                      <span class="mono">{o.apiVersion} {o.kind}/{o.name}</span>
                      {#if srcHref(o)}<a class="src mono small" href={srcHref(o)} onclick={(e) => e.stopPropagation()}>{baseName(o.source!.path)}</a>{/if}
                    </button>
                    {#if openObjects.has(o.key) && o.diff}<UnifiedDiff diff={o.diff} />{/if}
                  </div>
                {/each}
              </div>
            {:else}
              <p class="muted">No rendered changes.</p>
            {/each}
          {/if}
        </section>

        <!-- overlap -->
        {#if impact?.overlaps.length}
          <section id="overlap" class="overlaps">
            <OverlapCard overlaps={impact.overlaps} />
          </section>
        {/if}

        <!-- advisor -->
        <section class="card advisor" id="advisor" class:tall={!!running}>
          <div class="cardhead">
            <h2><Icon name="sparkle" size={18} /> Advisor</h2>
            <span class="muted small">LLM notes, advisory only. They do not change the rating or the gates.</span>
          </div>
          <div class="abuttons">
            {#each recipes.length ? recipes : [{ id: 'risks', title: RECIPE_TITLES.risks, question: '' }] as r (r.id)}
              {@const on = lastRecipe === r.id}
              <button
                class="btn ai"
                aria-pressed={on}
                onclick={() => ask(r.id)}
                disabled={!!running}
                title={r.question || undefined}
              >
                {#if running?.recipe === r.id}<span class="spin" aria-hidden="true"></span>{:else}<Icon name="sparkle" size={14} />{/if}{r.title}
              </button>
            {/each}
          </div>
          {#if advisorError}<div class="error-box">{advisorError}</div>{/if}
          {#if running}
            <div class="aresult">
              <AdvisorWorking title={running.title} tools={running.tools} by={running.by} onCancel={cancelAsk} />
            </div>
          {/if}
          {#each results.filter((x) => x.r.recipe !== running?.recipe) as x (x.key)}
            <div class="aresult" id="advisor-result-{x.r.recipe}">
              <AdvisorResult result={x.r} showReading reveal by={x.by} onRerun={() => ask(x.r.recipe)} busy={!!running} />
            </div>
          {/each}
        </section>
      </div>

      <aside class="sidecol">
        <!-- risk and policy -->
        <section class="card" id="risk">
          <div class="cardhead">
            <h3 class="eyebrow">Risk from .pk/policy.yaml</h3>
            {#if impact?.risk}<span class="risk r-{impact.risk.level}">{impact.risk.level}</span>{/if}
          </div>
          {#if rules.length}
            {#each rules as r (r.name)}
              <div class="rule lv-{r.risk}">
                <Icon name="shield" size={17} />
                <div>
                  <div>Policy rule <strong>{r.name}</strong>: risk {r.risk}.</div>
                  <div class="mono small muted">
                    {r.approvals} approval{r.approvals === 1 ? '' : 's'}{r.labGate ? ', lab' : ''}{r.approvers?.length ? ` · ${r.approvers.join(', ')}` : ''}
                  </div>
                  <div class="mono small paths">
                    {#each r.paths.slice(0, 3) as p, i (i)}{#if i}{', '}{/if}{#each pathParts(p) as part, j (j)}<span class="seg">{part}</span><wbr />{/each}{/each}{r.paths.length > 3 ? ` +${r.paths.length - 3}` : ''}
                  </div>
                </div>
              </div>
            {/each}
          {/if}
          {#each objectReasons as r, i (i)}
            <div class="rule lv-{reasonLevel(r.reason)}">
              <Icon name="shield" size={17} />
              <div>
                <div>{r.reason[0].toUpperCase() + r.reason.slice(1)}.</div>
                <div class="mono small muted paths">
                  {RISK_SRC[r.source] ?? r.source}{#if r.object}{' · '}{#each pathParts(r.object.split('/').slice(-3).filter(Boolean).join('/')) as part, j (j)}<span class="seg">{part}</span><wbr />{/each}{/if}
                </div>
              </div>
            </div>
          {/each}
          {#if impact && !rules.length && !objectReasons.length}
            <p class="muted">No rule matched. Default risk.</p>
          {/if}
        </section>

        {#if gates}
          <GatesCard {c} {gates} />
        {/if}

        {#if impact && !impact.overlaps.length}
          <section class="card">
            <h3 class="eyebrow">Overlap</h3>
            <p class="ok-line"><Icon name="check" size={15} /> No other open change touches these objects.</p>
          </section>
        {/if}
      </aside>
    </div>
  {/if}
</div>

<style>
  .review {
    display: grid;
    gap: 18px;
    align-content: start;
    /* The columns follow the room of the page, not the window: the side
       panel and the rail take room too. */
    container: review / inline-size;
  }
  .head .eyebrow a {
    text-transform: none;
    letter-spacing: 0;
  }
  .head {
    display: grid;
    gap: 8px;
  }
  .titlerow {
    display: flex;
    gap: 12px;
    align-items: center;
    flex-wrap: wrap;
  }
  h1 {
    margin: 0;
  }
  .meta {
    display: flex;
    gap: 18px;
    flex-wrap: wrap;
    font-size: 14px;
    margin-bottom: 6px;
  }
  .grid {
    display: grid;
    grid-template-columns: minmax(0, 1fr) 340px;
    gap: 18px;
    align-items: start;
  }
  .maincol,
  .sidecol {
    display: grid;
    gap: 16px;
    min-width: 0;
  }
  @container review (max-width: 1000px) {
    .grid {
      grid-template-columns: minmax(0, 1fr);
    }
  }
  @container review (min-width: 640px) and (max-width: 1000px) {
    .sidecol {
      grid-template-columns: repeat(2, minmax(0, 1fr));
      align-items: start;
    }
  }
  .card {
    padding: 18px 22px;
    display: grid;
    gap: 12px;
    align-content: start;
  }
  .impact {
    /* navy, and orange in dark where navy is lost on the panel */
    border-top: 4px solid var(--strong-fill);
  }
  .cardhead {
    display: flex;
    justify-content: space-between;
    align-items: baseline;
    gap: 12px;
    flex-wrap: wrap;
  }
  .cardhead .eyebrow {
    margin: 0;
  }
  h2 {
    margin: 0;
    font-size: 22px;
    display: flex;
    gap: 10px;
    align-items: center;
  }
  h2 .mono {
    font-size: 15px;
    font-weight: 400;
  }
  .risk {
    font: 600 13px var(--sans);
    border-radius: 5px;
    padding: 2px 8px;
    background: var(--panel-2);
    border: 1px solid var(--line);
  }
  .r-high {
    background: var(--bad-soft);
    color: var(--bad);
    border-color: transparent;
  }
  .r-medium {
    background: var(--warn-soft);
    color: var(--warn);
    border-color: transparent;
  }
  .r-low {
    background: var(--ok-soft);
    color: var(--ok);
    border-color: transparent;
  }
  .cluster {
    display: grid;
    gap: 6px;
  }
  .clname {
    display: flex;
    gap: 12px;
    align-items: baseline;
    flex-wrap: wrap;
  }
  .clname .eyebrow {
    margin: 0;
  }
  .strong {
    font-weight: 700;
    font-size: 20px;
  }
  .counts {
    display: inline-flex;
    gap: 12px;
  }
  .cnt {
    font: 700 28px/1 var(--mono);
    letter-spacing: -0.02em;
  }
  .objs {
    font-size: 16px;
    color: var(--ink);
  }
  .ks {
    border-left: 3px solid var(--line);
    padding: 2px 0 2px 18px;
    display: grid;
    gap: 6px;
  }
  .obj {
    display: grid;
    grid-template-columns: 150px 18px minmax(0, 1fr) auto;
    align-items: baseline;
    gap: 8px;
    font-size: 15px;
  }
  .kname {
    color: var(--muted);
  }
  .oname {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .sym {
    font: 700 15px var(--mono);
    text-align: center;
  }
  .op-add {
    color: var(--ok);
  }
  .op-change {
    color: var(--accent);
  }
  .op-delete {
    color: var(--bad);
  }
  .src {
    color: var(--muted);
  }
  .src:hover {
    color: var(--ink);
  }
  .quiet {
    margin: 0;
  }
  .steps {
    margin: 4px 0 0;
    padding-left: 18px;
  }
  .tabs {
    display: flex;
    align-items: center;
    gap: 4px;
    border-bottom: 1px solid var(--line);
    margin-top: 6px;
  }
  .tabs .expand {
    margin-left: auto;
  }
  .tabs button {
    border: 0;
    background: none;
    padding: 8px 16px;
    font-size: 15px;
    color: var(--muted);
    border-bottom: 3px solid transparent;
    margin-bottom: -1px;
  }
  .tabs button.on {
    color: var(--ink);
    font-weight: 700;
    border-bottom-color: var(--accent-fill);
  }
  .filediff {
    border: 1px solid var(--line);
    border-radius: 6px;
    overflow: hidden;
  }
  .filediff summary {
    display: flex;
    gap: 10px;
    align-items: center;
    padding: 8px 12px;
    background: var(--panel-2);
    cursor: pointer;
    font-size: 14px;
  }
  .filediff summary .mono {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .flag {
    font: 700 13px var(--mono);
    width: 14px;
  }
  .f-added {
    color: var(--ok);
  }
  .f-modified,
  .f-renamed {
    color: var(--accent);
  }
  .f-deleted {
    color: var(--bad);
  }
  .rgroup {
    display: grid;
    gap: 6px;
  }
  .rgroup .eyebrow {
    margin: 4px 0 0;
  }
  .robj {
    border: 1px solid var(--line);
    border-radius: 6px;
    overflow: hidden;
  }
  .rhead {
    display: flex;
    gap: 8px;
    align-items: center;
    width: 100%;
    border: 0;
    background: var(--panel-2);
    color: var(--ink);
    padding: 7px 10px;
    text-align: left;
    font-size: 14px;
  }
  .rhead .mono:not(.src) {
    flex: 1;
  }
  .overlaps {
    display: grid;
    gap: 10px;
  }
  .advisor {
    border-left: 4px solid var(--ai-fill);
    /* Below the sticky agent banner, when one is on the page. */
    scroll-margin-top: calc(12px + var(--agent-banner-h, 0px));
  }
  /* Room to scroll the card to the top while the advisor works. */
  .advisor.tall {
    min-height: calc(100vh - var(--topbar-h) - 40px);
  }
  .advisor h2 {
    font-size: 18px;
  }
  .advisor h2 :global(svg) {
    color: var(--ai);
  }
  .abuttons {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
  }
  /* The recipe asked last is pressed (filled, like the deck's chosen
     recipe); its spinner is the global .spin in the button colour. */
  .abuttons .spin {
    width: 14px;
    height: 14px;
  }
  .aresult {
    padding-top: 4px;
    min-width: 0;
  }
  /* The advisor card is the frame: the answer inside has no second one. */
  .aresult :global(.adv),
  .aresult :global(.work) {
    border: 0;
    border-top: 1px solid var(--line);
    border-radius: 0;
    background: transparent;
    padding: 14px 0 0;
  }
  .labpass {
    display: inline-flex;
    align-items: center;
    font-size: 11.5px;
    font-weight: 600;
    padding: 1px 8px;
    border-radius: 4px;
    background: var(--ok-soft);
    color: var(--ok);
  }
  .rule {
    display: flex;
    gap: 10px;
    align-items: flex-start;
    font-size: 14.5px;
  }
  .rule > :global(svg) {
    color: var(--warn);
    flex: none;
    margin-top: 2px;
  }
  .rule.lv-high > :global(svg) {
    color: var(--bad);
  }
  .rule.lv-low > :global(svg) {
    color: var(--ok);
  }
  .paths {
    color: var(--muted);
  }
  /* Paths break after a "/" only, never inside a file name. */
  .paths .seg {
    white-space: nowrap;
  }
  .ok-line {
    margin: 0;
    display: flex;
    gap: 6px;
    align-items: center;
    color: var(--ok);
    font-size: 14px;
  }
</style>
