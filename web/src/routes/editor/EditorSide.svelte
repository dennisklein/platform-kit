<script lang="ts">
  // Right panel of the editor: kind registry, problems, secret fields and
  // suggested edits for the open file.
  import type { FileContent, Kind, Problem, Suggestion } from '../../lib/api/types';
  import Icon from '../../lib/components/Icon.svelte';
  import SuggestionCard from '../../lib/components/advisor/SuggestionCard.svelte';

  let {
    file,
    kind,
    problems,
    suggestions = $bindable([]),
    onjump,
    onsuggestion,
  }: {
    file: FileContent;
    kind?: Kind;
    problems: Problem[];
    suggestions?: Suggestion[];
    onjump: (p: Problem) => void;
    onsuggestion: (s: Suggestion) => void;
  } = $props();

  const APPLY: Record<string, string> = { flux: 'Flux', manual: 'manual', sync: 'sync job', none: 'none' };
  const sorted = $derived(
    [...problems].sort((a, b) => sev(a) - sev(b) || (a.line ?? 0) - (b.line ?? 0)),
  );
  function sev(p: Problem) {
    return p.severity === 'error' ? 0 : p.severity === 'warning' ? 1 : 2;
  }
  const editorText = $derived(
    !file.kind ? (file.language === 'yaml' ? 'YAML' : file.language) : kind?.own ? 'YAML + form + hooks' : 'YAML + CRD schema',
  );
</script>

<aside class="side">
  <section class="card">
    <h3 class="eyebrow">Kind registry</h3>
    <dl>
      <dt>kind</dt>
      <dd class="mono">{file.kind ? (kind?.own ? file.kind.name : 'none') : 'none'}</dd>
      <dt>editor</dt>
      <dd>{editorText}</dd>
      {#if file.kind}
        <dt>schema</dt>
        <dd class="mono">{file.kind.name} {file.kind.version ?? file.kind.apiVersion.split('/').pop()}</dd>
      {/if}
      <dt>apply path</dt>
      <dd>{APPLY[file.applyPath ?? kind?.applyPath ?? 'none'] ?? file.applyPath}</dd>
      {#if file.truthClass}
        <dt>truth</dt>
        <dd>{file.truthClass}</dd>
      {/if}
      {#if file.convertedFrom}
        <dt>stored as</dt>
        <dd class="mono warn-text" title="Converted for the editor; saving writes the newest version">{file.convertedFrom}</dd>
      {/if}
    </dl>
    {#if kind?.views?.length}
      <p class="muted small">Views: {kind.views.join(', ')}</p>
    {/if}
  </section>

  <section class="card">
    <h3 class="eyebrow">Problems {#if problems.length}<span class="n">{problems.length}</span>{/if}</h3>
    {#if sorted.length === 0}
      <p class="ok-line"><Icon name="check" size={15} /> No problems</p>
    {:else}
      <ul class="probs">
        {#each sorted as p, i (i)}
          <li>
            <button class="prob s-{p.severity}" onclick={() => onjump(p)} title="{p.source}{p.code ? ` · ${p.code}` : ''}">
              <span class="ic"><Icon name={p.severity === 'error' ? 'x' : p.severity === 'warning' ? 'warning' : 'info'} size={14} /></span>
              <span>
                {#if p.line}line {p.line}:{' '}{/if}{#if p.field && !p.message.includes(p.field)}<code>{p.field}</code>{' '}{/if}{p.message}
              </span>
            </button>
          </li>
        {/each}
      </ul>
    {/if}
  </section>

  {#if file.secrets.length}
    <section class="card">
      <h3 class="eyebrow">Secret fields</h3>
      {#each file.secrets as s (s.field)}
        <div class="secret">
          <dl>
            <dt><Icon name="lock" size={13} /> <span class="mono">{s.field.replace(/^spec\.values\./, '')}</span></dt>
            <dd>{s.kind === 'sops' ? 'SOPS' : s.kind === 'openbao' ? 'OpenBao' : 'schema'}</dd>
            {#if s.key}<dt>key</dt><dd title={s.key}>{s.key.replace(/\s*\(.*\)$/, '')}</dd>{/if}
          </dl>
        </div>
      {/each}
      <p class="muted small">Values stay masked. Saving keeps the stored ciphertext.</p>
    </section>
  {/if}

  {#if suggestions.length}
    <section class="card">
      <h3 class="eyebrow">Suggested edits</h3>
      {#each suggestions as s, i (s.id)}
        <SuggestionCard bind:suggestion={suggestions[i]} onDecided={onsuggestion} note={i === suggestions.length - 1} />
      {/each}
    </section>
  {/if}
</aside>

<style>
  .side {
    display: grid;
    gap: 14px;
    align-content: start;
    padding: 16px;
    min-width: 0;
  }
  .card {
    padding: 14px 18px;
  }
  .eyebrow {
    margin: 0 0 10px;
    display: flex;
    gap: 6px;
    align-items: center;
  }
  .n {
    background: var(--bad);
    color: #fff;
    border-radius: 8px;
    padding: 0 6px;
    font-size: 10.5px;
    letter-spacing: 0;
  }
  dl {
    display: grid;
    grid-template-columns: auto minmax(0, 1fr);
    gap: 7px 12px;
    margin: 0;
    font-size: 14px;
  }
  dt {
    color: var(--muted);
    display: flex;
    gap: 4px;
    align-items: center;
    min-width: 0;
  }
  dd {
    margin: 0;
    text-align: right;
    overflow-wrap: anywhere;
  }
  .secret + .secret {
    margin-top: 8px;
    padding-top: 8px;
    border-top: 1px solid var(--line);
  }
  .probs {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    gap: 4px;
  }
  .prob {
    display: flex;
    gap: 8px;
    align-items: flex-start;
    width: 100%;
    text-align: left;
    border: 0;
    background: none;
    color: var(--ink);
    padding: 4px 6px;
    margin: 0 -6px;
    border-radius: 4px;
    font: 14px/1.4 var(--sans);
  }
  .prob:hover {
    background: var(--bg);
  }
  .prob code {
    font-size: 13px;
  }
  .ic {
    padding-top: 2px;
    flex: none;
  }
  .s-error .ic {
    color: var(--bad);
  }
  .s-warning .ic {
    color: var(--warn);
  }
  .s-info .ic {
    color: var(--info);
  }
  .ok-line {
    margin: 0;
    display: flex;
    gap: 6px;
    align-items: center;
    color: var(--ok);
    font-size: 14px;
  }
  .small {
    margin: 10px 0 0;
  }
</style>
