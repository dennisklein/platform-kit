<script lang="ts">
  // Button that copies a text and says "Copied" for a moment.
  import { copyText } from '../f/copy';
  import Glyph from './Glyph.svelte';

  let { text, label = 'Copy', dark = false }: { text: string; label?: string; dark?: boolean } = $props();

  let state = $state<'idle' | 'ok' | 'fail'>('idle');
  let t: ReturnType<typeof setTimeout> | undefined;

  async function copy() {
    state = (await copyText(text)) ? 'ok' : 'fail';
    clearTimeout(t);
    t = setTimeout(() => (state = 'idle'), 1800);
  }
</script>

<button type="button" class="btn small cp" class:dark class:ok={state === 'ok'} onclick={copy} aria-live="polite">
  <Glyph name={state === 'ok' ? 'check' : 'copy'} size={12} />
  {state === 'ok' ? 'Copied' : state === 'fail' ? 'Select and copy' : label}
</button>

<style>
  .cp {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    white-space: nowrap;
  }
  .cp.dark {
    background: #2a2f39;
    border-color: #454b58;
    color: #e7e9ee;
  }
  .cp.dark:hover {
    background: #343a46 !important;
  }
  .cp.ok {
    color: var(--ok);
    border-color: color-mix(in srgb, var(--ok) 50%, transparent);
  }
  .cp.dark.ok {
    color: #8bc97a;
  }
</style>
