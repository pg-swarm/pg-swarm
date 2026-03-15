import { useTheme } from '../context/ThemeContext';
import { Sun, Moon, Monitor } from 'lucide-react';

const THEME_ICONS = { light: Sun, dark: Moon, system: Monitor };
const THEME_CYCLE = ['light', 'dark', 'system'];

export default function MiniHeader() {
  const { theme, setTheme } = useTheme();
  const Icon = THEME_ICONS[theme];

  function cycleTheme() {
    const idx = THEME_CYCLE.indexOf(theme);
    setTheme(THEME_CYCLE[(idx + 1) % THEME_CYCLE.length]);
  }

  return (
    <header className="topbar" style={{ height: 42 }}>
      <div className="brand" style={{ fontSize: 14 }}>
        <img src="/favicon.svg" alt="PG-Swarm" width="20" height="20" />
        PG-Swarm
      </div>
      <button className="theme-toggle" onClick={cycleTheme} title={`Theme: ${theme}`}>
        <Icon size={13} />
        {theme}
      </button>
    </header>
  );
}
