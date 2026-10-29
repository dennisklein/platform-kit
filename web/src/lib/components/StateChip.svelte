<script lang="ts" module>
  import type { ChangeState, LabState } from '../api/types';

  export type ChipKind = ChangeState | 'labfail' | 'labrun';

  /** Chip kind of a change: a failed or running lab run shows on draft and lab. */
  export function chipKind(state: ChangeState, labState?: LabState): ChipKind {
    if (state === 'draft' || state === 'lab') {
      if (labState === 'fail') return 'labfail';
      if (labState === 'running') return 'labrun';
    }
    return state;
  }

  const LABELS: Record<ChipKind, string> = {
    draft: 'draft',
    lab: 'lab',
    labfail: 'lab failed',
    labrun: 'lab running',
    review: 'review',
    approved: 'approved',
    merged: 'merged',
    deployed: 'deployed',
    verified: 'verified',
    closed: 'closed',
  };
</script>

<script lang="ts">
  let {
    state,
    labState,
    label,
    short = false,
  }: { state: ChangeState; labState?: LabState; label?: string; short?: boolean } = $props();

  const kind = $derived(chipKind(state, labState));
  const text = $derived(label ?? (short && kind === 'labrun' ? 'lab' : LABELS[kind]));
</script>

<span class="chip s-{kind}" data-state={kind}
  >{#if kind === 'labrun'}<span class="dot" aria-hidden="true"></span>{/if}{text}</span
>

<style>
  .chip {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    font-size: 11.5px;
    font-weight: 600;
    line-height: 1.5;
    padding: 1px 8px;
    border-radius: 4px;
    white-space: nowrap;
    align-self: start;
  }
  .dot {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: currentColor;
    animation: pulse 1.4s ease-in-out infinite;
  }
  @keyframes pulse {
    50% {
      opacity: 0.25;
    }
  }
  .s-draft,
  .s-closed {
    background: var(--line);
    color: var(--muted);
  }
  .s-lab,
  .s-labrun {
    background: var(--warn-soft);
    color: var(--warn);
  }
  .s-labfail {
    background: var(--bad-soft);
    color: var(--bad);
  }
  .s-review,
  .s-approved {
    background: var(--info-soft);
    color: var(--info);
  }
  .s-merged,
  .s-deployed,
  .s-verified {
    background: var(--ok-soft);
    color: var(--ok);
  }
</style>
