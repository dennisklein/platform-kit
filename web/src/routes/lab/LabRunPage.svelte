<script lang="ts">
  // One lab run: steps live (lab.step / lab.finished events), scenario
  // results, the result with lab gate, report and the trailer the merge
  // commit gets. /changes/:id/lab shows the latest run (or ?run=),
  // /labs/runs/:run any run.
  import { onMount, untrack } from 'svelte';
  import type { Params } from '../../lib/router.svelte';
  import { href, router } from '../../lib/router.svelte';
  import { api, must, errorText } from '../../lib/api/client';
  import type { Gates, LabRun, LabStep, LabStepEvent } from '../../lib/api/types';
  import { change } from '../../lib/stores/change.svelte';
  import { events } from '../../lib/stores/events.svelte';
  import { actions, nextStep } from '../../lib/change/actions.svelte';
  import { duration, runLabel, shortSha, timeAgo } from '../../lib/change/format';
  import Track, { type TrackItem } from '../../lib/components/Track.svelte';
  import Icon from '../../lib/components/Icon.svelte';
  import AgentBanner from '../../lib/components/AgentBanner.svelte';
  import { presenceFor } from '../../lib/change/presence.svelte';

  let { params, query }: { params: Params; query: URLSearchParams } = $props();

  // The run id: from the path (/labs/runs/:run), ?run=, or the latest run of the change.
  const changeParam = $derived(params.id as string | undefined);
  const wanted = $derived(params.run ? decodeURIComponent(params.run) : (query.get('run') ?? ''));

  let run = $state<LabRun | null>(null);
  let runs = $state<LabRun[]>([]);
  let error = $state<string | null>(null);
  let gates = $state<Gates | null>(null);
  let selected = $state<string | undefined>(undefined);
  let pinnedStep = false;

  const changeId = $derived(run?.changeId ?? changeParam);
  $effect(() => change.use(changeId ?? null));
  const c = $derived(changeId && change.data?.id === changeId ? change.data : null);
  // An agent at work on this change (its lab run, a denied approve).
  const agentPresence = $derived(presenceFor(changeId ?? undefined, c?.presence));

  async function loadRuns() {
    if (!changeId) return;
    try {
      const l = await must(api.GET('/labs', { params: { query: { changeId } } }));
      runs = [...l.runs].sort((a, b) => b.number - a.number);
    } catch {
      runs = [];
    }
  }

  async function loadRun(id: string) {
    try {
      const r = await must(api.GET('/labs/runs/{run}', { params: { path: { run: id } } }));
      if (r.id !== (wanted || latestId)) return;
      run = r;
      error = null;
      if (!pinnedStep) selected = current(r)?.name;
    } catch (e) {
      error = errorText(e);
    }
  }

  async function loadGates() {
    if (!changeId) return;
    try {
      gates = await must(api.GET('/changes/{id}/gates', { params: { path: { id: changeId } } }));
    } catch {
      gates = null;
    }
  }

  let latestId = $state('');
  // Without ?run: the newest run of the change.
  $effect(() => {
    const w = wanted;
    const cp = changeParam;
    untrack(async () => {
      run = null;
      error = null;
      pinnedStep = false;
      if (w) {
        await loadRun(w);
      } else if (cp) {
        try {
          const l = await must(api.GET('/labs', { params: { query: { changeId: cp } } }));
          runs = [...l.runs].sort((a, b) => b.number - a.number);
          latestId = runs[0]?.id ?? '';
          if (latestId) await loadRun(latestId);
          else error = null;
        } catch (e) {
          error = errorText(e);
        }
      }
      void loadRuns();
      void loadGates();
    });
  });

  /** The step to show: the running one, the first failed, else the last. */
  function current(r: LabRun): LabStep | undefined {
    return (
      r.steps.find((s) => s.status === 'running') ??
      r.steps.find((s) => s.status === 'fail') ??
      [...r.steps].reverse().find((s) => s.status !== 'pending' && s.status !== 'skipped')
    );
  }

  onMount(() => {
    const offs = [
      events.on<LabStepEvent>('lab.step', (d) => {
        if (!run || d.run !== run.id) return;
        const steps = [...run.steps];
        const i = steps.findIndex((s) => s.index === d.step.index || s.name === d.step.name);
        if (i >= 0) steps[i] = d.step;
        else steps.push(d.step);
        steps.sort((a, b) => a.index - b.index);
        run = { ...run, steps };
        if (!pinnedStep && (d.step.status === 'running' || d.step.status === 'fail')) selected = d.step.name;
      }),
      events.on<LabRun>('lab.finished', (d) => {
        if (run && d.id === run.id) {
          run = d;
          if (!pinnedStep) selected = current(d)?.name;
          void loadGates();
          void loadRuns();
        } else if (changeId && d.changeId === changeId) void loadRuns();
      }),
    ];
    // Fallback while running, when events are not connected.
    const poll = setInterval(() => {
      if (run?.result === 'running' && !events.connected) void loadRun(run.id);
    }, 3000);
    return () => {
      offs.forEach((o) => o());
      clearInterval(poll);
    };
  });

  const STATUS: Record<string, TrackItem['status']> = {
    pass: 'done',
    running: 'running',
    fail: 'fail',
    pending: 'todo',
    skipped: 'skipped',
  };
  const items = $derived<TrackItem[]>(
    (run?.steps ?? []).map((s) => ({ key: s.name, title: s.title, sub: s.detail, status: STATUS[s.status] ?? 'todo' })),
  );
  const step = $derived(run?.steps.find((s) => s.name === selected));
  const scenario = $derived((run?.steps ?? []).filter((s) => s.scenario));
  const running = $derived(run?.result === 'running');
  const head = $derived(c?.headSha);
  // The report commit moves the head; the bench still counts the run for
  // the head (policy.LabCoversHead), for a pass (met) and for a fail
  // (failed) alike. "needed" with this run means it tested an older head.
  const forHead = $derived(
    !!run?.sha &&
      !!head &&
      (run.sha === head || (gates?.labGate.run === run.id && ['met', 'failed', 'running', 'na'].includes(gates.labGate.status))),
  );
  const gateText = $derived.by(() => {
    if (!run) return '';
    if (run.result === 'running') return 'running';
    if (run.result !== 'pass') return 'not met';
    if (gates?.labGate.run === run.id) return gates.labGate.status === 'met' ? 'met' : gates.labGate.status;
    return forHead ? 'met' : 'old head';
  });
  // After a pass that counts: the next step of the change, as in the talk
  // ("Push and open merge request").
  const next = $derived(c && run?.result === 'pass' && gateText === 'met' ? nextStep(c) : null);
  const trailer = $derived(run ? `Pk-Lab-Run: ${run.id} ${run.result}${run.report ? ` ${run.report}` : ''}` : '');
  const reportHref = $derived(run?.report ? href('/kb/article', { path: run.report, ref: run.changeId }) : '');

  const TYPE_LABEL: Record<string, string> = { check: 'check', bmc: 'bmc', fault: 'fault', wait: 'wait', exec: 'exec' };
  function scenarioTitle(s: LabStep): string {
    const t = TYPE_LABEL[s.type] ?? s.type;
    return `${t}: ${s.title}`;
  }

  function pick(key: string | undefined) {
    selected = key;
    pinnedStep = true;
  }

  function openRun(id: string) {
    if (changeParam) router.navigate(`/changes/${encodeURIComponent(changeParam)}/lab?run=${encodeURIComponent(id)}`);
    else router.navigate(`/labs/runs/${encodeURIComponent(id)}`);
  }

  let copied = $state(false);
  async function copyTrailer() {
    try {
      await navigator.clipboard.writeText(trailer);
      copied = true;
      setTimeout(() => (copied = false), 1500);
    } catch {
      /* clipboard blocked */
    }
  }
</script>

<div class="page lab">
  {#if agentPresence}<AgentBanner presence={agentPresence} />{/if}
  {#if error}
    <div class="error-box">{error}</div>
  {:else if !run}
    {#if changeParam && !wanted && runs.length === 0 && c}
      <div class="card empty">
        <Icon name="flask" size={26} />
        <h2>No lab run yet</h2>
        <p class="muted">The change lab boots the changed machines in an emulated twin of {c.impact?.clusters?.[0]?.cluster ?? 'the cluster'}, runs the scenario and writes a report into the change.</p>
        <button class="btn dark" onclick={() => actions.runLab(c.id)} disabled={actions.isBusy(c.id)}>
          <Icon name="flask" size={15} />{actions.isBusy(c.id, 'lab') ? 'Starting…' : 'Run lab'}
        </button>
      </div>
    {:else}
      <p class="muted">Loading lab run…</p>
    {/if}
  {:else}
    <header class="head">
      <div class="eyebrow">
        Lab{#if run.changeId}&nbsp;·&nbsp;<a class="mono" href={href(`/changes/${run.changeId}`)}>{run.changeId}</a>{/if}
        {#if c}<span class="ctitle">{c.title}</span>{/if}
      </div>
      <div class="titlerow">
        <h1>Change lab · {runLabel(run.id)}</h1>
        <span class="result r-{run.result}">{#if running}<span class="pulse"></span>{/if}{run.result}</span>
        <span class="grow"></span>
        <span class="muted where">
          lab host {run.host ?? 'local'} · {run.provision ?? 'metal'} provisioning · {run.fresh === false ? 'twin snapshot' : 'fresh machines'}
        </span>
      </div>
      <div class="meta muted">
        <span>{run.cluster ? `twin of ${run.cluster}` : run.lab}{run.nodes?.length ? ` · ${run.nodes.join(', ')}` : ''}</span>
        <span>driver {run.driver}{run.fidelity ? ` · ${run.fidelity}` : ''}</span>
        {#if run.sha}<span class="mono" class:warn-text={!forHead && !!head} title={forHead ? 'branch head' : 'not the branch head'}>{shortSha(run.sha)}{!forHead && head ? ' (old head)' : ''}</span>{/if}
        <span>
          {running ? `started ${timeAgo(run.startedAt)}` : `${duration(run.durationMs)}${run.simulatedMs ? ` (lab time ${duration(run.simulatedMs)})` : ''}`}
        </span>
        {#if run.by}<span>by {run.by}</span>{/if}
      </div>
      <div class="row">
        {#if c}
          <button class="btn" class:dark={!running && (run.result !== 'pass' || !forHead)} onclick={() => actions.runLab(c.id)} disabled={running || actions.isBusy(c.id)}>
            <Icon name="flask" size={15} />{actions.isBusy(c.id, 'lab') ? 'Starting…' : 'Run lab again'}
          </button>
          <a class="btn" href={href(`/changes/${c.id}`)}><Icon name="branch" size={15} />Change</a>
          <a class="btn" href={href(`/changes/${c.id}/review`, { advisor: 'lab-gaps' })}><Icon name="sparkle" size={15} />What does the lab not test?</a>
        {/if}
        {#if runs.length > 1}
          <select class="select" value={run.id} onchange={(e) => openRun(e.currentTarget.value)} aria-label="Run">
            {#each runs as r (r.id)}<option value={r.id}>{runLabel(r.id)} · {r.result}{r.sha ? ` · ${shortSha(r.sha)}` : ''}</option>{/each}
          </select>
        {/if}
      </div>
    </header>

    <section class="card trackcard">
      <Track {items} selectable bind:selected={() => selected, (k) => pick(k)} />
    </section>

    <div class="two">
      <section class="card">
        <h3 class="eyebrow">Scenario{run.scenario ? ` · ${run.scenario}` : ''}</h3>
        {#each scenario as s (s.name)}
          <button class="sline" class:sel={selected === s.name} onclick={() => pick(s.name)}>
            <span class="ic st-{s.status}">
              {#if s.status === 'pass'}<Icon name="check" size={16} />
              {:else if s.status === 'fail'}<Icon name="x" size={16} />
              {:else if s.status === 'running'}<span class="spin"></span>
              {:else}<Icon name="dot" size={16} />{/if}
            </span>
            <span class="stitle">{scenarioTitle(s)}</span>
            <span class="mono small muted sdetail">{s.detail ?? ''}</span>
          </button>
        {:else}
          <p class="muted">No scenario steps.</p>
        {/each}
        {#if run.failures?.length}
          <div class="callout bad">
            <Icon name="x" size={16} />
            <ul class="fails">{#each run.failures as f (f)}<li>{f}</li>{/each}</ul>
          </div>
        {/if}
      </section>

      <section class="card">
        <h3 class="eyebrow">Result</h3>
        <div class="rline">
          <span class="ic" class:ok={gateText === 'met'} class:bad={gateText === 'not met'}>
            <Icon name={gateText === 'met' ? 'check' : gateText === 'not met' ? 'x' : 'flask'} size={16} />
          </span>
          <span>Lab gate of <span class="mono">{run.gatePaths?.[0] ?? 'the change'}</span>{(run.gatePaths?.length ?? 0) > 1 ? ` +${run.gatePaths!.length - 1}` : ''}</span>
          <span class="mono small right" class:warn-text={gateText === 'old head'}>{gateText}</span>
        </div>
        <div class="rline">
          <span class="ic"><Icon name="kb" size={16} /></span>
          <span>Report</span>
          {#if reportHref && !running}
            <a class="mono small right" href={reportHref}>{run.report}</a>
          {:else}
            <span class="mono small right muted">{running ? 'after the run' : 'none'}</span>
          {/if}
        </div>
        <div class="rline">
          <span class="ic"><Icon name="file" size={16} /></span>
          <span>Trailer</span>
          <button class="mono small right trailer" onclick={copyTrailer} title="Copy. The merge commit gets this trailer.">
            {copied ? 'copied' : `Pk-Lab-Run: ${run.id} ${run.result}`}
          </button>
        </div>
        {#if run.checksTotal}
          <div class="rline">
            <span class="ic" class:ok={run.result === 'pass'} class:bad={run.result === 'fail'}>
              <Icon name={run.result === 'pass' ? 'check' : run.result === 'fail' ? 'x' : 'flask'} size={16} />
            </span>
            <span>Checks</span>
            <span class="mono small right">{run.summary ?? `${run.checksPassed} of ${run.checksTotal}`}</span>
          </div>
        {/if}
        {#if c && next && ['push', 'ready', 'merge'].includes(next)}
          <div class="nextrow">
            {#if next === 'push'}
              <button class="btn dark" onclick={() => actions.push(c.id)} disabled={actions.isBusy(c.id)}>
                {#if actions.isBusy(c.id, 'push')}<span class="spin"></span>{:else}<Icon name="upload" size={15} />{/if}
                {c.pushed ? 'Push to the merge request' : 'Push and open merge request'}
              </button>
            {:else if next === 'ready'}
              <button class="btn dark" onclick={() => actions.ready(c.id)} disabled={actions.isBusy(c.id)}>
                {#if actions.isBusy(c.id, 'ready')}<span class="spin"></span>{:else}<Icon name="check" size={15} />{/if}
                Mark ready for review
              </button>
            {:else}
              <a class="btn dark" href={href(`/changes/${c.id}`)}><Icon name="merge" size={15} />To the merge</a>
            {/if}
            {#if c.mr}<a class="small" href={c.mr.url} target="_blank" rel="noreferrer">MR !{c.mr.number} · {c.mr.draft ? 'draft' : 'review'}</a>{/if}
          </div>
        {/if}
      </section>
    </div>

    {#if step}
      <section class="card stepcard">
        <div class="stephead">
          <h3 class="eyebrow">Step {step.index + 1} · {step.name}</h3>
          <span class="muted small">{step.type}{step.durationMs ? ` · ${duration(step.durationMs)}` : ''}{step.status === 'running' ? ' · running' : ''}</span>
        </div>
        {#if step.checks?.length}
          <ul class="checks">
            {#each step.checks as ck, i (i)}
              <li class:bad={!ck.ok}>
                <Icon name={ck.ok ? 'check' : 'x'} size={14} />
                <span>{ck.name}</span>
                <span class="muted">{ck.detail ?? ''}</span>
              </li>
            {/each}
          </ul>
        {/if}
        <pre class="log mono">{(step.output ?? []).join('\n') || (step.status === 'pending' ? 'not started' : 'no output')}</pre>
      </section>
    {/if}
  {/if}
</div>

<style>
  .nextrow {
    display: flex;
    gap: 12px;
    align-items: center;
    margin-top: 6px;
    padding-top: 12px;
    border-top: 1px solid var(--line);
  }
  .lab {
    display: grid;
    gap: 16px;
    align-content: start;
  }
  .head {
    display: grid;
    gap: 8px;
  }
  .eyebrow a {
    text-transform: none;
    letter-spacing: 0;
  }
  .ctitle {
    text-transform: none;
    letter-spacing: 0;
    font-family: var(--sans);
    margin-left: 10px;
    color: var(--muted);
  }
  .titlerow {
    display: flex;
    gap: 12px;
    align-items: center;
    flex-wrap: wrap;
  }
  .grow {
    flex: 1;
  }
  h1 {
    margin: 0;
    font-size: 24px;
  }
  .where {
    font-size: 15px;
  }
  .meta {
    display: flex;
    gap: 18px;
    flex-wrap: wrap;
    font-size: 14px;
  }
  .row {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
    align-items: center;
    margin-top: 4px;
  }
  .select {
    padding: 5px 8px;
  }
  .result {
    font: 700 14px var(--sans);
    padding: 2px 10px;
    border-radius: 5px;
    display: inline-flex;
    gap: 6px;
    align-items: center;
  }
  .r-pass {
    background: var(--ok-soft);
    color: var(--ok);
  }
  .r-fail {
    background: var(--bad-soft);
    color: var(--bad);
  }
  .r-running {
    background: var(--warn-soft);
    color: var(--warn);
  }
  .r-cancelled {
    background: var(--panel-2);
    color: var(--muted);
  }
  .pulse {
    width: 8px;
    height: 8px;
    border-radius: 50%;
    background: currentColor;
    animation: pulse 1.2s ease-in-out infinite;
  }
  @keyframes pulse {
    50% {
      opacity: 0.3;
    }
  }
  .card {
    padding: 16px 20px;
    display: grid;
    gap: 6px;
    align-content: start;
  }
  .card .eyebrow {
    margin: 0 0 6px;
  }
  .trackcard {
    padding: 14px 10px 10px;
    overflow-x: auto;
  }
  .two {
    display: grid;
    grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
    gap: 16px;
  }
  @media (max-width: 1100px) {
    .two {
      grid-template-columns: minmax(0, 1fr);
    }
  }
  .sline {
    display: grid;
    grid-template-columns: 22px minmax(0, 1fr) auto;
    gap: 8px;
    align-items: center;
    border: 0;
    background: none;
    color: var(--ink);
    padding: 5px 6px;
    margin: 0 -6px;
    border-radius: 5px;
    text-align: left;
    font-size: 15px;
  }
  .sline:hover,
  .sline.sel {
    background: var(--bg);
  }
  .sdetail {
    white-space: nowrap;
  }
  .ic {
    display: inline-flex;
    color: var(--muted);
  }
  .ic.ok,
  .st-pass {
    color: var(--ok);
  }
  .ic.bad,
  .st-fail {
    color: var(--bad);
  }
  .st-running {
    color: var(--warn);
  }
  .spin {
    width: 13px;
    height: 13px;
    border: 2px solid currentColor;
    border-right-color: transparent;
    border-radius: 50%;
    animation: spin 0.8s linear infinite;
  }
  @keyframes spin {
    to {
      transform: rotate(360deg);
    }
  }
  .rline {
    display: grid;
    grid-template-columns: 22px auto minmax(0, 1fr);
    gap: 8px;
    align-items: center;
    padding: 5px 0;
    font-size: 15px;
  }
  .right {
    text-align: right;
    justify-self: end;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    max-width: 100%;
  }
  .trailer {
    border: 0;
    background: none;
    color: var(--ink);
    padding: 0;
    cursor: copy;
  }
  .fails {
    margin: 0;
    padding-left: 16px;
  }
  .stephead {
    display: flex;
    justify-content: space-between;
    align-items: baseline;
  }
  .checks {
    list-style: none;
    margin: 0 0 6px;
    padding: 0;
    display: grid;
    gap: 4px;
    font-size: 14px;
  }
  .checks li {
    display: flex;
    gap: 8px;
    align-items: center;
    color: var(--ok);
  }
  .checks li span:first-of-type {
    color: var(--ink);
  }
  .checks li.bad {
    color: var(--bad);
  }
  .log {
    margin: 0;
    background: var(--code-bg);
    border: 1px solid var(--line);
    border-radius: 6px;
    padding: 10px 12px;
    font-size: 13px;
    line-height: 1.6;
    max-height: 320px;
    overflow: auto;
    white-space: pre-wrap;
  }
  .empty {
    justify-items: center;
    text-align: center;
    padding: 40px;
    gap: 10px;
  }
  .empty h2 {
    margin: 0;
  }
  .empty p {
    max-width: 520px;
    margin: 0 0 6px;
  }
</style>
