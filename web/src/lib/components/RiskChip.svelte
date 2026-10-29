<script lang="ts">
  // The risk of a change from .pk/policy.yaml, in the same box as the state
  // chip (.schip in app.css), so the two sit side by side at one height.
  let { level, prefix = true }: { level: string; prefix?: boolean } = $props();
  const TONE: Record<string, string> = { high: 'bad', medium: 'warn', low: 'ok' };
</script>

<span class="schip tone-{TONE[level] ?? 'idle'}" data-risk={level}>{prefix ? `risk: ${level}` : level}</span>
