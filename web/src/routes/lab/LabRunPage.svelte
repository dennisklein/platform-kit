<script lang="ts">
  // One lab run: steps live (lab.step / lab.finished events), a console
  // with the output of every step, scenario results, the result with lab
  // gate, report and the trailer the merge commit gets. /changes/:id/lab
  // shows the latest run (or ?run=), /labs/runs/:run any run. A run that
  // the bench has no record of (seeded runs) is shown from its report.
  import { onMount, tick, untrack } from 'svelte';
  import { fade } from 'svelte/transition';
  import type { Params } from '../../lib/router.svelte';
  import { href, router } from '../../lib/router.svelte';
  import { api, must, errorText, ApiError } from '../../lib/api/client';
  import type { Gates, LabRun, LabRunRef, LabStep, LabStepEvent } from '../../lib/api/types';
  import { change } from '../../lib/stores/change.svelte';
  import { events } from '../../lib/stores/events.svelte';
  import { actions, nextStep } from '../../lib/change/actions.svelte';
  import { presenceFor } from '../../lib/change/presence.svelte';
  import { duration, runLabel, shortSha, timeAgo } from '../../lib/change/format';
  import Track, { type TrackItem } from '../../lib/components/Track.svelte';
  import Icon from '../../lib/components/Icon.svelte';
  import AgentBanner from '../../lib/components/AgentBanner.svelte';
  import Markdown from '../../lib/components/Markdown.svelte';
  import { changeOfRun, consoleLines, elapsed, parseReport, reportStatus } from './labview';

  let { params, query }: { params: Params; query: URLSearchParams } = $props();

  // The run id: from the path (/labs/runs/:run), ?run=, or the latest run of the change.
  const changeParam = $derived(params.id as string | undefined);
  const wanted = $derived(params.run ? decodeURIComponent(params.run) : (query.get('run') ?? ''));

  let run = $state<LabRun | null>(null);
  let runs = $state<LabRun[]>([]);
  let error = $state<string | null>(null);
  let notFound = $state(false);
  let loaded = $state(false);
  let gates = $state<Gates | null>(null);
  let selected = $state<string | undefined>(undefined);
  let pinned = $state(false);

  const changeId = $derived(run?.changeId ?? changeParam ?? changeOfRun(wanted));
  $effect(() => change.use(changeId ?? null));
  const c = $derived(changeId && change.data?.id === changeId ? change.data : null);
  const presence = $derived(presenceFor(changeId, c?.presence));
  const open = $derived(!!c && !['merged', 'deployed', 'verified', 'closed'].includes(c.state));

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
      if (run?.id !== r.id) revealed = {};
      run = r;
      error = null;
      notFound = false;
      if (!pinned) selected = current(r)?.name;
    } catch (e) {
      notFound = e instanceof ApiError && e.status === 404;
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
      notFound = false;
      loaded = false;
      pinned = false;
      revealed = {};
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
      loaded = true;
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

  // Output lines stream into the console one by one after a step ends.
  // revealed holds the lines shown so far of the steps that still stream.
  let revealed = $state<Record<string, number>>({});
  const streaming = $derived(Object.keys(revealed).length > 0);
  function streamTick() {
    if (!run) return;
    const order = [...run.steps].sort((a, b) => a.index - b.index);
    for (const s of order) {
      const n = revealed[s.name];
      if (n === undefined) continue;
      const total = s.output?.length ?? 0;
      const next = { ...revealed };
      if (n + 1 >= total) delete next[s.name];
      else next[s.name] = n + 1;
      revealed = next;
      return;
    }
  }

  // A clock for the elapsed times while the run runs.
  let now = $state(Date.now());

  onMount(() => {
    const offs = [
      events.on<LabStepEvent>('lab.step', (d) => {
        if (!run || d.run !== run.id) return;
        const steps = [...run.steps];
        const i = steps.findIndex((s) => s.index === d.step.index || s.name === d.step.name);
        const before = i >= 0 ? steps[i] : undefined;
        if (i >= 0) steps[i] = d.step;
        else steps.push(d.step);
        steps.sort((a, b) => a.index - b.index);
        // A step that ends now streams its lines; one seen ended stays as it is.
        const ended = d.step.status === 'pass' || d.step.status === 'fail';
        if (ended && before?.status !== d.step.status && (d.step.output?.length ?? 0) > 0) {
          revealed = { ...revealed, [d.step.name]: 0 };
        }
        run = { ...run, steps };
        if (!pinned && (d.step.status === 'running' || d.step.status === 'fail')) selected = d.step.name;
      }),
      events.on<LabRun>('lab.finished', (d) => {
        if (run && d.id === run.id) {
          run = d;
          if (!pinned) selected = current(d)?.name;
          void loadGates();
          void loadRuns();
        } else if (changeId && d.changeId === changeId) void loadRuns();
      }),
    ];
    // Fallback while running, when events are not connected.
    const poll = setInterval(() => {
      if (run?.result === 'running' && !events.connected) void loadRun(run.id);
    }, 3000);
    const stream = setInterval(streamTick, 140);
    const clock = setInterval(() => {
      if (run?.result === 'running') now = Date.now();
    }, 1000);
    return () => {
      offs.forEach((o) => o());
      clearInterval(poll);
      clearInterval(stream);
      clearInterval(clock);
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
  const scenario = $derived((run?.steps ?? []).filter((s) => s.scenario));
  const running = $derived(run?.result === 'running');
  const runClock = $derived(run && running ? elapsed(now - Date.parse(run.startedAt)) : '');
  const lines = $derived(run ? consoleLines(run.steps, revealed) : []);
  const pinnedStep = $derived(pinned ? run?.steps.find((s) => s.name === selected) : undefined);
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

  /** Pin a step: the console scrolls to it. Picking it again follows the run. */
  function pick(key: string | undefined) {
    if (!key || (pinned && selected === key)) {
      pinned = false;
      selected = run ? current(run)?.name : undefined;
      return;
    }
    selected = key;
    pinned = true;
  }

  function openRun(id: string) {
    if (changeParam) router.navigate(`/changes/${encodeURIComponent(changeParam)}/lab?run=${encodeURIComponent(id)}`);
    else router.navigate(`/labs/runs/${encodeURIComponent(id)}`);
  }

  // The console follows the run: it scrolls to the newest line, to the
  // pinned step, or (after a failed run) to the first failed step.
  let logEl = $state<HTMLDivElement | undefined>(undefined);
  const focusStep = $derived(
    pinned ? selected : run && !running && run.result === 'fail' && !streaming ? run.steps.find((s) => s.status === 'fail')?.name : undefined,
  );
  $effect(() => {
    void lines.length;
    const target = focusStep;
    const el = logEl;
    if (!el) return;
    void tick().then(() => {
      if (target) {
        const h = el.querySelector<HTMLElement>(`[data-step="${CSS.escape(target)}"]`);
        if (h) el.scrollTo({ top: Math.max(0, h.offsetTop - 8), behavior: 'smooth' });
      } else {
        el.scrollTo({ top: el.scrollHeight, behavior: 'smooth' });
      }
    });
  });

  let copied = $state(false);
  async function copyTrailer(text = trailer) {
    try {
      await navigator.clipboard.writeText(text);
      copied = true;
      setTimeout(() => (copied = false), 1500);
    } catch {
      /* clipboard blocked */
    }
  }

  // ---- A run the bench has no record of: show it from its report.
  /** The run reference of the change (trailer or metadata) the page asks for. */
  const reportRef = $derived.by<LabRunRef | undefined>(() => {
    if (run || !loaded || !c) return undefined;
    const refs = c.labRuns ?? [];
    if (wanted) return notFound ? refs.find((r) => r.run === wanted) : undefined;
    if (runs.length) return undefined;
    return [...refs].sort((a, b) => runNumber(b.run) - runNumber(a.run))[0];
  });
  function runNumber(id: string): number {
    return Number(id.match(/run-(\d+)$/)?.[1] ?? 0);
  }
  let report = $state<{ path: string; title: string; body: string } | null>(null);
  let reportError = $state<string | null>(null);
  $effect(() => {
    const ref = reportRef;
    const cid = c?.id;
    report = null;
    reportError = null;
    if (!ref?.report || !cid) return;
    const path = ref.report;
    untrack(async () => {
      try {
        const a = await must(api.GET('/kb/article', { params: { query: { path, ref: cid } } }));
        if (reportRef?.report === path) report = { path, title: a.title, body: a.body };
      } catch (e) {
        reportError = errorText(e);
      }
    });
  });
  const parsed = $derived(report ? parseReport(report.body) : null);
  const reportItems = $derived<TrackItem[]>(
    (parsed?.steps ?? []).map((s, i) => ({ key: `${i}`, title: s.title, sub: s.detail, status: reportStatus(s.result) })),
  );
  const reportResult = $derived(reportRef?.result ?? parsed?.result ?? 'unknown');
  const reportGate = $derived(
    reportRef && gates?.labGate.run === reportRef.run ? gates.labGate.status : reportResult === 'pass' ? 'met' : 'not met',
  );
  const reportTrailer = $derived(reportRef ? `Pk-Lab-Run: ${reportRef.run} ${reportRef.result}` : '');
</script>

<div class="page lab">
  {#if presence}<AgentBanner {presence} />{/if}

  {#if reportRef}
    <header class="head">
      <div class="eyebrow">
        Lab&nbsp;·&nbsp;<a class="mono" href={href(`/changes/${c!.id}`)}>{c!.id}</a>
        <span class="ctitle">{c!.title}</span>
      </div>
      <div class="titlerow">
        <h1>Change lab · {runLabel(reportRef.run)}</h1>
        <span class="result r-{reportResult}">{reportResult}</span>
        <span class="fromreport"><Icon name="kb" size={14} />from its report</span>
      </div>
      <div class="meta muted">
        <span>This bench has no step log of this run. Its report in the change has the steps and checks.</span>
        {#if reportRef.sha}<span class="mono" title="tested commit">{shortSha(reportRef.sha)}</span>{/if}
      </div>
      <div class="row">
        {#if c && open}
          <button class="btn dark" onclick={() => actions.runLab(c.id)} disabled={actions.isBusy(c.id)}>
            <Icon name="flask" size={15} />{actions.isBusy(c.id, 'lab') ? 'Starting…' : 'Run lab again'}
          </button>
        {/if}
        <a class="btn" href={href(`/changes/${c!.id}`)}><Icon name="branch" size={15} />Change</a>
        {#if reportRef.report}
          <a class="btn" href={href('/kb/article', { path: reportRef.report, ref: c!.id })}><Icon name="kb" size={15} />Open the report</a>
        {/if}
      </div>
    </header>

    {#if reportItems.length}
      <section class="card trackcard">
        <Track items={reportItems} />
      </section>
    {/if}

    <div class="two">
      <section class="card">
        <h3 class="eyebrow">Checks</h3>
        {#each parsed?.checks ?? [] as ck, i (i)}
          <div class="sline static">
            <span class="ic" class:st-pass={ck.ok} class:st-fail={!ck.ok}><Icon name={ck.ok ? 'check' : 'x'} size={16} /></span>
            <span class="stitle">{ck.text}</span>
            <span></span>
          </div>
        {:else}
          <p class="muted small">{report ? 'The report lists no checks.' : reportError ? `Report not readable: ${reportError}` : 'Loading the report…'}</p>
        {/each}
        {#if parsed?.failures.length}
          <div class="callout bad">
            <Icon name="x" size={16} />
            <ul class="fails">{#each parsed.failures as f (f)}<li>{f}</li>{/each}</ul>
          </div>
        {/if}
        {#each parsed?.notes ?? [] as n (n)}
          <div class="callout" class:warn={reportResult !== 'pass'}>
            <Icon name="info" size={16} />
            <div><Markdown source={n} path={report?.path} /></div>
          </div>
        {/each}
      </section>
      <section class="card">
        <h3 class="eyebrow">Result</h3>
        <div class="rline">
          <span class="ic" class:ok={reportGate === 'met'} class:bad={reportGate !== 'met'}>
            <Icon name={reportGate === 'met' ? 'check' : 'x'} size={16} />
          </span>
          <span>Lab gate</span>
          <span class="mono small right">{reportGate}</span>
        </div>
        <div class="rline">
          <span class="ic"><Icon name="kb" size={16} /></span>
          <span>Report</span>
          {#if reportRef.report}
            <a class="mono small right" href={href('/kb/article', { path: reportRef.report, ref: c!.id })}>{reportRef.report}</a>
          {:else}
            <span class="mono small right muted">none</span>
          {/if}
        </div>
        <div class="rline">
          <span class="ic"><Icon name="file" size={16} /></span>
          <span>Trailer</span>
          <button class="mono small right trailer" onclick={() => copyTrailer(reportTrailer)} title="Copy. The merge commit gets this trailer.">
            {copied ? 'copied' : reportTrailer}
          </button>
        </div>
      </section>
    </div>

    {#if report}
      <details class="card fullreport">
        <summary class="eyebrow">Full report · {report.path}</summary>
        <Markdown source={report.body} path={report.path} dropTitle={report.title} />
      </details>
    {/if}
  {:else if error}
    <div class="card empty">
      <Icon name="flask" size={26} />
      <h2>{notFound ? `Lab run ${wanted} not found` : 'The lab run could not be loaded'}</h2>
      <p class="muted">
        {notFound
          ? 'The bench has no record of this run, and the change lists no report for it.'
          : error}
      </p>
      <div class="row center">
        {#if changeId}<a class="btn dark" href={href(`/changes/${changeId}`)}><Icon name="branch" size={15} />Back to the change</a>{/if}
        <a class="btn" href={href('/labs')}><Icon name="labs" size={15} />All lab runs</a>
      </div>
    </div>
  {:else if !run}
    {#if changeParam && !wanted && runs.length === 0 && c && loaded}
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
        <span class="result r-{run.result}">
          {#if running}<span class="pulse"></span>running<span class="clock">{runClock}</span>{:else}{run.result}{/if}
        </span>
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
          {running ? `started ${timeAgo(run.startedAt, now)}` : `${duration(run.durationMs)}${run.simulatedMs ? ` (lab time ${duration(run.simulatedMs)})` : ''}`}
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
      <Track {items} selectable bind:selected={() => (pinned || running || run?.result === 'fail' ? selected : undefined), (k) => pick(k)} />
    </section>

    <div class="body" class:livefirst={running || streaming}>
      <section class="card scen">
        <h3 class="eyebrow">Scenario{run.scenario ? ` · ${run.scenario}` : ''}</h3>
        {#each scenario as s (s.name)}
          <button class="sline" class:sel={pinned && selected === s.name} onclick={() => pick(s.name)}>
            <span class="ic st-{s.status}">
              {#if s.status === 'pass'}<Icon name="check" size={16} />
              {:else if s.status === 'fail'}<Icon name="x" size={16} />
              {:else if s.status === 'running'}<span class="spin"></span>
              {:else}<span class="hollow" title={s.status}></span>{/if}
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

      <section class="card result-card">
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
          <button class="mono small right trailer" onclick={() => copyTrailer()} title="Copy. The merge commit gets this trailer.">
            {copied ? 'copied' : `Pk-Lab-Run: ${run.id} ${run.result}`}
          </button>
        </div>
        {#if run.checksTotal}
          <div class="rline">
            <span class="ic" class:ok={run.result === 'pass'} class:bad={run.result === 'fail'}>
              <Icon name={run.result === 'pass' ? 'check' : run.result === 'fail' ? 'x' : 'flask'} size={16} />
            </span>
            <span>Checks</span>
            <span class="mono small right" title={run.summary}>{run.summary ?? `${run.checksPassed} of ${run.checksTotal}`}</span>
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
              <a class="btn dark" href={href(`/changes/${c.id}`)}><Icon name="merge" size={15} />Go to merge</a>
            {/if}
            {#if c.mr}<a class="small" href={c.mr.url} target="_blank" rel="noreferrer">MR !{c.mr.number} · {c.mr.draft ? 'draft' : 'review'}</a>{/if}
          </div>
        {/if}
      </section>

      <section class="card console" aria-label="Lab console">
        <div class="stephead">
          <h3 class="eyebrow">
            Console{#if pinnedStep}&nbsp;· step {pinnedStep.index + 1} · {pinnedStep.name}{/if}
          </h3>
          {#if pinnedStep}
            <button class="btn small" onclick={() => pick(undefined)}><Icon name="play" size={13} />Follow the run</button>
          {:else if running}
            <span class="live small"><span class="pulse"></span>live</span>
          {/if}
        </div>
        <div class="logwrap">
          <div class="log mono" bind:this={logEl} role="log" aria-live="off">
            {#each lines as l (l.key)}
              {#if l.kind === 'head'}
                <div class="lh st-{l.status}" class:sel={pinned && selected === l.step} data-step={l.step}>
                  <span class="mk">{l.status === 'fail' ? '✕' : '✓'}</span>
                  <span class="lt">{l.title}</span>
                  {#if l.ms}<span class="dur">{duration(l.ms)}</span>{/if}
                </div>
              {:else if l.kind === 'out'}
                <div class="lo" class:bad={l.bad} class:sel={pinned && selected === l.step} in:fade={{ duration: 220 }}>{l.text}</div>
              {:else if l.kind === 'checks'}
                <div class="lc" class:bad={l.fail > 0} class:sel={pinned && selected === l.step} in:fade={{ duration: 220 }}>
                  {l.fail ? `${l.fail} of ${l.pass + l.fail} checks fail` : `${l.pass} ${l.pass === 1 ? 'check passes' : 'checks pass'}`}
                </div>
              {:else}
                <div class="lr" data-step={l.step}>
                  <span class="mk">▸</span>
                  <span class="lt">{l.title}</span>
                  <span class="dim">running{l.since ? ` · ${elapsed(now - Date.parse(l.since))}` : '…'}</span>
                  <span class="caret" aria-hidden="true"></span>
                </div>
              {/if}
            {/each}
            {#if running && !lines.some((l) => l.kind === 'running')}
              <div class="lr"><span class="mk">▸</span><span class="dim">starting the lab…</span><span class="caret" aria-hidden="true"></span></div>
            {:else if !running && !streaming && run.result !== 'running'}
              <div class="lend r-{run.result}" in:fade={{ duration: 300 }}>
                {runLabel(run.id)} {run.result}{run.summary ? ` · ${run.summary}` : ''}{run.durationMs ? ` · ${duration(run.durationMs)}` : ''}
              </div>
            {/if}
          </div>
        </div>
      </section>
    </div>
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
  .row.center {
    justify-content: center;
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
  .clock {
    font: 600 13.5px var(--mono);
    font-variant-numeric: tabular-nums;
    padding-left: 6px;
    margin-left: 2px;
    border-left: 1px solid color-mix(in srgb, currentColor 35%, transparent);
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
  .r-cancelled,
  .r-unknown {
    background: var(--panel-2);
    color: var(--muted);
  }
  .fromreport {
    display: inline-flex;
    gap: 5px;
    align-items: center;
    font-size: 13px;
    color: var(--muted);
    border: 1px solid var(--line);
    border-radius: 999px;
    padding: 1px 10px;
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
  /* Scenario and result on the left, the console fills the right column
     (as high as both cards), so the output is on screen during the run. */
  .body {
    display: grid;
    grid-template-columns: minmax(0, 5fr) minmax(0, 6fr);
    grid-template-rows: auto 1fr;
    gap: 16px;
  }
  .scen {
    grid-column: 1;
    grid-row: 1;
  }
  .result-card {
    grid-column: 1;
    grid-row: 2;
  }
  .console {
    grid-column: 2;
    grid-row: 1 / span 2;
    display: flex;
    flex-direction: column;
    min-height: 300px;
  }
  @media (max-width: 1100px) {
    .two,
    .body {
      grid-template-columns: minmax(0, 1fr);
    }
    .scen,
    .result-card,
    .console {
      grid-column: 1;
      grid-row: auto;
    }
    .console {
      min-height: 360px;
    }
    /* Side by side with a terminal: while the run is running the live
       console comes first, so the output is on screen. */
    .body.livefirst .console {
      order: -1;
      max-height: 60vh;
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
  .sline:not(.static):hover,
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
  .hollow {
    width: 14px;
    height: 14px;
    margin: 1px;
    border-radius: 50%;
    border: 2px solid var(--line-strong);
    box-sizing: border-box;
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
  /* Values wrap rather than being cut: a failed check names the node. */
  .right {
    text-align: right;
    justify-self: end;
    overflow-wrap: break-word;
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
    align-items: center;
    gap: 10px;
    margin-bottom: 6px;
  }
  .stephead .eyebrow {
    margin: 0;
  }
  .live {
    display: inline-flex;
    gap: 6px;
    align-items: center;
    color: var(--warn);
    font-weight: 600;
  }
  .logwrap {
    position: relative;
    flex: 1 1 auto;
    min-height: 240px;
  }
  .log {
    position: absolute;
    inset: 0;
    margin: 0;
    background: var(--code-bg);
    border: 1px solid var(--line);
    border-radius: 6px;
    padding: 10px 12px 14px;
    font-size: 13px;
    line-height: 1.6;
    overflow: auto;
    scrollbar-width: thin;
  }
  .log > div {
    white-space: pre-wrap;
    overflow-wrap: anywhere;
    padding: 0 6px;
    margin: 0 -6px;
    border-radius: 3px;
  }
  .lh,
  .lr {
    display: flex;
    gap: 8px;
    align-items: baseline;
    font-weight: 700;
    color: var(--ink);
    margin-top: 6px !important;
  }
  .lh:first-child,
  .lr:first-child {
    margin-top: 0 !important;
  }
  .lh .mk {
    color: var(--ok);
  }
  .lh.st-fail,
  .lh.st-fail .mk {
    color: var(--bad);
  }
  .lh .dur {
    margin-left: auto;
    font-weight: 400;
    color: var(--muted);
    white-space: nowrap;
  }
  .lo {
    color: var(--ink);
    padding-left: 22px !important;
  }
  .lo.bad {
    color: var(--bad);
    font-weight: 600;
  }
  .lc {
    color: var(--ok);
    padding-left: 22px !important;
    font-size: 12px;
  }
  .lc.bad {
    color: var(--bad);
  }
  .log > .sel {
    background: color-mix(in srgb, var(--accent-fill) 14%, transparent);
  }
  .lr {
    color: var(--warn);
  }
  .lr .lt {
    color: var(--ink);
  }
  .dim {
    font-weight: 400;
    color: var(--muted);
    font-variant-numeric: tabular-nums;
  }
  .caret {
    display: inline-block;
    width: 8px;
    height: 15px;
    align-self: center;
    background: var(--warn);
    animation: blink 1s steps(1) infinite;
  }
  @keyframes blink {
    50% {
      opacity: 0;
    }
  }
  .lend {
    margin-top: 10px !important;
    padding: 4px 8px !important;
    font-weight: 700;
    display: inline-block;
  }
  .fullreport summary {
    cursor: pointer;
    margin: 0;
  }
  .fullreport[open] summary {
    margin-bottom: 10px;
  }
  .callout :global(.md p) {
    margin: 0;
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
  @media (prefers-reduced-motion: reduce) {
    .caret,
    .pulse {
      animation: none;
    }
  }
</style>
