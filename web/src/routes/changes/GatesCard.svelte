<script lang="ts">
  // Gates from .pk/policy.yaml: lab gate, approvals, rebased, path rights,
  // risk. Same evaluation as pk ci and the merge check.
  import type { Change, Gates } from '../../lib/api/types';
  import { runLabel } from '../../lib/change/format';
  import { actions } from '../../lib/change/actions.svelte';
  import { session } from '../../lib/stores/me.svelte';
  import Icon from '../../lib/components/Icon.svelte';

  let { c, gates }: { c: Change; gates: Gates } = $props();

  type Tone = 'ok' | 'wait' | 'bad' | 'na';

  const lab = $derived.by(() => {
    const g = gates.labGate;
    const run = g.run ? runLabel(g.run) : '';
    const map: Record<string, [Tone, string]> = {
      met: ['ok', `${run} pass`],
      needed: ['wait', run ? `${run} is for an old head` : 'no run yet'],
      running: ['wait', `${run} running`],
      failed: ['bad', `${run} failed`],
      na: ['na', 'not required'],
    };
    const [tone, hint] = map[g.status] ?? ['na', g.status];
    return { tone, hint };
  });

  const labPaths = $derived(
    gates.matchedRules
      .filter((r) => r.labGate)
      .map((r) => r.name)
      .join(', '),
  );
  const approvalRules = $derived(
    gates.matchedRules
      .filter((r) => r.approvals === gates.approvals.need)
      .map((r) => r.name)
      .join(', ') || 'policy',
  );
  const others = $derived((gates.approvals.eligible ?? []).filter((p) => !gates.approvals.by.includes(p)));
  const writers = $derived(gates.pathRights.writers.map((w) => (w.endsWith('-agent') ? 'agent' : w)).join(', '));
  const target = $derived(c.parent ?? gates.rebased.target ?? c.target);
  const canDemo = $derived(!!session.me?.demo && c.pushed);
</script>

<div class="card gates">
  <div class="eyebrow">Gates from .pk/policy.yaml</div>
  <ul>
    <li class="t-{lab.tone}">
      <Icon name="flask" size={17} />
      <span class="what">{gates.labGate.required ? 'Green lab on branch head' : 'Lab'}</span>
      <span class="hint mono" title={gates.labGate.detail}>{lab.hint}</span>
      {#if gates.labGate.required && labPaths}<span class="sub">lab gate from rules: {labPaths}</span>{/if}
    </li>
    <li class="t-{gates.approvals.have >= gates.approvals.need ? 'ok' : 'wait'}">
      <Icon name="people" size={17} />
      <span class="what">Approvals {gates.approvals.have} of {gates.approvals.need}</span>
      <span class="hint mono">{approvalRules}</span>
      <span class="sub">
        {#if gates.approvals.by.length}by {gates.approvals.by.join(', ')}.{/if}
        {#if others.length}Can approve: {others.join(', ')}.{/if}
        {#each gates.approvals.ignored ?? [] as a (a.user)}
          <span class="ign" title={a.reason}>{a.user} does not count{a.reason ? ` (${a.reason})` : ''}.</span>
        {/each}
        {#if canDemo && gates.approvals.have < gates.approvals.need && c.state !== 'draft'}
          <span class="demo">
            demo:
            {#each others.filter((p) => p !== session.me?.id).slice(0, 3) as p (p)}
              <button class="link" disabled={actions.isBusy(c.id)} onclick={() => actions.demoApprove(c.id, p)}>approve as {p}</button>
            {/each}
          </span>
        {/if}
      </span>
    </li>
    <li class="t-{gates.rebased.ok ? 'ok' : 'wait'}">
      <Icon name="rebase" size={17} />
      <span class="what">{gates.rebased.ok ? `Rebased on ${target}` : `Behind ${target} by ${gates.rebased.behind}`}</span>
      {#if gates.rebased.ok}
        <span class="hint mono">up to date</span>
      {:else if c.local}
        <button class="hint mono link" disabled={actions.isBusy(c.id)} onclick={() => actions.rebase(c.id)}>one-click rebase</button>
      {:else}
        <span class="hint mono">rebase in its worktree</span>
      {/if}
    </li>
    <li class="t-{gates.pathRights.ok ? 'ok' : 'bad'}">
      <Icon name={gates.pathRights.ok ? 'check' : 'x'} size={17} />
      <span class="what">Write access: {writers || c.author}</span>
      <span class="hint mono">{gates.pathRights.ok ? 'allowed by policy' : 'not allowed'}</span>
      {#each gates.pathRights.details.filter((d) => !d.ok) as d (d.path + d.who)}
        <span class="sub bad">{d.who} may not write {d.path}{d.rule ? ` (rule ${d.rule})` : ''}{d.message ? `: ${d.message}` : ''}</span>
      {/each}
    </li>
    {#if gates.fastPath}
      <li class="t-ok">
        <Icon name="rebase" size={17} />
        <span class="what">Revert fast path</span>
        <span class="hint mono">policy fastPath</span>
      </li>
    {/if}
  </ul>
  {#if gates.mergeable}
    <div class="verdict ok"><Icon name="check" size={14} /> All gates met: ready to merge.</div>
  {:else if gates.blockers.length && c.state !== 'draft'}
    <div class="verdict"><Icon name="info" size={14} /> Before merge: {gates.blockers.join('; ')}</div>
  {/if}
</div>

<style>
  .gates {
    container-type: inline-size;
    display: grid;
    gap: 10px;
    align-content: start;
  }
  ul {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    gap: 9px;
  }
  li {
    display: grid;
    grid-template-columns: 22px minmax(0, 1fr) auto;
    align-items: baseline;
    column-gap: 10px;
    font-size: 14px;
  }
  li > :global(svg) {
    align-self: center;
  }
  .t-ok > :global(svg) {
    color: var(--ok);
  }
  .t-wait > :global(svg) {
    color: var(--warn);
  }
  .t-bad > :global(svg) {
    color: var(--bad);
  }
  .t-na > :global(svg) {
    color: var(--muted);
  }
  .what {
    min-width: 0;
    overflow-wrap: break-word;
  }
  .hint {
    color: var(--muted);
    font-size: 12.5px;
    text-align: right;
    min-width: 0;
    overflow-wrap: anywhere;
  }
  /* Narrow card: the value goes under its label, never over it. */
  @container (max-width: 340px) {
    li {
      grid-template-columns: 22px minmax(0, 1fr);
    }
    .hint {
      grid-column: 2;
      text-align: left;
    }
  }
  .t-bad .hint {
    color: var(--bad);
  }
  .sub {
    grid-column: 2 / -1;
    color: var(--muted);
    font-size: 12.5px;
  }
  .sub.bad {
    color: var(--bad);
  }
  .ign {
    color: var(--warn);
  }
  .demo {
    display: inline-flex;
    gap: 8px;
    margin-left: 6px;
  }
  .link {
    border: 0;
    background: none;
    padding: 0;
    color: var(--accent);
    text-decoration: underline;
    text-underline-offset: 2px;
    cursor: pointer;
  }
  .verdict {
    display: flex;
    gap: 6px;
    align-items: center;
    font-size: 12.5px;
    color: var(--muted);
    border-top: 1px solid var(--line);
    padding-top: 8px;
  }
  .verdict.ok {
    color: var(--ok);
  }
</style>
