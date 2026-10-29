<script lang="ts">
  // Agents: approve your agent for today (token shown once, MCP config
  // snippets with copy buttons), see when it ends, revoke it.
  import type { Params } from '../../lib/router.svelte';
  import { href } from '../../lib/router.svelte';
  import { api, must, errorText } from '../../lib/api/client';
  import type { AgentGrant, AgentApproval, AgentMode, AuditEntry } from '../../lib/api/ftypes';
  import { session } from '../../lib/stores/me.svelte';
  import { events } from '../../lib/stores/events.svelte';
  import { mcpCalls } from '../../lib/stores/mcpcalls.svelte';
  import { reasonText } from '../../lib/f/mcp';
  import { toasts } from '../../lib/stores/toasts.svelte';
  import { endsAt, ago, clock, dateTime } from '../../lib/f/fmt';
  import { approveBody, newTokenBody, HOURS } from '../../lib/f/agents';
  import CopyButton from '../../lib/components/CopyButton.svelte';
  import Modal from '../../lib/components/Modal.svelte';
  import Glyph from '../../lib/components/Glyph.svelte';
  import PageHeader from '../../lib/components/PageHeader.svelte';

  let { params, query }: { params: Params; query: URLSearchParams } = $props();

  let grants = $state<AgentGrant[] | null>(null);
  let audit = $state<AuditEntry[]>([]);
  let error = $state<string | null>(null);
  let busy = $state<string | null>(null);
  /** The approval just made: the token is only here, once. */
  let approval = $state<AgentApproval | null>(null);
  let snippetTab = $state(0);
  let hours = $state(8);
  let mode = $state<AgentMode>('navigate');
  let revokeOf = $state<AgentGrant | null>(null);
  let revokeOpen = $state(false);
  let now = $state(new Date());

  const me = $derived(session.me);

  async function load() {
    try {
      grants = await must(api.GET('/agents'));
      error = null;
    } catch (e) {
      error = errorText(e);
    }
    try {
      const a = await must(api.GET('/audit', { params: { query: { limit: 100 } } }));
      audit = a.filter((x) => x.action.startsWith('agent.') || x.actorKind === 'agent').slice(0, 12);
    } catch {
      audit = [];
    }
  }
  $effect(() => {
    void load();
  });

  // Presence events carry the agent's last tool; the clock moves "ends" and "ago".
  $effect(() => {
    const off = events.on('presence' as any, () => void load());
    const t = setInterval(() => (now = new Date()), 30_000);
    return () => {
      off();
      clearInterval(t);
    };
  });

  const mine = $derived((grants ?? []).filter((g) => g.actsFor === me?.id));
  const others = $derived((grants ?? []).filter((g) => g.actsFor !== me?.id));
  const isHuman = $derived(me?.kind === 'human');
  const preview = $derived(new Date(now.getTime() + hours * 3600_000));
  const live = (g: AgentGrant) => g.active && (!g.expiresAt || new Date(g.expiresAt).getTime() > now.getTime());

  async function approve(g: AgentGrant, body = approveBody(hours, mode)) {
    busy = `approve-${g.id}`;
    try {
      const a = await must(api.POST('/agents/{id}/approve', { params: { path: { id: g.id } }, body }));
      approval = a;
      snippetTab = 0;
      grants = (grants ?? []).map((x) => (x.id === g.id ? a.grant : x));
      toasts.ok(`${g.id} approved for ${body.hours} h.`, `Token ${endsAt(a.grant.expiresAt)}. Copy it now; it is shown once.`);
      void load();
    } catch (e) {
      toasts.error(`Could not approve ${g.id}`, e);
    } finally {
      busy = null;
    }
  }

  async function revoke() {
    const g = revokeOf;
    if (!g) return;
    busy = `revoke-${g.id}`;
    try {
      const next = await must(api.DELETE('/agents/{id}/grant', { params: { path: { id: g.id } } }));
      grants = (grants ?? []).map((x) => (x.id === g.id ? next : x));
      if (approval?.grant.id === g.id) approval = null;
      revokeOpen = false;
      toasts.ok(`${g.id} revoked.`, 'Its token stops working now.');
      void load();
    } catch (e) {
      toasts.error(`Could not revoke ${g.id}`, e);
    } finally {
      busy = null;
    }
  }

  const CAN: Record<string, string> = {
    push: 'push commits',
    'open-mr': 'open MRs',
    'run-lab': 'run labs',
    'mark-ready': 'mark ready',
  };
  const MODES: [AgentMode, string, string][] = [
    ['navigate', 'Navigate', 'Your screen follows the agent: the change, file or lab it works on opens for you.'],
    ['suggest', 'Suggest', 'Your screen stays put; the agent sends "look here" cards you can open.'],
  ];
  const clientName = (c: string) => (c === 'claude-code' ? 'Claude Code' : c === 'opencode' ? 'opencode' : c);
</script>

{#snippet grantCard(g: AgentGrant, own: boolean)}
  {@const on = live(g)}
  <article class="grant" class:on>
    <header class="gh">
      <span class="av"><Glyph name="bot" size={18} /></span>
      <div class="who">
        <h3 class="mono">{g.id}</h3>
        <span class="small muted">acts for <strong>{g.actsFor}</strong> · own identity in commits (Pk-Agent)</span>
      </div>
      <span class="spacer"></span>
      {#if on}
        <span class="pill ok"><span class="dot"></span>approved · {endsAt(g.expiresAt, now)}</span>
      {:else}
        <span class="pill"><span class="dot"></span>{g.active ? 'expired' : 'not approved'}</span>
      {/if}
    </header>

    <div class="can">
      {#each g.can as c (c)}<span class="cap"><Glyph name="check" size={11} /> {CAN[c] ?? c}</span>{/each}
      <span class="cap no"><Glyph name="x" size={11} /> approve or merge</span>
    </div>

    {#if on}
      <div class="tokbar">
        <Glyph name="key" size={15} />
        <span><b>{g.actsFor}/agent</b> via MCP · token approved {g.approvedAt && new Date(g.approvedAt).toDateString() === now.toDateString() ? 'today' : dateTime(g.approvedAt)}{g.approvedBy && g.approvedBy !== g.actsFor ? ` by ${g.approvedBy}` : ''}, <b>{endsAt(g.expiresAt, now)}</b></span>
        <span class="spacer"></span>
        <span class="mode">{g.mode} mode</span>
      </div>
      <p class="small muted seen">
        {#if g.lastSeenAt}
          Last call {ago(g.lastSeenAt, now)}{#if g.lastTool}{' · '}<span class="mono">{g.lastTool}</span>{/if}
        {:else}
          No MCP call yet. Waiting for the agent.
        {/if}
      </p>
      {#if own}
        <div class="acts">
          <button class="btn small danger" disabled={!!busy} onclick={() => ((revokeOf = g), (revokeOpen = true))}>
            <Glyph name="x" size={12} /> Revoke now
          </button>
          <span class="spacer"></span>
          <label class="small" for="nhrs-{g.id}">Valid for</label>
          <select id="nhrs-{g.id}" class="select" bind:value={hours}>
            {#each HOURS as h (h)}<option value={h}>{h} h</option>{/each}
          </select>
          <button class="btn small" disabled={!!busy || !isHuman} onclick={() => approve(g, newTokenBody(g, hours))} title="Issue a new token for {hours} h from now, in {g.mode} mode; the old token stops working">
            {#if busy === `approve-${g.id}`}<span class="spin"></span>{:else}<Glyph name="refresh" size={12} />{/if} New token
          </button>
        </div>
      {/if}
    {:else if own}
      <div class="approve">
        <fieldset class="modes">
          <legend class="eyebrow">Mode</legend>
          {#each MODES as [m, label, text] (m)}
            <label class="mode-opt" class:sel={mode === m}>
              <input type="radio" name="mode-{g.id}" value={m} bind:group={mode} />
              <span><strong>{label}</strong><span class="small muted">{text}</span></span>
            </label>
          {/each}
        </fieldset>
        <div class="row hrs">
          <label class="small" for="hrs-{g.id}">Valid for</label>
          <select id="hrs-{g.id}" class="select" bind:value={hours}>
            {#each HOURS as h (h)}<option value={h}>{h} h</option>{/each}
          </select>
          <span class="small muted">ends {clock(preview)}{preview.toDateString() !== now.toDateString() ? ' tomorrow' : ''}</span>
          <span class="spacer"></span>
          <button class="btn primary" disabled={!!busy || !isHuman} onclick={() => approve(g)}>
            {#if busy === `approve-${g.id}`}<span class="spin"></span>{:else}<Glyph name="shield" size={14} />{/if}
            Approve agent for today
          </button>
        </div>
        {#if !isHuman}<p class="small warn-text">Only a person can approve an agent.</p>{/if}
      </div>
    {:else}
      <p class="small muted">Only {g.actsFor} can approve this agent.</p>
    {/if}
  </article>
{/snippet}

<div class="page">
  <PageHeader title="Agents">
    {#snippet eyebrow()}<a href={href('/settings')}>Settings</a>{/snippet}
    {#snippet meta()}
      Your agent harness (opencode, Claude Code) works through MCP on this bench with its own identity and a token for
      today. It can push, open MRs, run labs and mark ready. A person approves every merge.
    {/snippet}
  </PageHeader>

  {#if error}<div class="error-box">{error}</div>{/if}

  {#if approval}
    <section class="once" aria-label="New token">
      <header class="oh">
        <Glyph name="key" size={16} />
        <strong>Token for {approval.grant.id}</strong>
        <span class="small dim">shown once · {endsAt(approval.grant.expiresAt, now)}</span>
        <span class="spacer"></span>
        <button class="btn small x" onclick={() => (approval = null)} aria-label="Hide token">Done</button>
      </header>
      <div class="tok">
        <code class="mono">{approval.token}</code>
        <CopyButton text={approval.token} label="Copy token" dark />
      </div>
      <p class="small dim">
        MCP endpoint <code class="mono">{approval.mcpUrl}</code>. Add one of these to your agent harness; the token is
        in it. This page does not keep it.
      </p>
      {#if approval.snippets.length}
        <div class="tabs" role="tablist">
          {#each approval.snippets as s, i (s.client)}
            <button role="tab" aria-selected={snippetTab === i} onclick={() => (snippetTab = i)}>{clientName(s.client)}</button>
          {/each}
        </div>
        {@const s = approval.snippets[snippetTab] ?? approval.snippets[0]}
        {#if s.command}
          <div class="snip">
            <div class="sh"><span class="small dim">Run in your project</span><span class="spacer"></span><CopyButton text={s.command} label="Copy command" dark /></div>
            <pre class="mono cmd"><span class="pr">$</span> {s.command}</pre>
          </div>
        {/if}
        {#if s.allow}
          <div class="snip">
            <div class="sh"><span class="small dim">Allow the platform-kit tools in this directory (else the agent asks at each new tool)</span><span class="spacer"></span><CopyButton text={s.allow} label="Copy allow rule" dark /></div>
            <pre class="mono cmd"><span class="pr">$</span> {s.allow}</pre>
            {#if s.allowFlag}<p class="small dim">Other way: start the agent with <code class="mono">{s.allowFlag}</code>.</p>{/if}
          </div>
        {/if}
        <div class="snip">
          <div class="sh">
            <span class="small dim">{s.command ? 'or put this in' : 'Put this in'} <code class="mono">{s.file}</code></span>
            <span class="spacer"></span>
            <CopyButton text={s.content} label="Copy {s.file}" dark />
          </div>
          <pre class="mono">{s.content}</pre>
        </div>
      {/if}
    </section>
  {/if}

  <section aria-label="Your agent">
    <h2 class="eyebrow">Your agent</h2>
    {#if grants === null && !error}
      <p class="small muted">Loading…</p>
    {:else if !mine.length}
      <div class="empty">
        The policy has no agent acting for {me?.id ?? 'you'}. Add one under <span class="mono">agents:</span> in the policy file.
      </div>
    {:else}
      <div class="list">
        {#each mine as g (g.id)}{@render grantCard(g, true)}{/each}
      </div>
    {/if}
  </section>

  {#if others.length}
    <section aria-label="Other agents">
      <h2 class="eyebrow">Other agents</h2>
      <div class="list">
        {#each others as g (g.id)}{@render grantCard(g, false)}{/each}
      </div>
    </section>
  {/if}

  {#if mcpCalls.list.length}
    <section aria-label="MCP calls">
      <h2 class="eyebrow">MCP calls, live in this tab</h2>
      <ol class="calls">
        {#each mcpCalls.list as c, i (i)}
          <li class:failed={c.failed}>
            <span class="t muted" title={dateTime(c.at)}>{clock(c.at)}</span>
            <span class="who">{c.actor}</span>
            <span class="txt">{c.text}</span>
            <span class="res"
              >{#if c.failed}<Glyph name={c.denied ? 'lock' : 'x'} size={12} /> {c.denied ? 'denied' : 'failed'}{c.reason && c.reason !== 'failed'
                  ? `: ${reasonText(c.reason)}`
                  : ''}{:else}<Glyph name="check" size={12} /> ok{/if}</span
            >
          </li>
        {/each}
      </ol>
    </section>
  {/if}

  {#if audit.length}
    <section aria-label="Audit">
      <h2 class="eyebrow">Recent agent events</h2>
      <table class="table au">
        <tbody>
          {#each audit as a (a.id)}
            <tr>
              <td class="muted" title={dateTime(a.time)}>{ago(a.time, now)}</td>
              <td class="mono">{a.action}</td>
              <td>{a.actor}{a.target ? ` → ${a.target}` : ''}</td>
              <td class="muted">{a.detail ?? ''}</td>
            </tr>
          {/each}
        </tbody>
      </table>
    </section>
  {/if}
</div>

<Modal bind:open={revokeOpen} title="Revoke {revokeOf?.id ?? 'agent'}?" icon="x" width={460} onclose={() => (revokeOf = null)}>
  <p>The token stops working at once. Calls that run now fail; commits the agent already pushed stay in its change.</p>
  {#snippet footer()}
    <button class="btn" onclick={() => (revokeOpen = false)}>Cancel</button>
    <button class="btn danger primary" disabled={!!busy} onclick={revoke}>
      {#if busy?.startsWith('revoke')}<span class="spin"></span>{/if} Revoke
    </button>
  {/snippet}
</Modal>

<style>
  /* One column that may shrink below its content (phone width). */
  .page {
    grid-template-columns: minmax(0, 1fr);
  }
  .calls {
    list-style: none;
    margin: 0;
    padding: 10px 12px;
    background: var(--gsi-navy);
    color: #e7e9ee;
    border-radius: var(--radius-s);
    font: 12.5px/1.7 var(--mono);
    font-variant-ligatures: none;
    max-height: 260px;
    max-width: 860px;
    overflow: auto;
  }
  .calls li {
    display: grid;
    grid-template-columns: 44px 110px minmax(0, 1fr) auto;
    gap: 10px;
    align-items: baseline;
  }
  .calls .t {
    color: #9aa1ad;
  }
  .calls .who {
    color: var(--gsi-orange);
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .calls .txt {
    overflow-wrap: anywhere;
  }
  .calls .res {
    color: #7bd88f;
    white-space: nowrap;
  }
  .calls li.failed .res {
    color: #ff8a80;
  }
  @media (max-width: 640px) {
    .calls li {
      grid-template-columns: 44px minmax(0, 1fr);
    }
    .calls .who,
    .calls .res {
      grid-column: 2;
    }
  }
  h2.eyebrow {
    margin: 0 0 8px;
  }
  .list {
    display: grid;
    gap: 12px;
    max-width: 860px;
  }
  .grant {
    border: 1px solid var(--line);
    border-radius: var(--radius);
    padding: 12px 14px;
    display: grid;
    gap: 10px;
    background: var(--panel);
  }
  .grant.on {
    border-color: color-mix(in srgb, var(--ok) 40%, var(--line));
  }
  .gh {
    display: flex;
    align-items: center;
    gap: 10px;
    flex-wrap: wrap;
  }
  .av {
    width: 34px;
    height: 34px;
    border-radius: 50%;
    display: grid;
    place-items: center;
    background: var(--accent-soft);
    color: var(--accent);
    flex: none;
  }
  .who h3 {
    margin: 0;
    font-size: 15px;
    text-transform: none;
    letter-spacing: 0;
    color: var(--ink);
  }
  .who {
    display: grid;
  }
  .can {
    display: flex;
    gap: 6px;
    flex-wrap: wrap;
  }
  .cap {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    font-size: 12px;
    padding: 1px 8px;
    border-radius: 4px;
    background: var(--ok-soft);
    color: var(--ok);
  }
  .cap.no {
    background: var(--bad-soft);
    color: var(--bad);
  }
  .tokbar {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 7px 10px;
    border-radius: 6px;
    background: var(--accent-soft);
    color: var(--accent);
    font-size: 13px;
    flex-wrap: wrap;
  }
  .tokbar b {
    color: var(--ink);
  }
  .tokbar .mode {
    font-size: 12px;
  }
  .grant p {
    margin: 0;
  }
  .acts {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 8px;
  }
  .acts .btn,
  .hrs .btn {
    display: inline-flex;
    align-items: center;
    gap: 6px;
  }
  .approve {
    display: grid;
    gap: 10px;
  }
  .modes {
    border: 0;
    padding: 0;
    margin: 0;
    display: grid;
    grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
    gap: 8px;
  }
  .modes legend {
    margin-bottom: 6px;
  }
  .mode-opt {
    display: flex;
    gap: 8px;
    align-items: flex-start;
    border: 1px solid var(--line);
    border-radius: 7px;
    padding: 8px 10px;
    cursor: pointer;
  }
  .mode-opt.sel {
    border-color: var(--accent-fill);
    background: color-mix(in srgb, var(--accent-soft) 50%, transparent);
  }
  .mode-opt input {
    margin-top: 3px;
  }
  .mode-opt > span {
    display: grid;
    gap: 2px;
  }
  .hrs {
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .once {
    background: var(--gsi-navy);
    color: #e7e9ee;
    border-radius: var(--radius);
    padding: 14px 16px;
    display: grid;
    gap: 10px;
    max-width: 860px;
    box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent-fill) 55%, transparent);
  }
  .oh {
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .oh :global(svg) {
    color: var(--gsi-orange);
  }
  .dim {
    color: #a3a9b5;
  }
  .once p {
    margin: 0;
  }
  .once code {
    color: #e7e9ee;
  }
  .x {
    background: transparent;
    color: #e7e9ee;
    border-color: #6b7280;
  }
  .tok {
    display: flex;
    align-items: center;
    gap: 10px;
    background: #15181d;
    border-radius: 6px;
    padding: 8px 10px;
  }
  .tok code {
    flex: 1;
    font-size: 13px;
    overflow-wrap: anywhere;
    color: #f2c14e;
  }
  .tabs {
    display: flex;
    gap: 2px;
    border-bottom: 1px solid #3b414c;
  }
  .tabs button {
    border: 0;
    background: none;
    color: #a3a9b5;
    padding: 5px 12px;
    font-size: 13px;
    border-bottom: 2px solid transparent;
    margin-bottom: -1px;
  }
  .tabs button[aria-selected='true'] {
    color: #fff;
    border-bottom-color: var(--gsi-orange);
  }
  .snip {
    display: grid;
    gap: 4px;
  }
  .sh {
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .snip pre {
    margin: 0;
    background: #15181d;
    color: #e7e9ee;
    border: 0;
    border-radius: 6px;
    padding: 10px 12px;
    font-size: 12px;
    overflow-x: auto;
    max-height: 300px;
  }
  .snip pre.cmd {
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  .pr {
    color: var(--gsi-orange);
    user-select: none;
  }
  .au {
    max-width: 860px;
    font-size: 13px;
  }
  @media (max-width: 640px) {
    .modes {
      grid-template-columns: minmax(0, 1fr);
    }
  }
  /* Wide tables scroll on their own at phone width. */
  @media (max-width: 700px) {
    .table {
      display: block;
      overflow-x: auto;
    }
    .table td {
      white-space: nowrap;
    }
  }
</style>
