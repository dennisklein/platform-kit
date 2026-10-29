// Which open MR the demo "approve as" buttons act on.

export interface DemoCandidate {
  id: string;
  state: string;
}

/**
 * The change to approve: the user's explicit pick, else the change on
 * screen, else the change the user works on, else one waiting for review,
 * else the first open MR. Empty when there is none.
 */
export function demoTarget(
  candidates: DemoCandidate[],
  o: { picked?: string; onScreen?: string; working?: string } = {},
): string {
  if (o.picked) return o.picked;
  const has = (id?: string) => !!id && candidates.some((c) => c.id === id);
  if (has(o.onScreen)) return o.onScreen!;
  if (has(o.working)) return o.working!;
  return candidates.find((c) => c.state === 'review')?.id ?? candidates[0]?.id ?? '';
}
