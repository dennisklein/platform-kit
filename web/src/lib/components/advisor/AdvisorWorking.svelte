<script lang="ts">
  // The card that stands where an advisor answer will come: what the
  // recipe reads (each line ticks to a check mark), the model and the
  // seconds so far, shimmer lines and Cancel.
  import { onMount } from 'svelte';
  import Glyph from '../Glyph.svelte';
  import { session } from '../../stores/me.svelte';
  import { readingSteps, STEP_MS } from '../../f/advisor';

  let {
    title,
    tools,
    writer,
    by,
    onCancel,
    compact = false,
  }: {
    title: string;
    tools?: string[];
    writer?: string;
    by?: string;
    onCancel?: () => void;
    /** One line: the current step only (for the commit message box). */
    compact?: boolean;
  } = $props();

  const started = Date.now();
  let now = $state(Date.now());
  onMount(() => {
    const t = setInterval(() => (now = Date.now()), 100);
    return () => clearInterval(t);
  });

  const steps = $derived(readingSteps(tools, writer));
  // Step i is done after (i + 1) steps; the last one (writing) runs until
  // the answer arrives.
  const at = $derived(Math.min(steps.length - 1, Math.floor((now - started) / STEP_MS)));
  const secs = $derived(Math.floor((now - started) / 1000));
  const model = $derived.by(() => {
    const llm = session.status?.llm;
    if (!llm || llm.mock) return 'built-in';
    if (!llm.reachable) return 'from project data';
    return llm.model;
  });
</script>

{#if compact}
  <div class="line" aria-busy="true" aria-live="polite" aria-label="{title}: working">
    <span class="spin" aria-hidden="true"></span>
    <span class="now">{steps[at]}…</span>
    <span class="model mono">{model} · {secs} s</span>
  </div>
{:else}
  <article class="work" aria-busy="true" aria-label="{title}: working">
    <header>
      <span class="spark"><Glyph name="sparkle" size={16} /></span>
      <h4>{title}</h4>
      {#if by}<span class="by"><Glyph name="bot" size={13} />asked by {by}</span>{/if}
      <span class="model mono">{model} · {secs} s</span>
    </header>
    <ol class="steps" aria-live="polite">
      {#each steps as s, i (s)}
        {#if i <= at}
          <li class:done={i < at}>
            {#if i < at}<Glyph name="check" size={14} />{:else}<span class="spin" aria-hidden="true"></span>{/if}
            <span>{s}{i === at ? '…' : ''}</span>
          </li>
        {/if}
      {/each}
    </ol>
    <div class="lines" aria-hidden="true">
      <span style="width: 94%"></span>
      <span style="width: 81%"></span>
      <span style="width: 63%"></span>
    </div>
    {#if onCancel}
      <footer><button class="btn ghost small" onclick={onCancel}><Glyph name="x" size={13} />Cancel</button></footer>
    {/if}
  </article>
{/if}

<style>
  .line {
    display: flex;
    align-items: center;
    gap: 8px;
    color: var(--ai);
    font-weight: 600;
    font-size: 14px;
    min-width: 0;
  }
  .line .now {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .work {
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
    animation: pulse 1.4s ease-in-out infinite;
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
    font-size: 12px;
    color: var(--ai);
    border: 1px solid var(--ai-line);
    border-radius: 4px;
    padding: 0 6px;
    white-space: nowrap;
    font-variant-numeric: tabular-nums;
  }
  .steps {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    gap: 5px;
    font-size: 14.5px;
  }
  .steps li {
    display: flex;
    gap: 8px;
    align-items: center;
    color: var(--ink);
    animation: in 0.2s ease-out;
  }
  .steps li.done {
    color: var(--muted);
  }
  .steps li.done :global(svg) {
    color: var(--ai);
  }
  .spin {
    width: 14px;
    height: 14px;
    border-radius: 50%;
    border: 2px solid var(--ai-line);
    border-top-color: var(--ai-fill);
    animation: spin 0.8s linear infinite;
    flex: none;
  }
  .lines {
    display: grid;
    gap: 7px;
    padding-left: 22px;
  }
  .lines span {
    height: 10px;
    border-radius: 5px;
    background: linear-gradient(90deg, var(--ai-soft) 0%, color-mix(in srgb, var(--ai-line) 55%, var(--ai-soft)) 50%, var(--ai-soft) 100%);
    background-size: 200% 100%;
    animation: shimmer 1.3s linear infinite;
  }
  footer {
    display: flex;
    justify-content: flex-end;
  }
  @keyframes spin {
    to {
      transform: rotate(360deg);
    }
  }
  @keyframes shimmer {
    from {
      background-position: 100% 0;
    }
    to {
      background-position: -100% 0;
    }
  }
  @keyframes pulse {
    50% {
      box-shadow: 0 0 0 5px color-mix(in srgb, var(--ai-fill) 18%, transparent);
    }
  }
  @keyframes in {
    from {
      opacity: 0;
      transform: translateY(4px);
    }
  }
  @media (prefers-reduced-motion: reduce) {
    .spark,
    .lines span,
    .steps li {
      animation: none;
    }
    .spin {
      animation-duration: 2s;
    }
  }
</style>
