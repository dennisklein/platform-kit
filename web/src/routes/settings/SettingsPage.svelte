<script lang="ts">
  // Settings: who you are on this bench, theme, your agent, the bench
  // services, who may do what (policy), and the demo controls.
  import type { Params } from '../../lib/router.svelte';
  import { href } from '../../lib/router.svelte';
  import { api, must } from '../../lib/api/client';
  import type { AgentGrant, PolicySummary } from '../../lib/api/ftypes';
  import { session } from '../../lib/stores/me.svelte';
  import { ui, type Theme } from '../../lib/stores/ui.svelte';
  import { endsAt, short, ago } from '../../lib/f/fmt';
  import { plural } from '../../lib/change/format';
  import DemoControls from '../../lib/components/DemoControls.svelte';
  import Glyph from '../../lib/components/Glyph.svelte';

  let { params, query }: { params: Params; query: URLSearchParams } = $props();

  let grants = $state<AgentGrant[]>([]);
  let policy = $state<PolicySummary | null>(null);

  $effect(() => {
    void must(api.GET('/agents'))
      .then((g) => (grants = g))
      .catch(() => (grants = []));
    void must(api.GET('/policy'))
      .then((p) => (policy = p))
      .catch(() => (policy = null));
  });

  const me = $derived(session.me);
  const st = $derived(session.status);
  const mine = $derived(grants.filter((g) => g.actsFor === me?.id));
  const THEMES: [Theme, string][] = [
    ['system', 'System'],
    ['light', 'Light'],
    ['dark', 'Dark'],
  ];
  const svc = $derived(
    st
      ? [
          { name: st.forge.name, ok: st.forge.reachable, detail: st.forge.detail ?? st.forge.url ?? '' },
          {
            name: 'LLM',
            ok: st.llm.reachable,
            detail: st.llm.mock ? 'built-in analysis (no PK_LLM_BASE_URL)' : `${st.llm.model}${st.llm.baseUrl ? ` at ${st.llm.baseUrl}` : ''}${st.llm.embedModel ? ` · embeddings ${st.llm.embedModel}` : ''}`,
          },
          { name: st.flux.name, ok: st.flux.reachable, detail: st.flux.detail ?? st.flux.url ?? '' },
        ]
      : [],
  );
  const myRules = $derived(
    (policy?.rules ?? []).map((r) => ({
      ...r,
      write: !!me && (r.writers.includes(me.id) || (me.roles ?? []).some((x) => r.writers.includes(x))),
      approve: !!me && (r.approvers.includes(me.id) || (me.roles ?? []).some((x) => r.approvers.includes(x))),
    })),
  );
</script>

<div class="page">
  <header>
    <h1>Settings</h1>
  </header>

  <div class="cols">
    <div class="col">
      <section class="card id" aria-label="You">
        <h2 class="eyebrow">You</h2>
        {#if me}
          <div class="me">
            <span class="avatar">{me.initials ?? me.id.slice(0, 2).toUpperCase()}</span>
            <div>
              <div class="name">{me.name}</div>
              <div class="small muted"><span class="mono">{me.id}</span>{me.email ? ` · ${me.email}` : ''} · {me.kind}{me.actsFor ? ` for ${me.actsFor}` : ''}</div>
            </div>
          </div>
          <div class="tags">
            {#each me.roles as r (r)}<span class="badge role">{r}</span>{/each}
            {#each me.teams ?? [] as t (t)}<span class="badge">team {t}</span>{/each}
          </div>
          <p class="small muted">
            Roles come from <a class="mono" href={href('/files', { path: policy?.path ?? '.pk/policy.yaml' })}>{policy?.path ?? '.pk/policy.yaml'}</a>.
            {st?.mode === 'local' ? 'Local mode: the bench runs as you.' : 'Shared mode: you signed in through the forge.'}
          </p>
        {:else}
          <p class="muted small">Loading…</p>
        {/if}
      </section>

      <section class="card" aria-label="Theme">
        <h2 class="eyebrow">Theme</h2>
        <div class="seg" role="radiogroup" aria-label="Theme">
          {#each THEMES as [t, label] (t)}
            <button role="radio" aria-checked={ui.theme === t} onclick={() => ui.setTheme(t)}>{label}</button>
          {/each}
        </div>
        <p class="small muted">Saved in this browser.</p>
      </section>

      <section class="card" aria-label="Agent">
        <h2 class="eyebrow">Your agent</h2>
        {#each mine as g (g.id)}
          <div class="agent">
            <Glyph name="bot" size={16} />
            <span class="mono">{g.id}</span>
            {#if g.active && g.expiresAt && new Date(g.expiresAt) > new Date()}
              <span class="pill ok"><span class="dot"></span>approved · {endsAt(g.expiresAt)}</span>
              <span class="small muted">{g.mode} mode{g.lastSeenAt ? ` · last call ${ago(g.lastSeenAt)}` : ''}</span>
            {:else}
              <span class="pill"><span class="dot"></span>not approved</span>
            {/if}
          </div>
        {:else}
          <p class="small muted">No agent acts for you in the policy.</p>
        {/each}
        <a class="btn small go" href={href('/settings/agents')}>
          <Glyph name="key" size={13} /> {mine.some((g) => g.active) ? 'Manage agents' : 'Approve agent for today'}
          <Glyph name="arrow-right" size={12} />
        </a>
      </section>

      <DemoControls />
    </div>

    <div class="col">
      <section class="card" aria-label="Bench">
        <h2 class="eyebrow">Bench</h2>
        {#if st}
          <dl class="kv">
            <div><dt>mode</dt><dd>{st.mode}{st.demo ? ' · demo' : ''}</dd></div>
            <div><dt>version</dt><dd class="mono">{st.version}</dd></div>
            <div>
              <dt>main</dt>
              <dd>
                <a class="mono" href={href(`/history/${st.main.sha}`)}>{short(st.main.sha)}</a>
                {#if st.main.subject}<span class="muted"> {st.main.subject}</span>{/if}
              </dd>
            </div>
          </dl>
          <ul class="svc">
            {#each svc as s (s.name)}
              <li>
                <span class="dot" class:ok={s.ok} aria-label={s.ok ? 'reachable' : 'unreachable'}></span>
                <strong>{s.name}</strong>
                <span class="small muted">{s.ok ? '' : 'unreachable · '}{s.detail}</span>
              </li>
            {/each}
          </ul>
          <p class="small muted">
            Every model call is in the <a href={href('/llm-log')}>LLM log</a>. Secret fields are removed before each call.
          </p>
        {/if}
      </section>

      {#if policy}
        <section class="card" aria-label="Policy">
          <h2 class="eyebrow">Who may do what</h2>
          <div class="scroll">
          <table class="table rules">
            <thead><tr><th>Rule</th><th>Paths</th><th>Approvals</th><th>You</th></tr></thead>
            <tbody>
              {#each myRules as r (r.name)}
                <tr>
                  <td><strong>{r.name}</strong>{#if r.risk}<span class="badge risk-{r.risk}">{r.risk}</span>{/if}</td>
                  <td class="mono small">{r.paths.join(', ')}</td>
                  <td>{r.approvals} × {r.approvers.join(', ')}{r.labGate ? ' + lab' : ''}</td>
                  <td class="you">
                    <span class:ok-text={r.write} class:muted={!r.write}>{r.write ? 'write' : 'no write'}</span>,
                    <span class:ok-text={r.approve} class:muted={!r.approve}>{r.approve ? 'approve' : 'no approve'}</span>
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
          </div>
          <p class="small muted">
            {plural(policy.people.length, 'person', 'people')}, {plural(policy.agents.length, 'agent')}, {plural(policy.bots.length, 'bot')}.
            {#if policy.fastPath?.approvals}Revert fast path: {policy.fastPath.approvals} approval{policy.fastPath.approvals > 1 ? 's' : ''}{policy.fastPath.labGate === false ? ', no lab' : ''}.{/if}
            Agents never approve or merge.
          </p>
          {#if policy.problems.length}
            <div class="callout warn"><Glyph name="alert" size={14} /> {policy.problems.length} problem{policy.problems.length > 1 ? 's' : ''} in the policy: {policy.problems[0].message}</div>
          {/if}
        </section>
      {/if}
    </div>
  </div>
</div>

<style>
  /* One column that may shrink below its content (phone width). */
  .page {
    grid-template-columns: minmax(0, 1fr);
  }
  h1 {
    margin: 0;
  }
  .cols {
    display: grid;
    grid-template-columns: minmax(0, 1fr) minmax(0, 1.2fr);
    gap: 16px;
    align-items: start;
  }
  .col {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 16px;
    min-width: 0;
  }
  .card {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 10px;
  }
  .card p {
    margin: 0;
  }
  h2.eyebrow {
    margin: 0;
  }
  .me {
    display: flex;
    gap: 12px;
    align-items: center;
  }
  .avatar {
    width: 42px;
    height: 42px;
    border-radius: 50%;
    background: var(--gsi-orange);
    color: #131519;
    display: grid;
    place-items: center;
    font-weight: 700;
    flex: none;
  }
  .name {
    font-weight: 600;
    font-size: 16px;
  }
  .tags {
    display: flex;
    gap: 6px;
    flex-wrap: wrap;
  }
  .role {
    background: var(--accent-soft);
    color: var(--accent);
  }
  .seg {
    display: inline-flex;
    border: 1px solid var(--line-strong);
    border-radius: 7px;
    overflow: hidden;
    justify-self: start;
  }
  .seg button {
    border: 0;
    background: var(--panel);
    color: var(--ink);
    padding: 5px 14px;
    font-size: 13px;
  }
  .seg button + button {
    border-left: 1px solid var(--line);
  }
  .seg button[aria-checked='true'] {
    background: var(--gsi-navy);
    color: #fff;
  }
  .agent {
    display: flex;
    gap: 8px;
    align-items: center;
    flex-wrap: wrap;
  }
  .go {
    justify-self: start;
    display: inline-flex;
    align-items: center;
    gap: 6px;
    text-decoration: none;
  }
  .kv {
    margin: 0;
    display: grid;
    gap: 4px;
    font-size: 13px;
  }
  .kv div {
    display: flex;
    gap: 10px;
  }
  .kv dt {
    width: 64px;
    color: var(--muted);
    flex: none;
  }
  .kv dd {
    margin: 0;
    min-width: 0;
  }
  .svc {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    gap: 6px;
    font-size: 13px;
  }
  .svc li {
    display: flex;
    gap: 8px;
    align-items: baseline;
  }
  .svc .dot {
    width: 8px;
    height: 8px;
    border-radius: 50%;
    background: var(--bad);
    flex: none;
    transform: translateY(-1px);
  }
  .svc .dot.ok {
    background: var(--ok);
  }
  .rules {
    font-size: 13px;
  }
  .rules td {
    vertical-align: top;
  }
  .rules .badge {
    margin-left: 6px;
  }
  .risk-high {
    background: var(--bad-soft);
    color: var(--bad);
  }
  .risk-low {
    background: var(--ok-soft);
    color: var(--ok);
  }
  .you {
    white-space: nowrap;
    font-size: 12px;
  }
  .scroll {
    overflow-x: auto;
    min-width: 0;
  }
  .card {
    min-width: 0;
  }
  @media (max-width: 900px) {
    .cols {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
