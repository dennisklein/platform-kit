<script lang="ts">
  // The chat conversation: questions as dennis, answers with the steps the
  // bench took (live while the answer forms), sources, suggestion cards
  // (diff, Accept/Reject), refusals for secrets, thumbs, copy and
  // regenerate.
  import { tick } from 'svelte';
  import { chat, type ChatEntry } from '../../stores/chat.svelte';
  import { session } from '../../stores/me.svelte';
  import { changes } from '../../stores/changes.svelte';
  import { href } from '../../router.svelte';
  import type { Suggestion } from '../../api/ftypes';
  import SourceChip from '../advisor/SourceChip.svelte';
  import SuggestionCard from '../advisor/SuggestionCard.svelte';
  import Thumbs from '../advisor/Thumbs.svelte';
  import Markdown from '../Markdown.svelte';
  import Glyph from '../Glyph.svelte';
  import { modelLabel, type AnswerLike } from '../../f/llm';
  import { copyText } from '../../f/copy';
  import { closeMarkdown, elapsed, footLabel, offerData, usedTools, type LiveStep } from './live';

  let { wide = false }: { wide?: boolean } = $props();

  // "from project data" when the bench data answered instead of the model;
  // "built-in · secret guard" for a refusal; short in the side panel.
  function answerModel(a: AnswerLike & { refused?: boolean }) {
    const llm = session.status?.llm;
    return footLabel(modelLabel(a, !!llm && !llm.mock && !llm.reachable), a, !wide);
  }

  let text = $state('');
  let list: HTMLDivElement | undefined = $state();
  let flow: HTMLDivElement | undefined = $state();
  let input: HTMLTextAreaElement | undefined = $state();
  /** Answers whose steps are open: the newest one of this visit, and the ones the user opened. */
  let opened = $state<Record<number, boolean>>({});
  /** Answers that just arrived: their sources and cards fade in. */
  let fresh = $state<Record<number, boolean>>({});
  let copied = $state<number | null>(null);
  let now = $state(Date.now());

  const ctx = $derived(chat.changeId);
  // The three questions of the demo script (docs/demo.html, step 3.3).
  const examples = ['Why does cn-003 use port 37 on data-a?', 'What is the hubble UI token?', 'Add cn-037 to the rack-6z BGP policy'];

  // "qwen2.5:7b" or "built-in" for the thinking line.
  const modelName = $derived.by(() => {
    const llm = session.status?.llm;
    if (!llm) return '';
    return llm.mock ? 'built-in' : llm.model;
  });

  // A model is set (not built-in): a slow answer offers the data answer.
  const usesModel = $derived(!!session.status?.llm && !session.status.llm.mock);

  function send(t = text) {
    if (!t.trim() || chat.busy) return;
    if (chat.send(t)) text = '';
    stick = true;
    void tick().then(() => scroll(true));
  }

  function nearBottom() {
    return !list || list.scrollHeight - list.scrollTop - list.clientHeight < 140;
  }

  function scroll(force = false) {
    if (list && (force || nearBottom())) list.scrollTop = list.scrollHeight;
  }

  // Follow the answer to its end while it grows (steps, text, then the
  // sources and the suggestion card with Accept), unless the user scrolled
  // up. The observer sees every growth, also the staged reveal.
  let stick = true;
  function onscroll() {
    stick = nearBottom();
  }
  $effect(() => {
    if (!flow || typeof ResizeObserver === 'undefined') return;
    const ro = new ResizeObserver(() => {
      if (stick) scroll(true);
    });
    ro.observe(flow);
    return () => ro.disconnect();
  });

  // The clock of the live answer.
  $effect(() => {
    if (!chat.busy) return;
    now = Date.now();
    const t = setInterval(() => (now = Date.now()), 500);
    return () => clearInterval(t);
  });

  // A new answer: its steps stay open and its extras fade in; older ones close.
  let liveId = 0;
  $effect(() => {
    const last = chat.entries[chat.entries.length - 1];
    if (last?.live && last.id !== liveId) {
      liveId = last.id;
      opened = {};
      fresh = {};
    } else if (last && !last.live && last.id === liveId && !fresh[last.id]) {
      fresh = { [last.id]: true };
      opened = { [last.id]: true };
    }
  });

  function onkeydown(e: KeyboardEvent) {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      // While an answer forms, the draft stays in the box.
      if (!chat.busy) send();
    } else if (e.key === 'Escape' && chat.busy) {
      e.preventDefault();
      chat.stop();
    }
  }

  function decided(e: ChatEntry, i: number, s: Suggestion) {
    if (!e.answer) return;
    const suggestions = e.answer.suggestions.map((x, j) => (j === i ? s : x));
    chat.update(e.id, { ...e.answer, suggestions });
  }

  async function copy(e: ChatEntry) {
    if (await copyText(e.answer?.text ?? e.content)) {
      copied = e.id;
      setTimeout(() => (copied = copied === e.id ? null : copied), 1600);
    }
  }

  function regenerate() {
    stick = true;
    if (chat.regenerate()) void tick().then(() => scroll(true));
  }

  function liveStatus(steps: LiveStep[], streaming: boolean, startedAt: number): string {
    const t = elapsed(startedAt, now);
    if (streaming) return `Writing · ${t}`;
    if (steps.some((s) => s.state === 'running')) return `Working · ${t}`;
    return modelName ? `Thinking · ${modelName} · ${t}` : `Thinking · ${t}`;
  }

  // "the model took longer than 25 s" → "model took longer than 25 s".
  const reason = (r?: string) => (r ?? '').replace(/^the /, '');

  const open = $derived(changes.list.filter((c) => !['merged', 'deployed', 'verified', 'closed'].includes(c.state)));
  const ctxTitle = $derived(changes.byId(ctx)?.title);

  function pick(e: Event) {
    const v = (e.target as HTMLSelectElement).value;
    chat.picked = v === '__none' ? null : v === '__auto' ? undefined : v;
  }
</script>

{#snippet stepList(steps: LiveStep[], live: boolean)}
  <ol class="steps" class:live aria-label="Steps">
    {#each steps as s (s.id)}
      <li class="step {s.state}" title={s.ms ? `${s.ms} ms` : undefined}>
        <span class="st" aria-hidden="true">
          {#if s.state === 'running'}<span class="spin"></span>{:else if s.state === 'failed'}<Glyph name="x" size={11} />{:else}<Glyph name="check" size={11} />{/if}
        </span>
        <span class="line"><span class="lbl">{s.label}</span>{#if s.summary}<span class="sum">→ {s.summary}</span>{/if}</span>
        <span class="sr-only">{s.state === 'running' ? 'running' : s.state === 'failed' ? 'failed' : 'done'}</span>
      </li>
    {/each}
  </ol>
{/snippet}

<div class="thread" class:wide>
  <div class="ctx">
    <Glyph name="branch" size={13} />
    <select class="ctxsel" onchange={pick} aria-label="Change for suggestions" title={ctxTitle ?? 'No change: answers only, no suggested edits'}>
      <option value="__auto" selected={chat.picked === undefined}>{ctx && chat.picked === undefined ? `${ctx} (current)` : 'current change'}</option>
      {#each open as c (c.id)}
        <option value={c.id} selected={chat.picked === c.id}>{c.id} · {c.title}</option>
      {/each}
      <option value="__none" selected={chat.picked === null}>no change</option>
    </select>
    <span class="spacer"></span>
    {#if chat.entries.length}
      <button class="btn ghost small" onclick={() => chat.clear()} title="Clear the conversation">Clear</button>
    {/if}
  </div>

  <div class="msgs" class:empty={!chat.entries.length} bind:this={list} {onscroll}>
    <div class="flow" bind:this={flow}>
    {#each chat.entries as e (e.id)}
      {#if e.role === 'user'}
        <div class="q">{e.content}</div>
      {:else}
        <div class="arow">
          <span class="avatar" aria-hidden="true"><Glyph name="sparkle" size={15} /></span>
          {#if e.error}
            <div class="a err" role="alert"><Glyph name="alert" size={14} /> {e.error}</div>
          {:else if e.live}
            {@const l = e.live}
            <div class="a live" aria-busy="true">
              <div class="who">
                <span class="name">pk assistant</span>
                <span class="status" aria-live="polite">{liveStatus(l.steps, !!l.text, l.startedAt)}</span>
              </div>
              {#if l.steps.length}{@render stepList(l.steps, true)}{/if}
              {#if l.text}
                <div class="txt streaming"><Markdown source={closeMarkdown(l.text)} /></div>
              {:else if !l.steps.length}
                <div class="dots" aria-hidden="true"><span></span><span></span><span></span></div>
              {/if}
              {#if offerData(l, now, usesModel)}
                <div class="offer">
                  <button class="data" onclick={() => chat.answerFromData()} title="Stop waiting for the model and answer from the project data">
                    <Glyph name="zap" size={12} /><span>Answer from project data now</span>
                  </button>
                </div>
              {/if}
            </div>
          {:else if e.stopped}
            {@const steps = e.steps ?? []}
            <div class="a stopped">
              <div class="who">
                <span class="name">pk assistant</span>
                {#if steps.length}
                  <button
                    class="used"
                    aria-expanded={!!opened[e.id]}
                    onclick={() => (opened = { ...opened, [e.id]: !opened[e.id] })}
                    title={opened[e.id] ? 'Hide the steps' : 'Show the steps'}
                  >
                    {usedTools(steps.length)}
                    <span class="chev" class:up={opened[e.id]}><Glyph name="chevron" size={11} /></span>
                  </button>
                {/if}
              </div>
              {#if steps.length && opened[e.id]}{@render stepList(steps, false)}{/if}
              {#if e.content}<div class="txt"><Markdown source={closeMarkdown(e.content)} /></div>{/if}
              <div class="foot">
                <span class="muted small">Stopped.</span>
                {#if chat.lastAnswerId === e.id}
                  <button class="icon" onclick={regenerate} disabled={chat.busy} title="Ask again"><Glyph name="refresh" size={13} /><span>Ask again</span></button>
                {/if}
              </div>
            </div>
          {:else if e.answer}
            {@const a = e.answer}
            {@const m = answerModel(a)}
            {@const steps = (a.steps ?? []) as LiveStep[]}
            <div class="a" class:refused={a.refused} class:fresh={fresh[e.id]}>
              <div class="who">
                <span class="name">pk assistant</span>
                {#if steps.length}
                  <button
                    class="used"
                    aria-expanded={!!opened[e.id]}
                    onclick={() => (opened = { ...opened, [e.id]: !opened[e.id] })}
                    title={opened[e.id] ? 'Hide the steps' : 'Show the steps'}
                  >
                    {usedTools(steps.length)}
                    <span class="chev" class:up={opened[e.id]}><Glyph name="chevron" size={11} /></span>
                  </button>
                {/if}
              </div>
              {#if steps.length && opened[e.id]}{@render stepList(steps, false)}{/if}
              <div class="txt">
                {#if a.refused}<span class="lock"><Glyph name="lock" size={14} /></span>{/if}
                <Markdown source={a.text} />
              </div>
              {#if a.sources.length}
                <div class="srcs extra">
                  {#each a.sources as s, i (i)}<SourceChip source={s} changeId={e.changeId} />{/each}
                </div>
              {/if}
              {#each a.suggestions as s, i (s.id)}
                <div class="sg extra">
                  <SuggestionCard suggestion={s} onDecided={(x) => decided(e, i, x)} />
                </div>
              {/each}
              <div class="foot extra">
                <Thumbs logId={a.logId} compact note={false} />
                <span class="model mono" class:warn-text={m.fallback && !a.fallbackReason} title={m.title}>{m.text}</span>
                {#if a.fallbackReason}
                  <span class="why" title="The bench answered from its own data instead"><Glyph name={a.fallbackReason.startsWith('you asked') ? 'zap' : 'clock'} size={11} /> {reason(a.fallbackReason)}</span>
                {/if}
                <span class="spacer"></span>
                <button class="icon" onclick={() => copy(e)} title="Copy the answer" aria-live="polite">
                  <Glyph name={copied === e.id ? 'check' : 'copy'} size={13} /><span class="sr-only">{copied === e.id ? 'Copied' : 'Copy'}</span>
                </button>
                {#if chat.lastAnswerId === e.id}
                  <button class="icon" onclick={regenerate} disabled={chat.busy} title="Ask again">
                    <Glyph name="refresh" size={13} /><span class="sr-only">Ask again</span>
                  </button>
                {/if}
                {#if a.logId}<a class="log" href={href('/llm-log', { q: a.logId })} title="Open in the LLM log">log</a>{/if}
              </div>
            </div>
          {/if}
        </div>
      {/if}
    {:else}
      <div class="intro">
        <span class="badge" aria-hidden="true"><Glyph name="sparkle" size={22} /></span>
        <h2>Ask about this project</h2>
        <p>
          The chat reads the state repo, the KB, merge trailers, MR discussions and lab reports, and shows its sources. Edits come
          back as suggestions in your change; you accept them.
        </p>
        <div class="ex">
          {#each examples as x (x)}
            <button class="chip" onclick={() => send(x)}>
              <Glyph name="sparkle" size={12} /><span>{x}</span><span class="go"><Glyph name="arrow-right" size={12} /></span>
            </button>
          {/each}
        </div>
      </div>
    {/each}
    </div>
  </div>

  <form
    class="ask"
    class:busy={chat.busy}
    onsubmit={(e) => {
      e.preventDefault();
      send();
    }}
  >
    <textarea
      bind:this={input}
      bind:value={text}
      {onkeydown}
      rows={wide ? 2 : 1}
      placeholder={chat.busy ? 'Type your next question…' : 'Ask about the project…'}
      aria-label="Message"
    ></textarea>
    {#if chat.busy}
      <button type="button" class="btn small stop" onclick={() => chat.stop()} title="Stop the answer (Esc)">
        <svg width="10" height="10" viewBox="0 0 10 10" aria-hidden="true"><rect width="10" height="10" rx="2" fill="currentColor" /></svg>
        <span>Stop</span>
      </button>
    {:else}
      <button class="btn small send" disabled={!text.trim()} title="Send (Enter)">
        <Glyph name="send" size={13} /><span class="sr-only">Send</span>
      </button>
    {/if}
  </form>
  <p class="who-line small muted">as {session.me?.id ?? '…'} · secret fields are removed before each call</p>
</div>

<style>
  .thread {
    display: grid;
    grid-template-rows: auto minmax(0, 1fr) auto auto;
    height: 100%;
    min-height: 0;
    gap: 8px;
  }
  .ctx {
    display: flex;
    align-items: center;
    gap: 6px;
    color: var(--accent);
    min-width: 0;
  }
  .ctxsel {
    border: 0;
    background: transparent;
    font: 500 12px var(--mono);
    color: var(--ink);
    max-width: 240px;
    padding: 2px 0;
  }
  .wide .ctxsel {
    font-size: 13px;
    max-width: 420px;
  }
  .msgs {
    position: relative;
    overflow-y: auto;
    display: flex;
    flex-direction: column;
    padding: 2px 2px 8px;
    min-height: 0;
  }
  /* The content of the thread; its growth drives the auto-scroll. */
  .flow {
    flex: none;
    display: flex;
    flex-direction: column;
    gap: 12px;
  }
  .wide .flow {
    gap: 16px;
  }
  .wide .msgs.empty {
    justify-content: center;
  }
  .q {
    align-self: flex-end;
    max-width: 88%;
    background: var(--gsi-navy);
    color: #fff;
    padding: 8px 12px;
    border-radius: 12px 12px 3px 12px;
    font-size: 14px;
    white-space: pre-wrap;
  }
  .wide .q {
    max-width: 72%;
    font-size: 15px;
    padding: 9px 14px;
  }

  /* Assistant: sparkle avatar, AI-teal frame. */
  .arow {
    display: flex;
    gap: 8px;
    align-items: flex-start;
    min-width: 0;
  }
  .avatar {
    flex: none;
    width: 26px;
    height: 26px;
    border-radius: 50%;
    display: grid;
    place-items: center;
    background: var(--ai-fill);
    color: var(--ai-on);
    margin-top: 2px;
  }
  .a {
    flex: 0 1 auto;
    max-width: calc(100% - 34px);
    background: var(--panel);
    border: 1px solid var(--ai-line);
    border-radius: 3px 12px 12px 12px;
    padding: 8px 12px 9px;
    display: grid;
    gap: 7px;
    font-size: 14px;
    min-width: 0;
    box-shadow: 0 1px 2px rgb(0 0 0 / 0.04);
  }
  .wide .a {
    max-width: min(86%, 780px);
    padding: 10px 16px 11px;
  }
  .a.live {
    min-width: min(320px, calc(100% - 34px));
    border-style: dashed;
  }
  .who {
    display: flex;
    align-items: center;
    gap: 8px;
    font-size: 12px;
    min-height: 18px;
  }
  .name {
    font-weight: 700;
    color: var(--ai);
    letter-spacing: 0.01em;
  }
  .status {
    color: var(--muted);
    font-variant-numeric: tabular-nums;
  }
  .used {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    border: 1px solid var(--ai-line);
    background: var(--ai-soft);
    color: var(--ai);
    border-radius: 999px;
    padding: 0 8px;
    font: 600 11.5px/18px var(--sans);
  }
  .used:hover {
    border-color: var(--ai);
  }
  .chev {
    display: inline-flex;
    transform: rotate(0deg);
    transition: transform 0.15s;
  }
  .chev.up {
    transform: rotate(180deg);
  }

  /* Steps: mono chips with a spinner, then a check. */
  .steps {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    gap: 4px;
  }
  .step {
    display: flex;
    align-items: center;
    gap: 6px;
    font: 12.5px/1.45 var(--mono);
    font-variant-ligatures: none;
    color: var(--ink);
    min-width: 0;
  }
  .steps.live .step {
    animation: rise 0.22s ease-out both;
  }
  .wide .step {
    font-size: 13px;
  }
  .st {
    flex: none;
    width: 18px;
    height: 18px;
    border-radius: 50%;
    display: grid;
    place-items: center;
    background: var(--ai-soft);
    color: var(--ai);
  }
  .step.failed .st {
    background: var(--warn-soft);
    color: var(--warn);
  }
  .step.running .lbl {
    color: var(--ai);
  }
  .lbl {
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    min-width: 0;
  }
  .sum {
    color: var(--muted);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    min-width: 0;
    flex: 0 1 auto;
  }
  .line {
    display: flex;
    gap: 6px;
    min-width: 0;
  }
  /* The narrow side panel: a step wraps as text instead of being cut. */
  .thread:not(.wide) .step {
    align-items: flex-start;
  }
  .thread:not(.wide) .line {
    display: block;
    overflow-wrap: anywhere;
  }
  .thread:not(.wide) .line .lbl,
  .thread:not(.wide) .line .sum {
    white-space: normal;
    overflow: visible;
  }
  .thread:not(.wide) .line .sum {
    margin-left: 6px;
  }
  .spin {
    width: 10px;
    height: 10px;
    border-radius: 50%;
    border: 2px solid color-mix(in srgb, var(--ai) 30%, transparent);
    border-top-color: var(--ai);
    animation: spin 0.8s linear infinite;
  }

  .txt {
    min-width: 0;
  }
  .txt :global(.md) {
    font-size: 14px;
    line-height: 1.55;
    max-width: none;
  }
  .wide .txt :global(.md) {
    font-size: 15px;
  }
  .txt :global(.md > :last-child) {
    margin-bottom: 0;
  }
  .txt :global(.md ul),
  .txt :global(.md ol) {
    padding-left: 20px;
  }
  .txt :global(.md table) {
    display: block;
    overflow-x: auto;
    max-width: 100%;
  }
  .txt :global(.md blockquote) {
    border-left-color: var(--ai-line);
  }
  /* A caret at the end of the text while it streams. */
  .txt.streaming :global(.md > :last-child)::after {
    content: '';
    display: inline-block;
    width: 0.45em;
    height: 1.05em;
    margin-left: 2px;
    vertical-align: -0.15em;
    background: var(--ai);
    border-radius: 1px;
    animation: caret 1s steps(2, start) infinite;
  }
  .a.refused {
    border-color: color-mix(in srgb, var(--warn) 45%, transparent);
    background: var(--warn-soft);
  }
  .a.refused .txt {
    display: flex;
    gap: 8px;
    align-items: flex-start;
  }
  .lock {
    color: var(--warn);
    padding-top: 3px;
  }
  .a.err {
    background: var(--bad-soft);
    color: var(--bad);
    border-color: transparent;
    display: flex;
    gap: 6px;
    align-items: center;
  }
  /* "Ask from project data now": a slow model need not hold the stage. */
  .offer {
    display: flex;
  }
  .data {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    border: 1px solid var(--ai-line);
    background: var(--ai-soft);
    color: var(--ai);
    border-radius: 999px;
    padding: 3px 11px 3px 9px;
    font: 600 12.5px/18px var(--sans);
    animation: rise 0.25s ease-out both;
  }
  .data:hover {
    border-color: var(--ai);
    background: var(--panel);
  }
  .srcs {
    display: flex;
    flex-wrap: wrap;
    gap: 4px;
  }
  .sg {
    border-top: 1px dashed var(--line);
    padding-top: 8px;
  }
  .foot {
    display: flex;
    align-items: center;
    gap: 6px;
    margin-top: -1px;
    min-width: 0;
  }
  /* The model and why labels; in the side panel the footer wraps instead
     of cutting them. */
  .model,
  .why {
    font-size: 11px;
    color: var(--muted);
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .thread:not(.wide) .foot {
    flex-wrap: wrap;
    row-gap: 2px;
  }
  .thread:not(.wide) .why {
    white-space: normal;
  }
  .why {
    display: inline-flex;
    align-items: center;
    gap: 3px;
  }
  .foot .icon,
  .foot .log {
    flex: none;
  }
  .icon {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    border: 0;
    background: transparent;
    color: var(--muted);
    padding: 3px 5px;
    border-radius: 5px;
    font-size: 12px;
  }
  .icon:hover:not(:disabled) {
    background: var(--ai-soft);
    color: var(--ai);
  }
  .icon:disabled {
    opacity: 0.5;
  }
  .log {
    font-size: 11px;
    color: var(--muted);
  }
  .fresh .extra {
    animation: rise 0.35s ease-out both;
  }
  .fresh .srcs.extra {
    animation-delay: 0.05s;
  }
  .fresh .sg.extra {
    animation-delay: 0.15s;
  }
  .fresh .foot.extra {
    animation-delay: 0.25s;
  }
  .dots {
    display: flex;
    gap: 4px;
    padding: 4px 0 2px;
  }
  .dots span {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: var(--ai);
    animation: blink 1.2s infinite;
  }
  .dots span:nth-child(2) {
    animation-delay: 0.2s;
  }
  .dots span:nth-child(3) {
    animation-delay: 0.4s;
  }

  /* Empty state: what the chat does, and example questions as chips. */
  .intro {
    color: var(--muted);
    font-size: 13px;
    display: grid;
    gap: 10px;
    justify-items: start;
    text-align: left;
  }
  .wide .intro {
    justify-items: center;
    text-align: center;
    max-width: 640px;
    margin: 0 auto;
    font-size: 14.5px;
    gap: 12px;
    padding-bottom: 6vh;
  }
  .badge {
    width: 34px;
    height: 34px;
    border-radius: 50%;
    display: grid;
    place-items: center;
    background: var(--ai-soft);
    color: var(--ai);
    border: 1px solid var(--ai-line);
  }
  .wide .badge {
    width: 48px;
    height: 48px;
  }
  .intro h2 {
    margin: 0;
    font-size: 15px;
    color: var(--ink);
  }
  .wide .intro h2 {
    font-size: 21px;
  }
  .intro p {
    margin: 0;
    line-height: 1.5;
  }
  .ex {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
  }
  .wide .ex {
    justify-content: center;
    gap: 8px;
    margin-top: 4px;
  }
  .chip {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    border: 1px solid var(--ai-line);
    background: var(--ai-soft);
    color: var(--ink);
    border-radius: 999px;
    padding: 5px 8px 5px 10px;
    font-size: 13px;
    text-align: left;
    max-width: 100%;
  }
  .chip > :global(svg:first-child) {
    color: var(--ai);
    flex: none;
  }
  .chip span:not(.go) {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .thread:not(.wide) .chip span:not(.go) {
    white-space: normal;
  }
  .wide .chip {
    font-size: 14px;
    padding: 7px 10px 7px 13px;
  }
  .chip .go {
    display: inline-flex;
    color: var(--ai);
    opacity: 0;
    transform: translateX(-3px);
    transition:
      opacity 0.15s,
      transform 0.15s;
  }
  .chip:hover {
    border-color: var(--ai);
    background: var(--panel);
  }
  .chip:hover .go,
  .chip:focus-visible .go {
    opacity: 1;
    transform: none;
  }

  /* Composer. */
  .ask {
    display: flex;
    gap: 6px;
    align-items: flex-end;
    border: 1px solid var(--line-strong);
    border-radius: 18px;
    padding: 4px 4px 4px 14px;
    background: var(--panel);
  }
  .ask:focus-within {
    border-color: var(--ai);
    box-shadow: 0 0 0 3px color-mix(in srgb, var(--ai) 15%, transparent);
  }
  textarea {
    flex: 1;
    border: 0;
    outline: none;
    resize: none;
    background: transparent;
    font-size: 14px;
    padding: 5px 0;
    max-height: 120px;
  }
  .wide textarea {
    font-size: 15px;
  }
  .ask .btn {
    border-radius: 14px;
    padding: 5px 10px;
  }
  .send {
    background: var(--ai-fill);
    border-color: var(--ai-fill);
    color: var(--ai-on);
  }
  .send:hover:not(:disabled) {
    background: var(--ai) !important;
    border-color: var(--ai) !important;
    color: var(--ai-on);
  }
  .stop {
    font-weight: 600;
    color: var(--ink);
  }
  .who-line {
    margin: -4px 0 0;
    text-align: center;
  }

  @keyframes spin {
    to {
      transform: rotate(360deg);
    }
  }
  @keyframes rise {
    from {
      opacity: 0;
      transform: translateY(4px);
    }
    to {
      opacity: 1;
      transform: none;
    }
  }
  @keyframes caret {
    to {
      visibility: hidden;
    }
  }
  @keyframes blink {
    0%,
    80%,
    100% {
      opacity: 0.25;
    }
    40% {
      opacity: 1;
    }
  }
  @media (prefers-reduced-motion: reduce) {
    .spin,
    .dots span,
    .txt.streaming :global(.md > :last-child)::after {
      animation: none;
    }
    .spin {
      border-color: var(--ai);
    }
    .steps.live .step,
    .fresh .extra,
    .data {
      animation: none;
    }
    .chev,
    .chip .go {
      transition: none;
    }
  }
</style>
