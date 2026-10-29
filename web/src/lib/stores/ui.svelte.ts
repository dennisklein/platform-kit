// Per-browser UI state: theme and side panel. Kept in localStorage when
// the browser allows it.
import { resolveTheme, toggledTheme, type Theme } from '../f/theme';

export type { Theme };
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

function systemPrefersDark(): boolean {
  try {
    return matchMedia('(prefers-color-scheme: dark)').matches;
  } catch {
    return false;
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

  /** The system asks for dark; follows changes while the page is open. */
  systemDark = $state(systemPrefersDark());

  constructor() {
    try {
      matchMedia('(prefers-color-scheme: dark)').addEventListener('change', (e) => {
        this.systemDark = e.matches;
      });
    } catch {
      // no matchMedia (tests): keep the first value
    }
  }

  /** The theme on screen: the choice, or the system theme for 'system'. */
  get shownTheme(): 'light' | 'dark' {
    return resolveTheme(this.theme, this.systemDark);
  }

  /** Top bar button: always switch what is on screen, light to dark or back.
   *  'System' stays a choice in Settings. */
  toggleTheme() {
    this.setTheme(toggledTheme(this.theme, this.systemDark));
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
