// Theme choice and the theme on screen. Pure, tested with node --test.
export type Theme = 'system' | 'light' | 'dark';

/** The theme on screen for a choice and the system preference. */
export function resolveTheme(theme: Theme, systemDark: boolean): 'light' | 'dark' {
  return theme === 'system' ? (systemDark ? 'dark' : 'light') : theme;
}

/** The choice after the top bar toggle: the opposite of what is on screen. */
export function toggledTheme(theme: Theme, systemDark: boolean): 'light' | 'dark' {
  return resolveTheme(theme, systemDark) === 'dark' ? 'light' : 'dark';
}
