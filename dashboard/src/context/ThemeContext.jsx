import { createContext, useContext, useState, useEffect, useMemo } from 'react';

const ThemeContext = createContext();

const STORAGE_KEY = 'pgswarm-theme';
const VALID = ['light', 'dark', 'system'];

function getInitial() {
  try {
    const stored = localStorage.getItem(STORAGE_KEY);
    if (VALID.includes(stored)) return stored;
  } catch { /* ignore */ }
  return 'system';
}

function resolveTheme(pref) {
  if (pref === 'light' || pref === 'dark') return pref;
  return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
}

export function ThemeProvider({ children }) {
  const [theme, setThemeRaw] = useState(getInitial);
  const [resolved, setResolved] = useState(() => resolveTheme(getInitial()));

  function setTheme(next) {
    if (!VALID.includes(next)) return;
    setThemeRaw(next);
    try { localStorage.setItem(STORAGE_KEY, next); } catch { /* ignore */ }
  }

  // Apply resolved theme to document and listen for OS changes
  useEffect(() => {
    function apply() {
      const r = resolveTheme(theme);
      setResolved(r);
      document.documentElement.setAttribute('data-theme', r);
    }

    apply();

    const mq = window.matchMedia('(prefers-color-scheme: dark)');
    function onChange() {
      if (theme === 'system') apply();
    }
    mq.addEventListener('change', onChange);
    return () => mq.removeEventListener('change', onChange);
  }, [theme]);

  const value = useMemo(() => ({ theme, setTheme, resolved }), [theme, resolved]);

  return (
    <ThemeContext.Provider value={value}>
      {children}
    </ThemeContext.Provider>
  );
}

export function useTheme() {
  const ctx = useContext(ThemeContext);
  if (!ctx) throw new Error('useTheme must be used within ThemeProvider');
  return ctx;
}
