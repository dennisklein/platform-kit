// Per-browser UI state: theme and side panel. Kept in localStorage when
// the browser allows it.
export type Theme = 'system' | 'light' | 'dark';
export type SideTab = 'chat' | 'activity';

function load<T extends string>(key: string, fallback: T, allowed: readonly T[]): T {
  try {
    const v = localStorage.getItem(key) as T | null;
    return v && allowed.includes(v) ? v : fallback;
  } catch {
    return fallback;
  }
}

function save(key: string, v: string) {
  try {
    localStorage.setItem(key, v);
  } catch {
    // private mode: keep it in memory only
  }
}

class UI {
  theme = $state<Theme>(load('pk.theme', 'system', ['system', 'light', 'dark'] as const));
  sideOpen = $state(load('pk.side', 'closed', ['open', 'closed'] as const) === 'open');
  sideTab = $state<SideTab>(load('pk.sideTab', 'chat', ['chat', 'activity'] as const));
  /** Set by the change rail "New" button; the new-change dialog reads it. */
  newChangeOpen = $state(false);

  applyTheme() {
    const el = document.documentElement;
    if (this.theme === 'system') el.removeAttribute('data-theme');
    else el.setAttribute('data-theme', this.theme);
  }

  setTheme(t: Theme) {
    this.theme = t;
    save('pk.theme', t);
    this.applyTheme();
  }

  cycleTheme() {
    const next: Record<Theme, Theme> = { system: 'dark', dark: 'light', light: 'system' };
    this.setTheme(next[this.theme]);
  }

  openSide(tab?: SideTab) {
    if (tab) this.sideTab = tab;
    this.sideOpen = true;
    save('pk.side', 'open');
    save('pk.sideTab', this.sideTab);
  }

  toggleSide() {
    this.sideOpen = !this.sideOpen;
    save('pk.side', this.sideOpen ? 'open' : 'closed');
  }
}

export const ui = new UI();
