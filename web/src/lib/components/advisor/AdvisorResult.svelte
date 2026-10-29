<script lang="ts">
  // The answer of an advisor recipe: numbered points with severity and
  // sources, the model label, the advisory-only note, thumbs up/down, Copy
  // and Run again. A fresh answer reveals its points one by one.
  import { onMount, untrack } from 'svelte';
  import type { AdvisorResult, Suggestion } from '../../api/ftypes';
  import SourceChip from './SourceChip.svelte';
  import Thumbs from './Thumbs.svelte';
  import { session } from '../../stores/me.svelte';
  import { modelLabel, offlineReason } from '../../f/llm';
  import { reducedMotion, resultMarkdown } from '../../f/advisor';
  import SuggestionCard from './SuggestionCard.svelte';
  import CopyButton from '../CopyButton.svelte';
  import Glyph from '../Glyph.svelte';

  let {
    result,
    showReading = false,
    reveal = false,
    by,
    onRerun,
    busy = false,
  }: {
    result: AdvisorResult;
    showReading?: boolean;
    /** Show the points one by one (a fresh answer). */
    reveal?: boolean;
    /** Who asked, when it was not the viewer ("dennis/agent"). */
    by?: string;
    /** Run the recipe again. */
    onRerun?: () => void;
    /** A run of this recipe is in progress. */
    busy?: boolean;
  } = $props();

  let rating = $state<'up' | 'down' | undefined>(undefined);
  let suggestions = $state<Suggestion[]>([]);
  // The model state when this answer arrived: offline means the bench's
  // deterministic findings answered (unless the result says otherwise).
  let offline = $state(false);
  let offlineWhy = $state('');
  $effect.pre(() => {
    rating = result.rating;
    suggestions = result.suggestions ?? [];
    const llm = untrack(() => session.status?.llm);
    offline = !!llm && !llm.mock && !llm.reachable;
    offlineWhy = llm && !llm.mock && !llm.reachable ? offlineReason(llm.detail) : '';
    untrack(() => session.noteAnswer(result));
  });
  const ml = $derived(modelLabel(result, offline));
  const modelTitle = $derived(
    ml.fallback && offlineWhy ? `${result.model || 'The model'} did not answer: ${offlineWhy}. This answer comes from the project data.` : ml.title,
  );

  // Staged reveal: points one by one, then the rest.
  const STEP = 160;
  // Only the first value counts: a card reveals once, when it mounts.
  const staged = untrack(() => reveal) && reducedMotion() === false;
  let shown = $state(staged ? 0 : Infinity);
  onMount(() => {
    if (!staged) return;
    const n = untrack(() => result.points.length);
    const t = setInterval(() => {
      shown += 1;
      if (shown > n) clearInterval(t);
    }, STEP);
    return () => clearInterval(t);
  });
  const rest = $derived(shown > result.points.length);

  const SEV: Record<string, string> = { risk: 'risk', warn: 'check', info: 'note', ok: 'ok' };
  const copyText = $derived(resultMarkdown(result));
</script>

<article class="adv" aria-label={result.title}>
  <header>
    <span class="spark"><Glyph name="sparkle" size={16} /></span>
    <h4>{result.title}</h4>
    {#if by}<span class="by" title="An agent asked this through MCP"><Glyph name="bot" size={13} />asked by {by}</span>{/if}
    <span class="model" class:fallback={ml.fallback} title={modelTitle}>{ml.text}</span>
  </header>

  {#if result.points.length}
    <ol class="points">
      {#each result.points as p, i (i)}
        <li class="sev-{p.severity}" class:in={i < shown}>
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

  <div class="rest" class:in={rest}>
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
      <span class="spacer"></span>
      <CopyButton text={copyText} />
      {#if onRerun}
        <button type="button" class="btn small" onclick={onRerun} disabled={busy} title="Ask the advisor again">
          <Glyph name="refresh" size={12} />Run again
        </button>
      {/if}
    </footer>
  </div>
</article>

<style>
  .adv {
    border: 1px solid var(--ai-line);
    border-left: 4px solid var(--ai-fill);
    border-radius: var(--radius);
    background: var(--panel);
    padding: 12px 16px 12px 18px;
    display: grid;
    gap: 10px;
    min-width: 0;
  }
  header {
    display: flex;
    align-items: center;
    gap: 8px;
    min-width: 0;
  }
  .spark {
    display: inline-grid;
    place-items: center;
    width: 26px;
    height: 26px;
    border-radius: 50%;
    background: var(--ai-soft);
    color: var(--ai);
    flex: none;
  }
  h4 {
    margin: 0;
    font-size: 16px;
    flex: 1;
    min-width: 0;
  }
  .by {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    font: 600 12px var(--sans);
    color: var(--ai);
    background: var(--ai-soft);
    border-radius: 999px;
    padding: 2px 9px;
    white-space: nowrap;
  }
  .model {
    font: 500 11.5px/1.6 var(--mono);
    color: var(--ai);
    border: 1px solid var(--ai-line);
    border-radius: 4px;
    padding: 0 6px;
    white-space: nowrap;
  }
  /* The answer came from the project data: say so, without alarm. */
  .model.fallback {
    color: var(--muted);
    border-color: var(--line);
    background: var(--panel-2);
  }
  .points {
    margin: 0;
    padding: 0;
    list-style: none;
    display: grid;
    gap: 9px;
  }
  .points li {
    display: grid;
    grid-template-columns: 50px 1fr;
    gap: 10px;
    align-items: baseline;
    line-height: 1.5;
    font-size: 15px;
    opacity: 0;
    transform: translateY(6px);
    transition:
      opacity 0.25s ease-out,
      transform 0.25s ease-out;
  }
  .points li.in,
  .rest.in {
    opacity: 1;
    transform: none;
  }
  .rest {
    display: grid;
    gap: 10px;
    opacity: 0;
    transition: opacity 0.3s ease-out;
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
    font-size: 13px;
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
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .spacer {
    flex: 1;
  }
  footer .btn {
    display: inline-flex;
    align-items: center;
    gap: 5px;
  }
  @media (prefers-reduced-motion: reduce) {
    .points li,
    .rest {
      transition: none;
    }
  }
</style>
