<script lang="ts">
  // One source of an LLM answer as a small link chip ("trailer c-2hd7vw").
  import type { Source } from '../../api/ftypes';
  import { href } from '../../router.svelte';
  import { sourceRoute } from '../../f/sources';

  let { source, changeId }: { source: Source; changeId?: string } = $props();

  const route = $derived(sourceRoute(source, changeId));
  const external = $derived(!!route && /^https?:/.test(route));
  const label = $derived(
    source.label.startsWith(source.kind) || ['file', 'policy', 'change'].includes(source.kind)
      ? source.label
      : `${source.kind === 'mr' ? 'MR' : source.kind} ${source.label.replace(/^(MR|trailer|lab|kb)\s+/i, '')}`,
  );
</script>

{#if route}
  <a
    class="src k-{source.kind}"
    href={external ? route : href(route)}
    target={external ? '_blank' : undefined}
    rel={external ? 'noreferrer' : undefined}
    title={source.ref ?? source.label}>{label}</a
  >
{:else}
  <span class="src k-{source.kind}" title={source.ref ?? source.label}>{label}</span>
{/if}

<style>
  .src {
    display: inline-block;
    font: 500 11.5px/1.6 var(--mono);
    padding: 0 6px;
    border-radius: 4px;
    background: var(--info-soft);
    color: var(--info);
    text-decoration: none;
    white-space: nowrap;
    max-width: 28ch;
    overflow: hidden;
    text-overflow: ellipsis;
    vertical-align: baseline;
  }
  a.src:hover {
    text-decoration: underline;
  }
  .k-policy {
    background: var(--warn-soft);
    color: var(--warn);
  }
</style>
