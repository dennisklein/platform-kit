// The forge (Gitea) pill of the top bar. The bench says the forge is down
// in two ways: /status (a ping now) and the change list (forgeOffline: the
// last fetch or forge poll failed, the list comes from the local clone).

export interface ForgeStatusLike {
  name?: string;
  reachable: boolean;
  url?: string;
  detail?: string;
}

export function forgePill(
  st: ForgeStatusLike | undefined,
  listOffline = false,
): { label: string; tone: '' | 'ok' | 'warn'; title: string } {
  const name = st?.name || 'Gitea';
  if (!st && !listOffline) return { label: 'Forge', tone: '', title: '' };
  if (listOffline || st?.reachable === false) {
    const why = st?.reachable === false && st.detail ? ` (${st.detail})` : '';
    return {
      label: `${name} offline`,
      tone: 'warn',
      title: `${name} offline: showing local state${why}. Edit, commit and labs still work; push and merge wait for the forge.`,
    };
  }
  return { label: name, tone: 'ok', title: st?.detail ?? st?.url ?? '' };
}
