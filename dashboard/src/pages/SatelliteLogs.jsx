import { useState, useEffect, useRef, useCallback } from 'react';
import { useParams } from 'react-router-dom';
import { api, subscribeSatelliteLogs, deriveSatState, timeAgo } from '../api';
import { Download } from 'lucide-react';
import MiniHeader from '../components/MiniHeader';

const LEVELS = ['trace', 'debug', 'info', 'warn', 'error'];
const MAX_LOCAL = 5000;

const levelColor = {
  trace: '#888',
  debug: '#5b9bd5',
  info:  '#4caf50',
  warn:  '#ff9800',
  warning: '#ff9800',
  error: '#e53935',
  fatal: '#b71c1c',
};

const stateColor = {
  connected: '#4caf50',
  pending:   '#ff9800',
  offline:   '#888',
  disconnected: '#e53935',
};

function LevelPill({ level }) {
  return (
    <span style={{
      background: levelColor[level] || '#888',
      color: '#fff',
      padding: '1px 6px',
      borderRadius: 3,
      fontSize: 11,
      fontWeight: 600,
      textTransform: 'uppercase',
      minWidth: 42,
      display: 'inline-block',
      textAlign: 'center',
    }}>
      {level}
    </span>
  );
}

function levelPri(l) {
  return LEVELS.indexOf(l) === -1 ? 0 : LEVELS.indexOf(l);
}

function escapeCSV(val) {
  if (val == null) return '';
  const s = String(val);
  if (s.includes(',') || s.includes('"') || s.includes('\n')) {
    return '"' + s.replace(/"/g, '""') + '"';
  }
  return s;
}

function downloadCSV(entries) {
  const header = 'timestamp,level,logger,message,fields';
  const rows = entries.map(e => {
    const ts = e.timestamp ? new Date(e.timestamp).toISOString() : '';
    const fields = e.fields && Object.keys(e.fields).length > 0
      ? Object.entries(e.fields).map(([k, v]) => `${k}=${v}`).join('; ')
      : '';
    return [ts, e.level, e.logger || '', e.message, fields].map(escapeCSV).join(',');
  });
  const blob = new Blob([header + '\n' + rows.join('\n')], { type: 'text/csv' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = `satellite-logs-${new Date().toISOString().slice(0, 19)}.csv`;
  a.click();
  URL.revokeObjectURL(url);
}

export default function SatelliteLogs() {
  const { id } = useParams();
  const [logs, setLogs] = useState([]);
  const [serverLevel, setServerLevel] = useState('info');
  const [filterLevel, setFilterLevel] = useState('trace');
  const [autoScroll, setAutoScroll] = useState(true);
  const [sat, setSat] = useState(null);
  const [selected, setSelected] = useState(new Set());
  const endRef = useRef(null);

  // Fetch satellite info + set document title
  useEffect(() => {
    api.satellites().then(sats => {
      const found = sats.find(s => s.id === id);
      if (found) {
        setSat(found);
        document.title = `Logs: ${found.hostname || found.k8s_cluster_name} - PG-Swarm`;
      }
    }).catch(() => {});
    return () => { document.title = 'PG-Swarm'; };
  }, [id]);

  // Fetch recent logs + SSE subscription
  useEffect(() => {
    let unsub;
    api.satelliteLogs(id, 500, 'trace').then(data => {
      setLogs(Array.isArray(data) ? data : []);
    }).catch(() => {});

    unsub = subscribeSatelliteLogs(id, entry => {
      setLogs(prev => {
        const next = [...prev, entry];
        return next.length > MAX_LOCAL ? next.slice(next.length - MAX_LOCAL) : next;
      });
    });

    return () => { if (unsub) unsub(); };
  }, [id]);

  // Auto-scroll
  useEffect(() => {
    if (autoScroll && endRef.current) {
      endRef.current.scrollIntoView({ behavior: 'smooth' });
    }
  }, [logs, autoScroll]);

  const changeServerLevel = useCallback(async (level) => {
    try {
      await api.setSatelliteLogLevel(id, level);
      setServerLevel(level);
    } catch { /* ignore */ }
  }, [id]);

  const filtered = logs.filter(e => levelPri(e.level) >= levelPri(filterLevel));

  function toggleSelect(idx) {
    setSelected(prev => {
      const next = new Set(prev);
      if (next.has(idx)) next.delete(idx); else next.add(idx);
      return next;
    });
  }

  function toggleSelectAll() {
    if (selected.size === filtered.length) {
      setSelected(new Set());
    } else {
      setSelected(new Set(filtered.map((_, i) => i)));
    }
  }

  function handleDownload() {
    const entries = selected.size > 0
      ? filtered.filter((_, i) => selected.has(i))
      : filtered;
    downloadCSV(entries);
  }

  const state = sat ? deriveSatState(sat) : '';
  const labels = sat?.labels || {};
  const labelEntries = Object.entries(labels);

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100vh', background: 'var(--bg)', color: 'var(--text)' }}>
      <MiniHeader />

      {/* Header bar */}
      <div style={{
        background: 'var(--white)',
        borderBottom: '1px solid var(--border)',
        padding: '10px 16px',
        display: 'flex',
        flexDirection: 'column',
        gap: 6,
        flexShrink: 0,
      }}>
        {/* Row 1: satellite identity + state */}
        <div style={{ display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' }}>
          <span style={{ fontWeight: 700, fontSize: 15, color: 'var(--text)' }}>
            {sat?.hostname || id}
          </span>
          {sat?.k8s_cluster_name && (
            <span style={{ color: 'var(--text-secondary)', fontSize: 13 }}>
              {sat.k8s_cluster_name}
            </span>
          )}
          {sat?.region && (
            <span style={{ background: 'var(--gray-bg)', padding: '1px 8px', borderRadius: 3, fontSize: 12, color: 'var(--text-secondary)' }}>
              {sat.region}
            </span>
          )}
          {state && (
            <span style={{
              background: stateColor[state] || '#888',
              color: '#fff',
              padding: '1px 8px',
              borderRadius: 3,
              fontSize: 11,
              fontWeight: 600,
              textTransform: 'uppercase',
            }}>
              {state}
            </span>
          )}
          {sat && (
            <span style={{ color: 'var(--text-secondary)', fontSize: 12 }}>
              heartbeat: {timeAgo(sat.last_heartbeat)}
            </span>
          )}

          {/* Labels */}
          {labelEntries.length > 0 && (
            <div style={{ display: 'flex', gap: 4, flexWrap: 'wrap' }}>
              {labelEntries.map(([k, v]) => (
                <span key={k} style={{
                  background: 'var(--gray-bg)',
                  border: '1px solid var(--border)',
                  padding: '0 6px',
                  borderRadius: 3,
                  fontSize: 11,
                  color: 'var(--text-secondary)',
                }}>
                  {k}={v}
                </span>
              ))}
            </div>
          )}
        </div>

        {/* Row 2: controls */}
        <div style={{ display: 'flex', alignItems: 'center', gap: 12, fontSize: 13, flexWrap: 'wrap' }}>
          <label style={{ color: 'var(--text-secondary)' }}>
            Stream level:
            <select value={serverLevel} onChange={e => changeServerLevel(e.target.value)}
              style={{ marginLeft: 4, background: 'var(--gray-bg)', color: 'var(--text)', border: '1px solid var(--border)', borderRadius: 3, padding: '2px 4px', fontSize: 12 }}>
              {LEVELS.map(l => <option key={l} value={l}>{l}</option>)}
            </select>
          </label>
          <label style={{ color: 'var(--text-secondary)' }}>
            Filter:
            <select value={filterLevel} onChange={e => setFilterLevel(e.target.value)}
              style={{ marginLeft: 4, background: 'var(--gray-bg)', color: 'var(--text)', border: '1px solid var(--border)', borderRadius: 3, padding: '2px 4px', fontSize: 12 }}>
              {LEVELS.map(l => <option key={l} value={l}>{l}</option>)}
            </select>
          </label>
          <label style={{ cursor: 'pointer', color: 'var(--text-secondary)' }}>
            <input type="checkbox" checked={autoScroll} onChange={e => setAutoScroll(e.target.checked)} />
            {' '}Auto-scroll
          </label>
          <span style={{ color: 'var(--text-secondary)', fontSize: 12 }}>{filtered.length} entries{selected.size > 0 ? ` (${selected.size} selected)` : ''}</span>
          <div style={{ marginLeft: 'auto', display: 'flex', gap: 8 }}>
            <button onClick={handleDownload} style={{
              background: 'var(--gray-bg)', color: 'var(--text)', border: '1px solid var(--border)', borderRadius: 4,
              padding: '3px 10px', fontSize: 12, cursor: 'pointer', display: 'flex', alignItems: 'center', gap: 4,
            }}>
              <Download size={12} /> {selected.size > 0 ? `Download ${selected.size}` : 'Download all'}
            </button>
            <button onClick={() => { setLogs([]); setSelected(new Set()); }} style={{
              background: 'var(--gray-bg)', color: 'var(--text)', border: '1px solid var(--border)', borderRadius: 4,
              padding: '3px 10px', fontSize: 12, cursor: 'pointer',
            }}>
              Clear
            </button>
          </div>
        </div>
      </div>

      {/* Log stream area -- always dark (terminal view) */}
      <div style={{
        flex: 1,
        overflow: 'auto',
        fontFamily: 'monospace',
        fontSize: 12,
        lineHeight: '20px',
        padding: '4px 0',
        background: '#0d1117',
        color: '#c9d1d9',
      }}>
        {filtered.length === 0 && (
          <div style={{ color: '#484f58', padding: 40, textAlign: 'center' }}>
            No log entries. Set the stream level and wait for satellite activity.
          </div>
        )}
        {/* Select-all row */}
        {filtered.length > 0 && (
          <div style={{ padding: '2px 8px', borderBottom: '1px solid #21262d', color: '#484f58', fontSize: 11 }}>
            <label style={{ cursor: 'pointer' }}>
              <input type="checkbox" checked={selected.size === filtered.length && filtered.length > 0}
                onChange={toggleSelectAll} style={{ marginRight: 6 }} />
              Select all
            </label>
          </div>
        )}
        {filtered.map((e, i) => {
          const ts = e.timestamp ? new Date(e.timestamp).toISOString().slice(11, 23) : '';
          const fields = e.fields && Object.keys(e.fields).length > 0
            ? ' ' + Object.entries(e.fields).map(([k, v]) => `${k}=${v}`).join(' ')
            : '';
          const isSelected = selected.has(i);
          return (
            <div key={i} onClick={() => toggleSelect(i)} style={{
              padding: '1px 8px',
              borderBottom: '1px solid #161b22',
              background: isSelected ? '#1c2333' : 'transparent',
              cursor: 'pointer',
              userSelect: 'none',
            }}>
              <input type="checkbox" checked={isSelected} readOnly
                style={{ marginRight: 6, verticalAlign: 'middle', pointerEvents: 'none' }} />
              <span style={{ color: '#484f58' }}>{ts}</span>
              {' '}
              <LevelPill level={e.level} />
              {' '}
              {e.logger && <span style={{ color: '#8b949e' }}>[{e.logger}]</span>}
              {' '}
              <span>{e.message}</span>
              {fields && <span style={{ color: '#6e7681' }}>{fields}</span>}
            </div>
          );
        })}
        <div ref={endRef} />
      </div>
    </div>
  );
}
