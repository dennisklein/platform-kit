<script lang="ts">
  // The answer of an advisor recipe: numbered points with severity and
  // sources, the model label, the advisory-only note and thumbs up/down.
  import { untrack } from 'svelte';
  import type { AdvisorResult, Suggestion } from '../../api/ftypes';
  import SourceChip from './SourceChip.svelte';
  import Thumbs from './Thumbs.svelte';
  import { session } from '../../stores/me.svelte';
  import { modelLabel } from '../../f/llm';
  import SuggestionCard from './SuggestionCard.svelte';

  let {
    result,
    showReading = false,
  }: { result: AdvisorResult; showReading?: boolean } = $props();

  let rating = $state<'up' | 'down' | undefined>(undefined);
  let suggestions = $state<Suggestion[]>([]);
  // The model state when this answer arrived: offline means the bench's
  // deterministic findings answered (unless the result says otherwise).
  let offline = $state(false);
  $effect.pre(() => {
    rating = result.rating;
    suggestions = result.suggestions ?? [];
    const llm = untrack(() => session.status?.llm);
    offline = !!llm && !llm.mock && !llm.reachable;
    untrack(() => session.noteAnswer(result));
  });
  const ml = $derived(modelLabel(result, offline));

  const SEV: Record<string, string> = { risk: 'risk', warn: 'check', info: 'note', ok: 'ok' };
</script>

<article class="adv" aria-label={result.title}>
  <header>
    <h4>{result.title}</h4>
    <span class="model" class:fallback={ml.fallback} title={ml.title}>{ml.text}</span>
  </header>

  {#if result.points.length}
    <ol class="points">
      {#each result.points as p, i (i)}
        <li class="sev-{p.severity}">
          <span class="sev" title="severity: {p.severity}">{SEV[p.severity] ?? p.severity}</span>
          <div class="body">
            {#if p.title}<strong>{p.title.replace(/\.?$/, '.')}</strong>{/if}
            {p.text}
            {#each p.sources as s, j (j)}
              <SourceChip source={s} changeId={result.changeId} />{' '}
            {/each}
          </div>
        </li>
      {/each}
    </ol>
  {:else}
    <p class="muted small">Nothing to report.</p>
  {/if}

  {#each suggestions as s, i (s.id)}
    <div class="sg"><SuggestionCard bind:suggestion={suggestions[i]} note={false} /></div>
  {/each}

  {#if result.note}<p class="note">{result.note}</p>{/if}

  {#if showReading && result.reading?.length}
    <p class="reading small muted">
      Read:
      {#each result.reading as s, i (i)}<SourceChip source={s} changeId={result.changeId} />{' '}{/each}
    </p>
  {/if}

  <footer>
    <Thumbs logId={result.logId} bind:rating />
  </footer>
</article>

<style>
  .adv {
    border: 1px solid var(--line);
    border-left: 4px solid var(--info);
    border-radius: var(--radius);
    background: var(--panel);
    padding: 12px 16px 12px 18px;
    display: grid;
    gap: 8px;
    min-width: 0;
  }
  header {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: 8px;
  }
  h4 {
    margin: 0;
    font-size: 15px;
  }
  .model {
    font: 500 11px/1.6 var(--mono);
    color: var(--muted);
    border: 1px solid var(--line);
    border-radius: 4px;
    padding: 0 6px;
    white-space: nowrap;
  }
  .model.fallback {
    color: var(--warn);
    border-color: color-mix(in srgb, var(--warn) 40%, transparent);
    background: var(--warn-soft);
    white-space: normal;
  }
  .points {
    margin: 0;
    padding: 0;
    list-style: none;
    display: grid;
    gap: 8px;
    counter-reset: pt;
  }
  .points li {
    display: grid;
    grid-template-columns: 46px 1fr;
    gap: 8px;
    align-items: baseline;
    line-height: 1.5;
  }
  .sev {
    font: 600 10.5px/1.7 var(--mono);
    text-transform: uppercase;
    letter-spacing: 0.04em;
    text-align: center;
    border-radius: 4px;
    background: var(--line);
    color: var(--muted);
  }
  .sev-risk .sev {
    background: var(--bad-soft);
    color: var(--bad);
  }
  .sev-warn .sev {
    background: var(--warn-soft);
    color: var(--warn);
  }
  .sev-ok .sev {
    background: var(--ok-soft);
    color: var(--ok);
  }
  .sev-info .sev {
    background: var(--info-soft);
    color: var(--info);
  }
  .body :global(.src) {
    margin-left: 2px;
  }
  .note {
    margin: 0;
    font-size: 12.5px;
    color: var(--muted);
  }
  .reading {
    margin: 0;
  }
  .sg {
    padding: 8px 0 2px;
    border-top: 1px dashed var(--line);
  }
  footer {
    border-top: 1px solid var(--line);
    padding-top: 8px;
  }
</style>
