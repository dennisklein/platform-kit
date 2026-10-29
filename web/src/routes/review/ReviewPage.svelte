<script lang="ts">
  // Review a change by its effect: impact per cluster (grouped by
  // Kustomization, with links to the source lines), the diff as semantic,
  // source or rendered view, overlaps, the risk with the policy rules that
  // matched, the gates, and the advisor buttons (LLM, advisory only).
  import { onMount, untrack } from 'svelte';
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
  let results = $state<Result[]>([]);
  let running = $state<string | null>(null);
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

  // ?advisor=risks runs that recipe once (the "Ask advisor: risks" button).
  let autoRan = '';
  $effect(() => {
    const r = query.get('advisor');
    const key = `${id}:${r}`;
    if (r && autoRan !== key) {
      autoRan = key;
      untrack(() => void ask(r));
    }
  });

  async function ask(recipe: string) {
    running = recipe;
    advisorError = null;
    try {
      const r = await must(api.POST('/changes/{id}/advisors/{recipe}', { params: { path: { id, recipe } } }));
      results = [r, ...results.filter((x) => x.recipe !== recipe)];
    } catch (e) {
      advisorError = errorText(e);
    } finally {
      running = null;
    }
  }

  function setView(v: DiffViewKind) {
    const q = new URLSearchParams(query);
    q.set('tab', v);
    q.delete('advisor');
    router.replace(`/changes/${id}/review?${q}`);
  }

  // --- impact -----------------------------------------------------------------
  const changedClusters = $derived((impact?.clusters ?? []).filter((x) => x.kustomizations.some((k) => k.objects.length)));
  const quiet = $derived((impact?.clusters ?? []).filter((x) => !x.kustomizations.some((k) => k.objects.length)).map((x) => x.cluster));
  const SYM: Record<string, string> = { add: '+', change: '~', delete: '−' };

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
        <StateChip state={c.state} labState={c.labState} />
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
                  <span class="muted small">{cl.objectsBase} → {cl.objectsHead} objects{cl.ring ? ` · ring ${cl.ring}` : ''}</span>
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
        <section class="card advisor" id="advisor">
          <div class="cardhead">
            <h2><Icon name="sparkle" size={18} /> Advisor</h2>
            <span class="muted small">LLM notes, advisory only. They do not change the rating or the gates.</span>
          </div>
          <div class="abuttons">
            {#each recipes as r (r.id)}
              <button class="btn" onclick={() => ask(r.id)} disabled={!!running} title={r.question}>
                <Icon name="sparkle" size={14} />{running === r.id ? 'Asking…' : r.title}
              </button>
            {:else}
              <button class="btn" onclick={() => ask('risks')} disabled={!!running}><Icon name="sparkle" size={14} />Risks of this change</button>
            {/each}
          </div>
          {#if advisorError}<div class="error-box">{advisorError}</div>{/if}
          {#each results as r (r.recipe)}
            <div class="aresult"><AdvisorResult result={r} showReading /></div>
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
              <div class="rule">
                <Icon name="shield" size={17} />
                <div>
                  <div>Policy rule <strong>{r.name}</strong>: risk {r.risk}.</div>
                  <div class="mono small muted">
                    {r.approvals} approval{r.approvals === 1 ? '' : 's'}{r.labGate ? ', lab' : ''}{r.approvers?.length ? ` · ${r.approvers.join(', ')}` : ''}
                  </div>
                  <div class="mono small paths">{r.paths.slice(0, 3).join(', ')}{r.paths.length > 3 ? ` +${r.paths.length - 3}` : ''}</div>
                </div>
              </div>
            {/each}
          {/if}
          {#each objectReasons as r, i (i)}
            <div class="rule">
              <Icon name="shield" size={17} />
              <div>
                <div>{r.reason[0].toUpperCase() + r.reason.slice(1)}.</div>
                <div class="mono small muted">{RISK_SRC[r.source] ?? r.source}{r.object ? ` · ${r.object.split('/').slice(-3).filter(Boolean).join('/')}` : ''}</div>
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
    font-size: 24px;
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
  @media (max-width: 1500px) {
    .grid {
      grid-template-columns: minmax(0, 1fr);
    }
  }
  .card {
    padding: 18px 22px;
    display: grid;
    gap: 12px;
    align-content: start;
  }
  .impact {
    border-top: 4px solid var(--gsi-navy);
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
    gap: 10px;
    align-items: baseline;
  }
  .clname .eyebrow {
    margin: 0;
  }
  .strong {
    font-weight: 700;
    font-size: 17px;
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
    border-left: 4px solid var(--info);
  }
  .advisor h2 {
    font-size: 18px;
  }
  .advisor h2 :global(svg) {
    color: var(--info);
  }
  .abuttons {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
  }
  .aresult {
    border-top: 1px solid var(--line);
    padding-top: 12px;
  }
  .rule {
    display: flex;
    gap: 10px;
    align-items: flex-start;
    font-size: 14.5px;
  }
  .rule > :global(svg) {
    color: var(--bad);
    flex: none;
    margin-top: 2px;
  }
  .paths {
    color: var(--muted);
    overflow-wrap: anywhere;
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
