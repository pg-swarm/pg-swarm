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
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" style={{ width: 20, height: 20 }}>
          <ellipse cx="12" cy="5" rx="9" ry="3"/>
          <path d="M3 5V19C3 20.5 7 22 12 22S21 20.5 21 19V5"/>
          <path d="M3 12C3 13.5 7 15 12 15S21 13.5 21 12"/>
        </svg>
        pg-swarm
      </div>
      <button className="theme-toggle" onClick={cycleTheme} title={`Theme: ${theme}`}>
        <Icon size={13} />
        {theme}
      </button>
    </header>
  );
}
