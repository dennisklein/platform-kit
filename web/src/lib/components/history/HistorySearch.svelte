<script lang="ts">
  // Search box of the history browser. Filters are typed as "entity:cn-003",
  // "path:control/**", "author:jonas" or "kind:Node" and become chips; the
  // rest is free text over titles, intents and trailers.
  import { FILTER_KEYS, parseQuery, type HistoryQuery, type FilterKey } from '../../f/query';
  import Glyph from '../Glyph.svelte';

  let { value, onsearch }: { value: HistoryQuery; onsearch: (q: HistoryQuery) => void } = $props();

  let chips = $state<Partial<Record<FilterKey, string>>>({});
  let text = $state('');
  let input: HTMLInputElement | undefined = $state();

  // Follow the URL (back/forward, links from other pages).
  $effect.pre(() => {
    const c: Partial<Record<FilterKey, string>> = {};
    for (const k of FILTER_KEYS) if (value[k]) c[k] = value[k];
    chips = c;
    text = value.q ?? '';
  });

  function emit() {
    const parsed = parseQuery(text);
    const next: HistoryQuery = { ...chips };
    for (const k of FILTER_KEYS) if (parsed[k]) next[k] = parsed[k];
    if (parsed.q) next.q = parsed.q;
    onsearch(next);
  }

  let timer: ReturnType<typeof setTimeout> | undefined;
  function oninput() {
    // "key:value " turns into a chip as soon as it is complete.
    const m = /(^|\s)(\w+):(\S+)\s$/.exec(text);
    if (m && (FILTER_KEYS as readonly string[]).includes(m[2].toLowerCase())) {
      chips = { ...chips, [m[2].toLowerCase()]: m[3] };
      text = text.slice(0, m.index + m[1].length);
    }
    clearTimeout(timer);
    timer = setTimeout(emit, 350);
  }

  function onkeydown(e: KeyboardEvent) {
    if (e.key === 'Enter') {
      clearTimeout(timer);
      emit();
    } else if (e.key === 'Backspace' && !text) {
      const keys = FILTER_KEYS.filter((k) => chips[k]);
      const last = keys[keys.length - 1];
      if (last) {
        e.preventDefault();
        text = `${last}:${chips[last]}`;
        const { [last]: _, ...rest } = chips;
        chips = rest;
      }
    }
  }

  function remove(k: FilterKey) {
    const { [k]: _, ...rest } = chips;
    chips = rest;
    emit();
  }

  function addKey(k: FilterKey) {
    text = `${text ? text.trimEnd() + ' ' : ''}${k}:`;
    input?.focus();
  }

  const active = $derived(FILTER_KEYS.filter((k) => chips[k]));
</script>

<div class="hs">
  <div class="box" role="search">
    <Glyph name="search" size={16} />
    {#each active as k (k)}
      <span class="chip">
        <span class="k">{k}:</span> <span class="v">{chips[k]}</span>
        <button type="button" onclick={() => remove(k)} aria-label="Remove {k} filter"><Glyph name="x" size={11} /></button>
      </span>
    {/each}
    <input
      bind:this={input}
      bind:value={text}
      {oninput}
      {onkeydown}
      placeholder={active.length ? 'more words…' : 'Search: entity:cn-003  author:jonas  path:control/**  kind:Node  or words'}
      aria-label="Search history"
      spellcheck="false"
    />
  </div>
  <div class="keys">
    {#each FILTER_KEYS.filter((k) => !chips[k]) as k (k)}
      <button type="button" class="key" onclick={() => addKey(k)}>{k}:</button>
    {/each}
  </div>
</div>

<style>
  .hs {
    display: grid;
    gap: 4px;
    flex: 1;
    min-width: 0;
  }
  .box {
    display: flex;
    align-items: center;
    gap: 8px;
    border: 1px solid var(--line-strong);
    border-radius: 7px;
    padding: 5px 10px;
    background: var(--panel);
    color: var(--muted);
    min-width: 0;
  }
  .box:focus-within {
    border-color: var(--accent-fill);
    box-shadow: 0 0 0 2px color-mix(in srgb, var(--accent-fill) 35%, transparent);
  }
  input {
    flex: 1;
    min-width: 120px;
    border: 0;
    outline: none;
    background: transparent;
    padding: 3px 0;
    font-size: 14px;
    color: var(--ink);
  }
  .chip {
    display: inline-flex;
    align-items: center;
    gap: 2px;
    font: 600 13px/1.6 var(--mono);
    padding: 0 4px 0 8px;
    border-radius: 5px;
    background: var(--accent-soft);
    color: var(--accent);
    white-space: nowrap;
  }
  .chip .k {
    opacity: 0.8;
  }
  .chip button {
    display: inline-grid;
    place-items: center;
    border: 0;
    background: none;
    color: inherit;
    padding: 2px;
    margin-left: 2px;
    border-radius: 3px;
  }
  .chip button:hover {
    background: color-mix(in srgb, var(--accent) 15%, transparent);
  }
  .keys {
    display: flex;
    gap: 4px;
    padding-left: 2px;
  }
  .key {
    border: 0;
    background: none;
    font: 12px var(--mono);
    color: var(--muted);
    padding: 0 4px;
  }
  .key:hover {
    color: var(--accent);
  }
</style>
