// A forge reset (pk forge-init --reset, make reset) rebuilds main: its head
// moves to an older merge. Normal merges only move main forward in time.
export interface MainLike {
  sha: string;
  at?: string;
}

export function mainRewritten(prev: MainLike | undefined | null, cur: MainLike | undefined | null): boolean {
  if (!prev?.sha || !cur?.sha || prev.sha === cur.sha || !prev.at || !cur.at) return false;
  const a = Date.parse(prev.at);
  const b = Date.parse(cur.at);
  return Number.isFinite(a) && Number.isFinite(b) && b < a;
}
