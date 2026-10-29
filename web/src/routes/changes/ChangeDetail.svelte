<script lang="ts">
  // One change: header, lifecycle, gates, overlap, impact, merge request,
  // commits and files, lab runs, rollout.
  import type { Params } from '../../lib/router.svelte';
  import { href, seg } from '../../lib/router.svelte';
  import type { LifecycleStep } from '../../lib/api/types';
  import { change } from '../../lib/stores/change.svelte';
  import { changes } from '../../lib/stores/changes.svelte';
  import { presenceFor } from '../../lib/change/presence.svelte';
  import { runLabel, shortSha, timeAgo, plural, tildePath, baseName } from '../../lib/change/format';
  import StateChip from '../../lib/components/StateChip.svelte';
  import Track, { type TrackItem } from '../../lib/components/Track.svelte';
  import Icon from '../../lib/components/Icon.svelte';
  import PresenceChip from '../../lib/components/PresenceChip.svelte';
  import AgentBanner from '../../lib/components/AgentBanner.svelte';
  import OverlapCard from '../../lib/components/OverlapCard.svelte';
  import ChangeActions from './ChangeActions.svelte';
  import GatesCard from './GatesCard.svelte';
  import MrPanel from './MrPanel.svelte';
  import RolloutCard from './RolloutCard.svelte';
  import { api, must } from '../../lib/api/client';
  import type { ImpactSummary } from '../../lib/api/types';

  let { params }: { params: Params; query: URLSearchParams } = $props();

  $effect(() => change.use(params.id));

  const c = $derived(change.data?.id === params.id ? change.data : null);
  const summary = $derived(changes.byId(params.id));
  const presence = $derived(presenceFor(params.id, c?.presence ?? summary?.presence));

  const SUB: Record<string, string> = {
    draft: 'branch',
    lab: 'lab runs',
    review: 'MR',
    approved: 'approvals',
    merged: 'trailer',
    deployed: 'Flux',
    verified: 'health',
  };

  const track = $derived<TrackItem[]>(
    (c?.lifecycle ?? []).map((s: LifecycleStep) => ({
      key: s.state,
      title: s.state,
      sub: s.label || SUB[s.state],
      status: s.status === 'now' && s.state === 'lab' && c?.labState === 'running' ? 'running' : s.status,
      detail: s.detail,
    })),
  );
  let selected = $state<string | undefined>(undefined);
  const selectedStep = $derived(c?.lifecycle.find((s) => s.state === selected));

  const merged = $derived(c ? ['merged', 'deployed', 'verified'].includes(c.state) : false);
  // A draft whose lab passed on the head: the lab gate is met, review is next.
  const labPassed = $derived(!!c && c.state === 'draft' && c.labState === 'pass');
  const agentAuthored = $derived(!!c && (c.authorKind === 'agent' || !!c.agent));

  // A merged change has no live impact summary: take it from the impact of
  // its merge commit (what it changed).
  let mergedImpact = $state<ImpactSummary | null>(null);
  $effect(() => {
    const cur = c;
    mergedImpact = null;
    if (!cur || !merged || cur.impact) return;
    const i = cur.id;
    void must(api.GET('/changes/{id}/impact', { params: { path: { id: i } } }))
      .then((r) => {
        if (c?.id === i) mergedImpact = r.summary;
      })
      .catch(() => {});
  });
  const impactSummary = $derived(c?.impact ?? mergedImpact ?? undefined);
  const impactClusters = $derived((impactSummary?.clusters ?? []).filter((x) => x.added + x.changed + x.deleted > 0));
  const quietClusters = $derived((impactSummary?.clusters ?? []).filter((x) => x.added + x.changed + x.deleted === 0).map((x) => x.cluster));

  const FILE_ICON: Record<string, string> = { added: '+', modified: 'M', deleted: '−', renamed: 'R', untracked: '+' };
</script>

<div class="page">
  {#if change.error && !c}
    <div class="error-box">Change {params.id}: {change.error}</div>
  {:else if !c}
    <div class="muted">Loading {params.id}…</div>
  {:else}
    {#if presence}<AgentBanner {presence} />{/if}

    <header class="head">
      <div class="titles">
        <div class="row">
          <h1>{c.title}</h1>
          {#if labPassed}
            <span class="labpass" title="The lab passed on the head. Next: push and mark ready.">lab passed</span>
          {:else}
            <StateChip state={c.state} labState={c.labState} />
          {/if}
          {#if c.risk}<span class="risk r-{c.risk}">risk: {c.risk}</span>{/if}
        </div>
        <div class="meta">
          <span class="mono">{c.branch}</span>
          {#if c.mr}<a href={c.mr.url} target="_blank" rel="noreferrer">MR !{c.mr.number} {c.mr.draft ? 'draft' : c.mr.state === 'open' ? 'review' : c.mr.state}</a>{:else}<span>no MR yet</span>{/if}
          <a href={href(`/changes/${seg(c.id)}/files`)}>{plural(c.files, 'file')}</a>
          <span>{plural(c.commits.length, 'commit')}</span>
          {#if c.parent}<span>stacked on <a href={href(`/changes/${seg(c.parent)}`)}>{c.parent}</a></span>{/if}
          {#if c.behindTarget > 0}<span class="warn-text"><b>behind {c.parent ?? c.target} by {c.behindTarget}</b></span>{/if}
          <span>author {c.author}{c.agent && c.agent !== c.author ? ` + ${c.agent}` : ''}</span>
          {#if c.dirty}<span class="info-text">uncommitted files</span>{/if}
        </div>
      </div>
      {#if presence}<PresenceChip {presence} />{/if}
    </header>

    <ChangeActions {c} />

    {#if c.intent}<p class="intent">{c.intent}</p>{/if}

    <section class="card life">
      <Track items={track} bind:selected selectable />
      {#if selectedStep}
        <div class="stepdetail small">
          <b>{selectedStep.state}</b>: {selectedStep.detail ?? selectedStep.label ?? ''}
          {#if selectedStep.state === 'lab'}
            {#each c.labRuns as r (r.run)} · <a href={href(`/changes/${seg(c.id)}/lab`, { run: r.run })}>{runLabel(r.run)} {r.result}</a>{/each}
          {:else if selectedStep.state === 'review' && c.mr}
            · <a href={c.mr.url} target="_blank" rel="noreferrer">MR !{c.mr.number}</a>
          {:else if selectedStep.state === 'approved'}
            · {c.approvalsBy.map((a) => `${a.user}${a.counts ? '' : ' (does not count)'}`).join(', ') || 'no approvals yet'}
          {:else if (selectedStep.state === 'deployed' || selectedStep.state === 'verified') && c.rollout}
            · {c.rollout.map((r) => `${r.cluster}: ${r.verified ? 'verified' : r.deployed ? 'deployed' : 'pending'}`).join(', ')}
          {/if}
        </div>
      {/if}
    </section>

    <div class="cols" class:wide={merged}>
      {#if c.gates}
        <GatesCard {c} gates={c.gates} />
      {:else if merged}
        <RolloutCard {c} />
      {:else if c.state === 'closed'}
        <div class="card muted small">Closed without merge. Gates are not evaluated.</div>
      {:else}
        <div class="card muted small">Gates are not evaluated yet.</div>
      {/if}
      <div class="card side">
        <div class="eyebrow">Overlap</div>
        {#if c.overlapsDetail.length}
          <OverlapCard overlaps={c.overlapsDetail} />
        {:else}
          <p class="muted small none">No other open change touches the same objects.</p>
        {/if}
        <a class="impact" href={href(`/changes/${seg(c.id)}/review`)}>
          <Icon name="hexagon" size={18} />
          <span>Impact: {impactSummary?.text || 'no rendered change'}</span>
          <span class="spacer"></span>
          <span class="mono muted small">{impactClusters.length ? 'review' : ''}</span>
        </a>
        {#if quietClusters.length && impactClusters.length}<span class="muted small quiet">{quietClusters.join(', ')}: no change</span>{/if}
        {#if agentAuthored && !merged}
          <div class="callout warn">
            <Icon name="shield" size={17} />
            <div>Approve and merge stay with people: <b>{c.approvals.have} of {c.approvals.need} approvals</b>. The agent has its own identity, and the forge rules block it too.</div>
          </div>
        {/if}
      </div>
    </div>

    {#if c.parent || c.children.length}
      <section class="card">
        <div class="eyebrow">Stack</div>
        <div class="stack mono small">
          <div>{c.parent ? '…' : c.target}</div>
          {#if c.parent}<div class="lvl1">└ <a href={href(`/changes/${seg(c.parent)}`)}>{c.parent}</a> {changes.byId(c.parent)?.title ?? ''}</div>{/if}
          <div class={c.parent ? 'lvl2' : 'lvl1'}>└ <b>{c.id}</b> {c.title}</div>
          {#each c.children as k (k)}
            <div class={c.parent ? 'lvl3' : 'lvl2'}>└ <a href={href(`/changes/${seg(k)}`)}>{k}</a> {changes.byId(k)?.title ?? ''}</div>
          {/each}
        </div>
      </section>
    {/if}

    <MrPanel {c} />

    <div class="cols">
      <section class="card">
        <div class="eyebrow">Commits</div>
        <ul class="commits">
          {#each c.commits as m (m.sha)}
            <li>
              <span class="sha mono">{m.short || shortSha(m.sha)}</span>
              <span class="subj" title={m.body}>{m.subject}</span>
              {#if m.agent}<span class="badge" title="Pk-Agent: {m.agent}">Pk-Agent</span>{/if}
              <span class="muted small">{m.author} · {timeAgo(m.date)}</span>
            </li>
          {:else}
            <li class="muted">none yet</li>
          {/each}
        </ul>
      </section>
      <section class="card">
        <div class="eyebrow">Changed files</div>
        <ul class="files">
          {#each c.changedFiles ?? [] as f (f.path)}
            <li>
              <span class="fs s-{f.status}" title={f.status}>{FILE_ICON[f.status] ?? '·'}</span>
              <a class="mono" href={href(`/changes/${seg(c.id)}/files`, { path: f.path })} title={f.path}>{f.path}</a>
              {#if !f.committed}<span class="badge">saved</span>{/if}
            </li>
          {:else}
            <li class="muted">none</li>
          {/each}
        </ul>
        {#if c.worktree}<div class="muted small wt">worktree <span class="mono">{tildePath(c.worktree)}</span></div>{/if}
      </section>
    </div>

    <div class="cols">
      <section class="card">
        <div class="eyebrow">Lab runs</div>
        <ul class="runs">
          {#each [...c.labRuns].reverse() as r (r.run)}
            <li>
              <a href={href(`/changes/${seg(c.id)}/lab`, { run: r.run })}><Icon name="flask" size={14} /> {runLabel(r.run)}</a>
              <span class="res r-{r.result}">{r.result}</span>
              <span class="mono muted small">{shortSha(r.sha)}</span>
              {#if r.report}<a class="small" href={href('/kb/article', { path: r.report, ref: c.id })}>{baseName(r.report)}</a>{/if}
            </li>
          {:else}
            <li class="muted small">No lab run yet.</li>
          {/each}
        </ul>
      </section>
      <section class="card">
        <div class="eyebrow">Work items</div>
        <ul class="runs">
          {#each c.workItems ?? [] as w (w)}
            <li><a href={href('/work', { item: w })} class="mono">{w}</a></li>
          {:else}
            <li class="muted small">No work item linked.</li>
          {/each}
        </ul>
      </section>
    </div>
  {/if}
</div>

<style>
  .head {
    display: flex;
    gap: 16px;
    align-items: flex-start;
    justify-content: space-between;
  }
  .titles {
    display: grid;
    gap: 6px;
    min-width: 0;
  }
  .meta {
    display: flex;
    flex-wrap: wrap;
    gap: 4px 18px;
    color: var(--muted);
    font-size: 13.5px;
  }
  .meta a {
    color: var(--muted);
  }
  .info-text {
    color: var(--info);
  }
  .risk {
    font-size: 11.5px;
    font-weight: 600;
    padding: 1px 8px;
    border-radius: 4px;
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
  .r-high {
    background: var(--bad-soft);
    color: var(--bad);
  }
  .r-medium {
    background: var(--warn-soft);
    color: var(--warn);
  }
  .r-low {
    background: var(--ok-soft);
    color: var(--ok);
  }
  .intent {
    margin: 0;
    color: var(--muted);
  }
  .life {
    padding: 16px 12px 12px;
  }
  .stepdetail {
    margin: 10px 8px 0;
    padding-top: 8px;
    border-top: 1px solid var(--line);
  }
  .cols {
    display: grid;
    grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
    gap: 16px;
  }
  /* After the merge the rollout is the story: it gets the wider column. */
  .cols.wide {
    grid-template-columns: minmax(0, 3fr) minmax(0, 2fr);
  }
  @media (max-width: 1100px) {
    .cols,
    .cols.wide {
      grid-template-columns: 1fr;
    }
  }
  .side {
    display: grid;
    gap: 10px;
    align-content: start;
  }
  .none {
    margin: 0;
  }
  .impact {
    display: flex;
    gap: 10px;
    align-items: center;
    color: var(--ink);
    text-decoration: none;
    font-size: 14px;
  }
  .impact:hover span:nth-child(2) {
    text-decoration: underline;
  }
  .quiet {
    margin-top: -6px;
    padding-left: 28px;
  }
  .stack {
    display: grid;
    gap: 4px;
    margin-top: 8px;
  }
  .lvl1 {
    padding-left: 12px;
  }
  .lvl2 {
    padding-left: 28px;
  }
  .lvl3 {
    padding-left: 44px;
  }
  ul {
    list-style: none;
    margin: 8px 0 0;
    padding: 0;
    display: grid;
    gap: 7px;
  }
  .commits li {
    display: grid;
    grid-template-columns: auto minmax(0, 1fr) auto auto;
    gap: 10px;
    align-items: baseline;
    font-size: 13.5px;
  }
  .sha {
    color: var(--accent);
  }
  .subj {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .files li {
    display: flex;
    gap: 8px;
    align-items: baseline;
    min-width: 0;
  }
  .files a {
    color: var(--ink);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .fs {
    font: 700 12px var(--mono);
    width: 14px;
    text-align: center;
    flex: none;
  }
  .s-added,
  .s-untracked {
    color: var(--ok);
  }
  .s-modified,
  .s-renamed {
    color: var(--warn);
  }
  .s-deleted {
    color: var(--bad);
  }
  .wt {
    margin-top: 10px;
  }
  .runs li {
    display: flex;
    gap: 12px;
    align-items: center;
    flex-wrap: wrap;
    font-size: 13.5px;
  }
  .runs a {
    display: inline-flex;
    gap: 4px;
    align-items: center;
  }
  .res {
    font-size: 11.5px;
    font-weight: 600;
    padding: 0 7px;
    border-radius: 4px;
    background: var(--line);
    color: var(--muted);
  }
  .res.r-pass {
    background: var(--ok-soft);
    color: var(--ok);
  }
  .res.r-fail {
    background: var(--bad-soft);
    color: var(--bad);
  }
  .res.r-running {
    background: var(--warn-soft);
    color: var(--warn);
  }
</style>
