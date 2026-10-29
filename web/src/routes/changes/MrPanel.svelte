<script lang="ts">
  // Merge request: the description with trailers (from the metadata file
  // while draft) and the checklist before review.
  import type { Change } from '../../lib/api/types';
  import { actions } from '../../lib/change/actions.svelte';
  import { runLabel, shortSha } from '../../lib/change/format';
  import Icon from '../../lib/components/Icon.svelte';
  import { descriptionPreview, mergePreview } from './trailers';

  let { c }: { c: Change } = $props();

  const draft = $derived(!c.mr || c.mr.draft);
  const merged = $derived(['merged', 'deployed', 'verified'].includes(c.state));
  const own = $derived(c.commits.filter((x) => !x.subject.startsWith('chore(change):')));
  const pushedCommits = $derived(c.pushed && c.aheadRemote === 0);
  const lab = $derived(c.gates?.labGate);
  const labOk = $derived(!lab || !lab.required || lab.status === 'met');
  // Merged changes carry no gates: show the last recorded lab run.
  const lastRun = $derived(c.labRuns.length ? c.labRuns[c.labRuns.length - 1] : undefined);
  const metaMoved = $derived(!!c.mr && !c.mr.draft);
  const appr = $derived(c.gates?.approvals ?? { have: c.approvals.have, need: c.approvals.need, by: [] });
  const canReady = $derived(c.local && c.pushed && draft && labOk && !merged);

  let view = $state<'description' | 'merge'>('description');
  $effect(() => {
    if (!draft) view = 'merge';
  });
</script>

<section class="card mr">
  <div class="head">
    <h2>{merged ? 'Merged' : c.mr ? 'Merge request' : 'Push and open merge request'}</h2>
    {#if c.mr}
      <a class="mrchip" href={c.mr.url} target="_blank" rel="noreferrer">
        MR !{c.mr.number} · {c.mr.state === 'merged' ? 'merged' : c.mr.draft ? 'draft' : 'review'}
        <Icon name="external" size={12} />
      </a>
    {/if}
    <span class="spacer"></span>
    {#if !draft}
      <div class="tabs" role="tablist">
        <button role="tab" aria-selected={view === 'description'} onclick={() => (view = 'description')}>Description</button>
        <button role="tab" aria-selected={view === 'merge'} onclick={() => (view = 'merge')}>Merge commit</button>
      </div>
    {/if}
  </div>
  <div class="grid">
    <div class="desc">
      <div class="eyebrow">
        {#if view === 'merge'}Merge commit preview (trailers){:else if c.description}MR description{:else}MR description, from .pk/changes/{c.id}.yaml{/if}
      </div>
      <pre>{view === 'merge' ? mergePreview(c) : descriptionPreview(c)}</pre>
    </div>
    <div class="check">
      <div class="eyebrow">{merged ? 'Done' : 'Before review'}</div>
      <ul>
        <li class:ok={pushedCommits}>
          <Icon name={pushedCommits ? 'check' : 'upload'} size={16} />
          <span>{own.length} commit{own.length === 1 ? '' : 's'} {pushedCommits ? 'pushed' : c.pushed ? `pushed, ${c.aheadRemote} not yet` : 'not pushed'}</span>
          <span class="hint mono">{own.slice(0, 3).map((x) => x.short || shortSha(x.sha)).join(', ')}</span>
        </li>
        {#if !lab && merged && lastRun}
          <li class:ok={lastRun.result === 'pass'} class:bad={lastRun.result === 'fail'}>
            <Icon name={lastRun.result === 'pass' ? 'check' : 'flask'} size={16} />
            <span>{lastRun.result === 'pass' ? 'Lab passed' : `Lab ${lastRun.result}`}</span>
            <span class="hint mono">{runLabel(lastRun.run)} {lastRun.result}</span>
          </li>
        {:else}
          <li class:ok={labOk} class:bad={lab?.status === 'failed'}>
            <Icon name={labOk ? 'check' : 'flask'} size={16} />
            <span>{!lab?.required ? 'No lab gate' : labOk ? 'Lab gate met' : lab.status === 'running' ? 'Lab running' : 'Lab gate needed'}</span>
            <span class="hint mono">{lab?.run ? `${runLabel(lab.run)} ${lab.status === 'met' ? 'pass' : lab.status}` : lab?.required ? 'no run' : ''}</span>
          </li>
        {/if}
        <li class:ok={metaMoved}>
          <Icon name={metaMoved ? 'check' : 'file'} size={16} />
          <span>Metadata file moved to the MR</span>
          <span class="hint mono">{metaMoved ? 'main stays clean' : 'on mark ready'}</span>
        </li>
        <li class:ok={appr.have >= appr.need && appr.need > 0}>
          <Icon name="people" size={16} />
          <span>Approvals {appr.have} of {appr.need}</span>
          <span class="hint mono">{appr.by.length ? appr.by.join(', ') : 'policy'}</span>
        </li>
      </ul>
      {#if !merged}
        <div class="row">
          {#if c.local && (!c.pushed || c.aheadRemote > 0)}
            <button class="btn dark" disabled={actions.isBusy(c.id)} onclick={() => actions.push(c.id)}>
              {#if actions.isBusy(c.id, 'push')}<span class="spin"></span>{:else}<Icon name="upload" size={15} />{/if}
              {c.pushed ? 'Push' : 'Push and open draft MR'}
            </button>
          {/if}
          {#if draft && c.pushed}
            <button class="btn dark" disabled={!canReady || actions.isBusy(c.id)} onclick={() => actions.ready(c.id)} title={labOk ? 'Remove WIP, write trailers, delete the metadata file' : 'The lab gate is not met'}>
              {#if actions.isBusy(c.id, 'ready')}<span class="spin"></span>{:else}<Icon name="check" size={15} />{/if}
              Mark ready for review
            </button>
          {/if}
          {#if !labOk && c.local && lab?.status !== 'running'}
            <button class="btn" disabled={actions.isBusy(c.id)} onclick={() => actions.runLab(c.id)}><Icon name="flask" size={15} /> Run lab</button>
          {/if}
        </div>
      {/if}
    </div>
  </div>
</section>

<style>
  .mr {
    display: grid;
    gap: 12px;
  }
  .head {
    display: flex;
    align-items: center;
    gap: 12px;
    flex-wrap: wrap;
  }
  .mrchip {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    font-size: 12.5px;
    font-weight: 600;
    background: var(--info-soft);
    color: var(--info);
    padding: 2px 9px;
    border-radius: 4px;
    text-decoration: none;
  }
  .tabs {
    display: flex;
    gap: 2px;
  }
  .tabs button {
    border: 0;
    background: none;
    padding: 3px 10px;
    border-radius: 5px;
    color: var(--muted);
    font-size: 13px;
  }
  .tabs button[aria-selected='true'] {
    background: var(--panel-2);
    color: var(--ink);
    box-shadow: inset 0 0 0 1px var(--line);
  }
  .grid {
    display: grid;
    grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
    gap: 16px;
  }
  @media (max-width: 1100px) {
    .grid {
      grid-template-columns: 1fr;
    }
  }
  .desc,
  .check {
    display: grid;
    gap: 8px;
    align-content: start;
  }
  pre {
    background: var(--panel);
    font-size: 13px;
    line-height: 1.65;
    white-space: pre-wrap;
    border-color: var(--line-strong);
    padding: 12px 16px;
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
    grid-template-columns: 20px minmax(0, 1fr) auto;
    gap: 10px;
    align-items: center;
    font-size: 14px;
  }
  li > :global(svg) {
    color: var(--warn);
  }
  li.ok > :global(svg) {
    color: var(--ok);
  }
  li.bad > :global(svg) {
    color: var(--bad);
  }
  .hint {
    color: var(--muted);
    font-size: 12.5px;
  }
  .row {
    margin-top: 4px;
  }
</style>
