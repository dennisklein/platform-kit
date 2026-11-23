<script lang="ts" module>
  const TONE: Record<string, string> = {
    pass: 'ok',
    fail: 'bad',
    alert: 'bad',
    running: 'warn',
    cancelled: 'idle',
    skipped: 'idle',
    pending: 'idle',
    none: 'idle',
  };

  /** Tone of a lab or step result: ok, bad, warn or idle. */
  export function resultTone(r: string | undefined): string {
    return TONE[r ?? 'none'] ?? 'idle';
  }
</script>

<script lang="ts">
  // A lab or check result (pass, fail, running) in the same box as the
  // change state chip (.schip in app.css). A running result pulses.
  let { result, label, title }: { result: string | undefined; label?: string; title?: string } = $props();
</script>

<span class="schip tone-{resultTone(result)}" data-result={result} {title}
  >{#if result === 'running'}<span class="sdot live" aria-hidden="true"></span>{/if}{label ?? result ?? '—'}</span
>
