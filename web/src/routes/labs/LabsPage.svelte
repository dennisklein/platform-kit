<script lang="ts">
  // Labs: the digital twin (follows main in fluxsim), recent change lab runs
  // and the topology the labs generate from control/inventory,
  // control/cabling.yaml and .pk/emulation.yaml.
  import type { Params } from '../../lib/router.svelte';
  import { href, router } from '../../lib/router.svelte';
  import { api, must, errorText } from '../../lib/api/client';
  import type { components } from '../../lib/api/schema';
  import type { LabList, LabRun } from '../../lib/api/types';
  import { events } from '../../lib/stores/events.svelte';
  import { short, ago } from '../../lib/f/fmt';
  import Glyph from '../../lib/components/Glyph.svelte';

  type Topology = components['schemas']['Topology'];
  type LabDefinition = components['schemas']['LabDefinition'];
  type Cluster = components['schemas']['Cluster'];

  let { params, query }: { params: Params; query: URLSearchParams } = $props();

  let list = $state<LabList | null>(null);
  let clusters = $state<string[]>([]);
  let error = $state<string | null>(null);
  let topo = $state<Topology | null>(null);
  let topoError = $state<string | null>(null);
  let topoLoading = $state(false);

  // The topology shown: ?topology=<cluster or twin>, default the first twin.
  const twins = $derived((list?.definitions ?? []).filter((d) => d.purpose === 'twin'));
  const target = $derived(query.get('topology') ?? twins[0]?.name ?? clusters[0] ?? '');

  async function load() {
    try {
      list = await must(api.GET('/labs', { params: { query: {} } }));
      error = null;
    } catch (e) {
      error = errorText(e);
    }
  }
  async function loadClusters() {
    try {
      const cs = await must(api.GET('/clusters'));
      clusters = cs.map((c: Cluster) => c.id);
    } catch {
      clusters = [];
    }
  }
  async function loadTopology(name: string) {
    if (!name) return;
    topoLoading = true;
    try {
      const t = await must(api.GET('/labs/topology', { params: { query: { ref: 'main', cluster: name } } }));
      if (name === target) {
        topo = t;
        topoError = null;
      }
    } catch (e) {
      if (name === target) topoError = errorText(e);
    } finally {
      topoLoading = false;
    }
  }

  $effect(() => {
    void load();
    void loadClusters();
  });
  $effect(() => {
    void loadTopology(target);
  });

  // Runs and the twin's Flux state change with lab and flux events.
  $effect(() => {
    let t: ReturnType<typeof setTimeout> | undefined;
    const later = () => {
      clearTimeout(t);
      t = setTimeout(() => void load(), 400);
    };
    const offs = ['lab.finished', 'flux.updated'].map((e) => events.on(e as any, later));
    const offStep = events.on('lab.step' as any, (ev: any) => {
      // Patch a running run in place; a new run reloads the list.
      if (!list) return;
      const i = list.runs.findIndex((r) => r.id === ev?.run);
      if (i < 0) return later();
      const r = list.runs[i];
      const steps = [...r.steps];
      if (ev.step && ev.step.index < steps.length) steps[ev.step.index] = ev.step;
      list.runs[i] = { ...r, steps };
    });
    return () => {
      clearTimeout(t);
      offs.forEach((o) => o());
      offStep();
    };
  });

  const runs = $derived(list?.runs ?? []);

  function runHref(r: LabRun) {
    return href(`/labs/runs/${encodeURIComponent(r.id)}`);
  }
  function labTime(ms: number | undefined) {
    if (!ms) return '';
    const s = Math.round(ms / 1000);
    const m = Math.floor(s / 60);
    return m ? `${m}m ${String(s % 60).padStart(2, '0')}s` : `${s}s`;
  }
  function progress(r: LabRun) {
    const done = r.steps.filter((s) => s.status !== 'pending' && s.status !== 'running').length;
    const cur = r.steps.find((s) => s.status === 'running');
    return `${cur ? cur.title : 'starting'} · step ${Math.min(done + 1, r.steps.length)} of ${r.steps.length}`;
  }
  function tone(r: LabRun) {
    return r.result === 'pass' ? 'ok' : r.result === 'fail' ? 'bad' : r.result === 'running' ? 'warn' : '';
  }
  function twinHealth(s: Cluster | undefined): { tone: string; label: string } {
    if (!s) return { tone: '', label: 'no fluxsim status' };
    if (s.failing || !s.healthy) return { tone: 'bad', label: s.applied?.sha ? 'failing' : 'not applied' };
    if (s.applied?.sha && s.verified?.sha !== s.applied.sha) return { tone: 'warn', label: 'verifying' };
    return { tone: 'ok', label: 'healthy' };
  }
  function scale(d: LabDefinition): string[] {
    const s = (d.spec?.scale ?? {}) as { machines?: string[] };
    return s.machines ?? [];
  }
  function specStr(d: LabDefinition, key: string): string {
    const v = d.spec?.[key];
    if (Array.isArray(v)) return v.join(', ');
    return v == null ? '' : String(v);
  }

  // Topology tables.
  const networks = $derived(topo?.networks ?? []);
  const machines = $derived(topo?.machines ?? []);
  const machineNames = $derived(new Set(machines.map((m) => m.name)));
  const uplinks = $derived((topo?.links ?? []).filter((l) => !machineNames.has(l.a.split(':')[0]) && !machineNames.has(l.b.split(':')[0])));
  const backends = $derived(
    Object.entries(
      machines.reduce<Record<string, number>>((acc, m) => {
        const b = m.machine?.backend ?? 'none';
        acc[b] = (acc[b] ?? 0) + 1;
        return acc;
      }, {}),
    ),
  );
  const options = $derived([...new Set([...twins.map((t) => t.name), ...clusters])]);
</script>

<div class="page">
  <header class="head">
    <div>
      <h1>Labs</h1>
      <p class="muted small sub">
        Labs run on emulated hardware. The topology comes from <span class="mono">control/inventory</span>,
        <span class="mono">control/cabling.yaml</span> and <span class="mono">.pk/emulation.yaml</span>; nobody writes it by hand.
        A change lab writes its report into the change. Driver: <strong>sim</strong> on this host.
      </p>
    </div>
    <span class="spacer"></span>
    <button class="btn small" onclick={() => { void load(); void loadTopology(target); }} title="Reload">
      <Glyph name="refresh" size={13} /> Reload
    </button>
  </header>

  {#if error}
    <div class="error-box">{error}</div>
  {/if}

  <section aria-label="Digital twins">
    <h2 class="eyebrow">Digital twin</h2>
    {#if list && !twins.length}
      <div class="empty">No Lab with purpose twin in <span class="mono">.pk/labs</span>.</div>
    {/if}
    <div class="twins">
      {#each twins as d (d.name)}
        {@const h = twinHealth(d.status)}
        <article class="card twin t-{h.tone || 'muted'}">
          <header class="th">
            <Glyph name="flask" size={16} />
            <h3 class="mono">{d.name}</h3>
            <span class="pill {h.tone}"><span class="dot"></span>{h.label}</span>
            <span class="spacer"></span>
            <span class="badge">follows {d.follow ?? 'main'}</span>
            {#if d.topology}<span class="badge">twin of {d.topology}</span>{/if}
          </header>
          <div class="rows">
            <div class="rev">
              <span class="rl">applied</span>
              {#if d.status?.applied?.sha}
                <a class="mono" href={href(`/history/${d.status.applied.sha}`)}>{short(d.status.applied.sha)}</a>
                {#if d.status.applied.change}<a class="mono chg" href={href(`/changes/${d.status.applied.change}`)}>{d.status.applied.change}</a>{/if}
                <span class="muted small">{ago(d.status.applied.at)}</span>
              {:else}
                <span class="muted small">{d.status ? 'nothing yet' : 'fluxsim unreachable'}</span>
              {/if}
            </div>
            <div class="rev">
              <span class="rl">verified</span>
              {#if d.status?.verified?.sha}
                <a class="mono" href={href(`/history/${d.status.verified.sha}`)}>{short(d.status.verified.sha)}</a>
                <span class="muted small">{ago(d.status.verified.at)}</span>
              {:else}
                <span class="muted small">not yet</span>
              {/if}
            </div>
          </div>
          {#if d.status?.message}<p class="msg small" class:bad={h.tone === 'bad'}>{d.status.message}</p>{/if}
          <dl class="facts small">
            <div><dt>fidelity</dt><dd>{specStr(d, 'fidelity') || '—'}</dd></div>
            <div><dt>provision</dt><dd>{specStr(d, 'provision') || '—'}</dd></div>
            <div><dt>host</dt><dd>{specStr(d, 'host') || '—'}</dd></div>
            <div><dt>snapshot</dt><dd>{specStr(d, 'snapshot') || '—'}</dd></div>
            <div><dt>lifetime</dt><dd>{specStr(d, 'lifetime') || '—'}</dd></div>
            {#if specStr(d, 'expose')}<div><dt>expose</dt><dd>{specStr(d, 'expose')}</dd></div>{/if}
            {#if scale(d).length}
              <div class="wide">
                <dt>machines</dt>
                <dd>
                  {#each scale(d) as m (m)}<a class="mchip mono" href={href(`/assets/Node/${m}`)}>{m}</a>{/each}
                  {#if (d.spec?.scale as any)?.backend}<span class="muted"> on {(d.spec?.scale as any).backend}</span>{/if}
                </dd>
              </div>
            {/if}
          </dl>
          <footer class="cf small muted">
            <a class="mono" href={href('/files', { path: d.path })}>{d.path}</a>
            <span class="spacer"></span>
            {#if d.status?.objects != null}<span>{d.status.objects} objects</span>{/if}
            <a href={href('/labs', { topology: d.name })}>topology</a>
          </footer>
        </article>
      {/each}
    </div>
  </section>

  <section aria-label="Recent runs">
    <h2 class="eyebrow">Change lab runs</h2>
    {#if list === null && !error}
      <p class="muted small">Loading…</p>
    {:else if !runs.length}
      <div class="empty">No lab runs yet. Run the lab of a change from its page.</div>
    {:else}
      <table class="table runs">
        <thead>
          <tr><th>Run</th><th>Result</th><th>Summary</th><th>Scenario</th><th>Lab time</th><th>By</th><th>Started</th></tr>
        </thead>
        <tbody>
          {#each runs as r (r.id)}
            <tr class="rrow" onclick={() => router.navigate(`/labs/runs/${encodeURIComponent(r.id)}`)}>
              <td class="mono">
                <a href={runHref(r)} onclick={(e) => e.stopPropagation()}>{r.id}</a>
              </td>
              <td><span class="pill {tone(r)}"><span class="dot"></span>{r.result}</span></td>
              <td class="sum">
                {#if r.result === 'running'}
                  <span class="muted">{progress(r)}</span>
                {:else}
                  {r.summary ?? ''}
                {/if}
              </td>
              <td><span class="badge">{r.scenario ?? ''}</span>{#if r.cluster}{' '}<span class="muted small">{r.cluster}</span>{/if}</td>
              <td class="mono small">{labTime(r.simulatedMs)}</td>
              <td>{r.by ?? ''}</td>
              <td class="muted small">{ago(r.startedAt)}</td>
            </tr>
          {/each}
        </tbody>
      </table>
    {/if}
  </section>

  <section aria-label="Topology">
    <div class="toph">
      <h2 class="eyebrow">Topology · main</h2>
      <span class="spacer"></span>
      <label class="small muted" for="topo-sel">cluster or twin</label>
      <select id="topo-sel" value={target} onchange={(e) => router.navigate(`/labs?topology=${encodeURIComponent((e.currentTarget as HTMLSelectElement).value)}`)}>
        {#each options as o (o)}<option value={o}>{o}</option>{/each}
      </select>
    </div>
    {#if topoError}
      <div class="error-box">{topoError}</div>
    {:else if !topo}
      <p class="muted small">{topoLoading ? 'Generating…' : ''}</p>
    {:else}
      <div class="card topo">
        <div class="tsum">
          <span><strong class="mono">{topo.name}</strong>{#if topo.cluster && topo.cluster !== topo.name}{' '}<span class="muted">(cluster {topo.cluster})</span>{/if}</span>
          <span class="muted">{machines.length} machines</span>
          {#each backends as [b, n] (b)}<span class="badge">{n} × {b}</span>{/each}
          <span class="muted">substrate <span class="mono">{topo.substrate}</span>, uplink <span class="mono">{topo.uplink}</span></span>
          {#if topo.sha}<span class="muted">at <a class="mono" href={href(`/history/${topo.sha}`)}>{short(topo.sha)}</a></span>{/if}
        </div>
        <div class="nets">
          {#each networks as n (n.name)}
            <span class="net" class:off={!n.emulate} title={n.emulate ? 'emulated' : 'not emulated'}>
              <span class="mono">{n.name}</span>
              {#if n.cidr}<span class="muted mono small">{n.cidr}</span>{/if}
              {#if n.vlan}<span class="muted small">VLAN {n.vlan}</span>{/if}
              {#if !n.emulate}<span class="muted small">not emulated</span>{/if}
            </span>
          {/each}
        </div>
        {#if topo.problems?.length}
          <ul class="problems small">
            {#each topo.problems as p, i (i)}
              <li class:bad={p.severity === 'error'}>
                <Glyph name="alert" size={12} />
                <a class="mono" href={href('/files', { path: p.path, line: p.line ? String(p.line) : undefined })}>{p.path}</a>
                {p.message}
              </li>
            {/each}
          </ul>
        {/if}
        <table class="table machines">
          <thead>
            <tr><th>Machine</th><th>Role</th><th>Location</th><th>BMC</th><th>Backend</th><th>NICs</th></tr>
          </thead>
          <tbody>
            {#each machines as m (m.name)}
              <tr>
                <td class="mono"><a href={href(`/assets/Node/${m.name}`)}>{m.name}</a></td>
                <td>{m.role ?? ''}</td>
                <td class="mono small">{m.rack ?? ''}{m.unit ? ` U${m.unit}` : ''}</td>
                <td class="small">
                  {#if m.bmc?.fixture}<span>Redfish {m.bmc.fixture}</span>{:else}<span class="muted">no fixture</span>{/if}
                  {#if m.bmc?.port}{' '}<span class="muted mono">· {m.bmc.port}</span>{/if}
                </td>
                <td class="small">
                  {m.machine?.backend ?? 'none'}{#if m.machine?.cpus}{' '}<span class="muted">· {m.machine.cpus} CPU, {Math.round((m.machine.memoryMiB ?? 0) / 1024)} GiB</span>{/if}
                </td>
                <td class="small mono">
                  {#each m.nics ?? [] as n, i (n.name)}{i ? ', ' : ''}{n.name} → {n.peer ?? n.network}{/each}
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
        {#if uplinks.length}
          <div class="ups small">
            <span class="muted">switch uplinks</span>
            {#each uplinks as l (l.a + l.b)}<span class="mono up">{l.a} ↔ {l.b}{#if l.cable}{' '}<span class="muted">{l.cable}</span>{/if}</span>{/each}
          </div>
        {/if}
      </div>
    {/if}
  </section>
</div>

<style>
  .page {
    display: grid;
    gap: 20px;
    align-content: start;
  }
  .head {
    display: flex;
    align-items: flex-start;
    gap: 12px;
  }
  .head .btn {
    display: inline-flex;
    gap: 5px;
    align-items: center;
  }
  h1 {
    margin: 0;
    font-size: 22px;
  }
  .sub {
    margin: 4px 0 0;
    max-width: 720px;
  }
  h2.eyebrow {
    margin: 0 0 8px;
  }
  .card {
    border: 1px solid var(--line);
    border-radius: var(--radius);
    background: var(--panel);
    padding: 12px 14px;
  }
  .twins {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(420px, 1fr));
    gap: 12px;
  }
  .twin {
    display: grid;
    gap: 10px;
    border-left: 4px solid var(--ok);
    min-width: 0;
  }
  .twin.t-bad {
    border-left-color: var(--bad);
  }
  .twin.t-warn {
    border-left-color: var(--warn);
  }
  .twin.t-muted {
    border-left-color: var(--line);
  }
  .th {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .th h3 {
    margin: 0;
    font-size: 16px;
    text-transform: none;
    letter-spacing: 0;
    color: var(--ink);
  }
  .rows {
    display: grid;
    gap: 3px;
  }
  .rev {
    display: flex;
    align-items: baseline;
    gap: 8px;
    font-size: 13px;
  }
  .rl {
    width: 64px;
    flex: none;
    color: var(--muted);
    font-size: 12px;
  }
  .chg {
    color: var(--accent);
  }
  .msg {
    margin: 0;
    color: var(--muted);
  }
  .msg.bad {
    color: var(--bad);
  }
  .facts {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(120px, 1fr));
    gap: 6px 12px;
    margin: 0;
  }
  .facts div {
    min-width: 0;
  }
  .facts .wide {
    grid-column: 1 / -1;
  }
  .facts dt {
    color: var(--muted);
    font-size: 11px;
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }
  .facts dd {
    margin: 0;
  }
  .mchip {
    display: inline-block;
    font-size: 12px;
    padding: 1px 7px;
    margin: 0 4px 2px 0;
    border-radius: 4px;
    background: var(--accent-soft);
    color: var(--accent);
  }
  .cf {
    display: flex;
    gap: 10px;
    align-items: center;
  }
  .spacer {
    flex: 1;
  }
  .runs .rrow {
    cursor: pointer;
  }
  .runs .rrow:hover td {
    background: var(--hover, color-mix(in srgb, var(--accent) 5%, transparent));
  }
  .runs .sum {
    max-width: 520px;
  }
  .toph {
    display: flex;
    align-items: center;
    gap: 8px;
    margin-bottom: 8px;
  }
  .toph h2.eyebrow {
    margin: 0;
  }
  .topo {
    display: grid;
    gap: 10px;
  }
  .tsum {
    display: flex;
    gap: 12px;
    align-items: baseline;
    flex-wrap: wrap;
    font-size: 13px;
  }
  .nets {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
  }
  .net {
    display: inline-flex;
    gap: 6px;
    align-items: baseline;
    border: 1px solid var(--line);
    border-radius: 999px;
    padding: 2px 10px;
    font-size: 12px;
  }
  .net.off {
    border-style: dashed;
    opacity: 0.7;
  }
  .problems {
    margin: 0;
    padding: 0;
    list-style: none;
    display: grid;
    gap: 3px;
    color: var(--warn);
  }
  .problems li {
    display: flex;
    gap: 6px;
    align-items: center;
  }
  .problems li.bad {
    color: var(--bad);
  }
  .ups {
    display: flex;
    gap: 10px;
    flex-wrap: wrap;
    align-items: baseline;
  }
  .up {
    font-size: 12px;
  }
</style>
