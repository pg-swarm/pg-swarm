import React, { useState } from 'react';
import { Plus, Pencil, Trash2, X, ChevronDown, ChevronRight, ArrowRight,
         Zap, Play, Shield, RotateCcw, RefreshCw, Terminal, Check } from 'lucide-react';
import { useData } from '../context/DataContext';
import { api } from '../api';

// ── Action types ──────────────────────────────────────────────────────────────

const ACTION_TYPES = [
  { value: 'restart', label: 'Restart', badge: 'badge-amber' },
  { value: 'reload',  label: 'Reload',  badge: 'badge-blue'  },
  { value: 'rebuild', label: 'Rebuild', badge: 'badge-red'   },
  { value: 'reboot',  label: 'Reboot',  badge: 'badge-amber' },
  { value: 'rewind',  label: 'Rewind',  badge: 'badge-blue'  },
  { value: 'exec',    label: 'Execute', badge: 'badge-blue'  },
];
const TYPE_MAP = Object.fromEntries(ACTION_TYPES.map(t => [t.value, t]));

const SEV_BADGE = { critical: 'badge-red', error: 'badge-amber', warning: 'badge-amber', info: 'badge-blue' };
const SEVERITIES = ['critical', 'error', 'warning', 'info'];

function fmtCooldown(s) {
  if (!s) return '—';
  return s < 60 ? s + 's' : Math.floor(s / 60) + 'm' + (s % 60 ? s % 60 + 's' : '');
}

function groupByCategory(rules) {
  const g = {};
  for (const r of rules) {
    const cat = r.category || 'Custom';
    if (!g[cat]) g[cat] = [];
    g[cat].push(r);
  }
  return g;
}

// ── Main component ────────────────────────────────────────────────────────────

export default function EventRulesTab({ toast }) {
  const { eventRules, eventActions, eventHandlers, eventRuleSets, refresh } = useData();
  const [tab, setTab] = useState('events');

  const tabs = [
    { key: 'events',   label: 'Events',   count: (eventRules   || []).length },
    { key: 'actions',  label: 'Actions',  count: (eventActions || []).length },
    { key: 'handlers', label: 'Handlers', count: (eventHandlers|| []).filter(h => h.enabled).length + ' / ' + (eventHandlers || []).length },
    { key: 'rulesets', label: 'Rule Sets',count: (eventRuleSets|| []).length },
  ];

  return (
    <>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', padding: '12px 20px' }}>
        <span className="sm muted">
          Events, actions, and handlers are global. Assign a rule set to a profile to control which handlers are active on a cluster.
        </span>
      </div>
      <div style={{ display: 'flex', gap: 0, borderBottom: '1px solid var(--border)', padding: '0 16px' }}>
        {tabs.map(t => (
          <button
            key={t.key}
            className={'tab-item' + (tab === t.key ? ' active' : '')}
            onClick={() => setTab(t.key)}
            style={{ fontSize: 13 }}
          >
            {t.label}
            <span className="sm" style={{ marginLeft: 6, opacity: tab === t.key ? 1 : 0.5 }}>{t.count}</span>
          </button>
        ))}
      </div>

      {tab === 'events'   && <EventsPanel   rules={eventRules || []}     toast={toast} refresh={refresh} />}
      {tab === 'actions'  && <ActionsPanel  actions={eventActions || []}  toast={toast} refresh={refresh} />}
      {tab === 'handlers' && <HandlersPanel handlers={eventHandlers || []} rules={eventRules || []} actions={eventActions || []} toast={toast} refresh={refresh} />}
      {tab === 'rulesets' && <RuleSetsPanel ruleSets={eventRuleSets || []} handlers={eventHandlers || []} toast={toast} refresh={refresh} />}
    </>
  );
}

// ── Events panel ──────────────────────────────────────────────────────────────

function EventsPanel({ rules, toast, refresh }) {
  const [collapsed, setCollapsed] = useState({});
  const [editing, setEditing] = useState(null); // null | rule object | 'new'
  const grouped = groupByCategory(rules);

  async function save(rule) {
    try {
      if (rule.id) {
        await api.updateEventRule(rule.id, rule);
        toast('Rule updated');
      } else {
        await api.createEventRule(rule);
        toast('Rule created');
      }
      refresh();
      setEditing(null);
    } catch (e) { toast('Save failed: ' + e.message, true); }
  }

  async function del(id) {
    try { await api.deleteEventRule(id); toast('Rule deleted'); refresh(); }
    catch (e) { toast('Delete failed: ' + e.message, true); }
  }

  return (
    <>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', padding: '12px 20px', borderBottom: '1px solid var(--border)' }}>
        <div>
          <div style={{ fontWeight: 600, fontSize: 14, marginBottom: 2 }}>Events</div>
          <span className="sm muted">Log pattern detection rules. Each match emits a named event that handlers can act on.</span>
        </div>
        <button className="btn btn-approve btn-sm" onClick={() => setEditing('new')}><Plus size={13} /> New Event</button>
      </div>
      <table>
        <thead>
          <tr>
            <th style={{ width: 16 }}></th>
            <th>Name</th>
            <th>Severity</th>
            <th>Pattern</th>
            <th>Cooldown</th>
            <th style={{ width: 80 }}></th>
          </tr>
        </thead>
        <tbody>
          {Object.entries(grouped).map(([cat, catRules]) => (
            <React.Fragment key={cat}>
              <tr
                onClick={() => setCollapsed(p => ({ ...p, [cat]: !p[cat] }))}
                style={{ cursor: 'pointer', background: 'var(--bg-secondary)' }}
              >
                <td colSpan={6} style={{ padding: '8px 12px' }}>
                  <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                    {collapsed[cat] ? <ChevronRight size={13} /> : <ChevronDown size={13} />}
                    <span style={{ fontWeight: 600, fontSize: 13 }}>{cat}</span>
                    <span className="muted sm" style={{ marginLeft: 4 }}>{catRules.filter(r => r.enabled).length} / {catRules.length}</span>
                  </div>
                </td>
              </tr>
              {!collapsed[cat] && catRules.map(r => (
                <tr key={r.id || r.name}>
                  <td style={{ width: 16 }}>
                    <span style={{ width: 8, height: 8, borderRadius: '50%', background: r.enabled ? 'var(--green)' : 'var(--text-secondary)', display: 'inline-block' }} />
                  </td>
                  <td><span className="mono sm">{r.name}</span></td>
                  <td><span className={'badge ' + SEV_BADGE[r.severity]}>{r.severity}</span></td>
                  <td className="muted sm" style={{ maxWidth: 320, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{r.pattern}</td>
                  <td className="muted sm">{fmtCooldown(r.cooldown_seconds)}</td>
                  <td>
                    <div className="actions">
                      {!r.builtin && <button className="btn btn-sm" onClick={() => setEditing(r)}><Pencil size={11} /></button>}
                      {!r.builtin && <button className="btn btn-sm btn-reject" onClick={() => del(r.id)}><Trash2 size={11} /></button>}
                    </div>
                  </td>
                </tr>
              ))}
            </React.Fragment>
          ))}
        </tbody>
      </table>

      {editing !== null && (
        <RuleModal
          rule={editing === 'new' ? {} : editing}
          onSave={save}
          onClose={() => setEditing(null)}
        />
      )}
    </>
  );
}

function RuleModal({ rule, onSave, onClose }) {
  const [form, setForm] = useState({
    name: rule.name || '', pattern: rule.pattern || '',
    severity: rule.severity || 'warning', category: rule.category || 'Custom',
    enabled: rule.enabled !== false, cooldown_seconds: rule.cooldown_seconds || 60,
    threshold: rule.threshold || 1, threshold_window_seconds: rule.threshold_window_seconds || 0,
    id: rule.id,
  });
  const [testInput, setTestInput] = useState('');
  const [testResult, setTestResult] = useState(null);
  const [patternError, setPatternError] = useState('');
  const set = (k, v) => setForm(p => ({ ...p, [k]: v }));

  function testPattern() {
    if (!form.pattern || !testInput) { setTestResult(null); return; }
    try {
      const re = new RegExp(form.pattern);
      setPatternError('');
      setTestResult(re.test(testInput));
    } catch (e) { setPatternError(e.message); setTestResult(null); }
  }

  function handlePatternChange(v) {
    set('pattern', v);
    setPatternError('');
    setTestResult(null);
  }

  function submit() {
    if (!form.name.trim() || !form.pattern.trim()) return;
    try { new RegExp(form.pattern); } catch (e) { setPatternError(e.message); return; }
    onSave(form);
  }

  return (
    <div className="confirm-overlay" onClick={onClose}>
      <div className="confirm-modal" onClick={e => e.stopPropagation()} style={{ width: 560 }}>
        <div className="confirm-header">
          <h3>{form.id ? 'Edit Event' : 'New Event'}</h3>
          <button className="modal-close" onClick={onClose}><X size={18} /></button>
        </div>
        <div className="confirm-body">
          <div className="form-grid">
            <div className="form-row">
              <label>Name</label>
              <input className="input" value={form.name}
                onChange={e => set('name', e.target.value.replace(/[^a-z0-9-]/g, ''))}
                placeholder="e.g. custom-deadlock" />
            </div>
            <div className="form-row">
              <label>Severity</label>
              <select className="input" value={form.severity} onChange={e => set('severity', e.target.value)}>
                {SEVERITIES.map(s => <option key={s} value={s}>{s}</option>)}
              </select>
            </div>
          </div>
          <div className="form-row" style={{ marginTop: 12 }}>
            <label>Pattern (Regex)</label>
            <input className={'input mono' + (patternError ? ' input-error' : '')} value={form.pattern}
              onChange={e => handlePatternChange(e.target.value)}
              placeholder="e.g. deadlock detected" style={{ fontSize: 12 }} />
            {patternError && <span className="form-error" style={{ color: 'var(--red)', fontSize: 11 }}>Invalid regex: {patternError}</span>}
          </div>
          <div style={{ marginTop: 8, padding: 12, background: 'var(--bg-secondary)', borderRadius: 6 }}>
            <label style={{ fontSize: 12, fontWeight: 600, marginBottom: 6, display: 'block' }}>Test Pattern</label>
            <div style={{ display: 'flex', gap: 8 }}>
              <input className="input mono" value={testInput}
                onChange={e => { setTestInput(e.target.value); setTestResult(null); }}
                placeholder="Paste a sample log line..." style={{ flex: 1, fontSize: 11 }} />
              <button className="btn btn-sm" onClick={testPattern}><Play size={11} /> Test</button>
            </div>
            {testResult !== null && (
              <div style={{ marginTop: 6 }}>
                {testResult
                  ? <span className="badge badge-green" style={{ fontSize: 11 }}>Match</span>
                  : <span className="badge badge-red" style={{ fontSize: 11 }}>No match</span>}
              </div>
            )}
          </div>
          <div className="form-grid" style={{ marginTop: 12 }}>
            <div className="form-row">
              <label>Category</label>
              <input className="input" value={form.category} onChange={e => set('category', e.target.value)} />
            </div>
            <div className="form-row">
              <label>Cooldown (seconds)</label>
              <input className="input" type="number" min="0" value={form.cooldown_seconds}
                onChange={e => set('cooldown_seconds', parseInt(e.target.value) || 0)} />
            </div>
          </div>
          <div className="form-grid" style={{ marginTop: 12 }}>
            <div className="form-row">
              <label>Threshold</label>
              <input className="input" type="number" min="1" value={form.threshold}
                onChange={e => set('threshold', parseInt(e.target.value) || 1)} />
              <span className="muted" style={{ fontSize: 11 }}>Matches required before firing</span>
            </div>
            <div className="form-row">
              <label>Window (seconds)</label>
              <input className="input" type="number" min="0" value={form.threshold_window_seconds}
                onChange={e => set('threshold_window_seconds', parseInt(e.target.value) || 0)} />
              <span className="muted" style={{ fontSize: 11 }}>Time window for counting (0 = single match)</span>
            </div>
          </div>
        </div>
        <div className="confirm-footer">
          <button className="btn" onClick={onClose}>Cancel</button>
          <button className="btn btn-approve" onClick={submit}>{form.id ? 'Update' : 'Create'}</button>
        </div>
      </div>
    </div>
  );
}

// ── Actions panel ─────────────────────────────────────────────────────────────

function ActionsPanel({ actions, toast, refresh }) {
  const [editing, setEditing] = useState(null);

  async function save(action) {
    try {
      if (action.id) {
        await api.updateEventAction(action.id, action);
        toast('Action updated');
      } else {
        await api.createEventAction(action);
        toast('Action created');
      }
      refresh();
      setEditing(null);
    } catch (e) { toast('Save failed: ' + e.message, true); }
  }

  async function del(id) {
    try { await api.deleteEventAction(id); toast('Action deleted'); refresh(); }
    catch (e) { toast('Delete failed: ' + e.message, true); }
  }

  if (editing !== null) {
    return <ActionEditor action={editing === 'new' ? {} : editing} onSave={save} onCancel={() => setEditing(null)} />;
  }

  return (
    <>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', padding: '12px 20px', borderBottom: '1px solid var(--border)' }}>
        <div>
          <div style={{ fontWeight: 600, fontSize: 14, marginBottom: 2 }}>Actions</div>
          <span className="sm muted">Reusable named actions that can be triggered by event handlers.</span>
        </div>
        <button className="btn btn-approve btn-sm" onClick={() => setEditing('new')}><Plus size={13} /> New Action</button>
      </div>
      <table>
        <thead><tr><th>Name</th><th>Type</th><th>Description</th><th style={{ width: 80 }}></th></tr></thead>
        <tbody>
          {actions.map(a => (
            <tr key={a.id || a.name}>
              <td><span className="mono sm">{a.name}</span></td>
              <td><span className={'badge ' + (TYPE_MAP[a.type]?.badge || '')}>{a.type}</span></td>
              <td className="muted sm">{a.description}</td>
              <td>
                <div className="actions">
                  <button className="btn btn-sm" onClick={() => setEditing(a)}><Pencil size={11} /></button>
                  <button className="btn btn-sm btn-reject" onClick={() => del(a.id)}><Trash2 size={11} /></button>
                </div>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </>
  );
}

function ActionEditor({ action, onSave, onCancel }) {
  const [form, setForm] = useState({
    name: action.name || '', type: action.type || 'restart',
    description: action.description || '',
    config: typeof action.config === 'object' ? JSON.stringify(action.config, null, 2) : (action.config || '{}'),
    id: action.id,
  });
  const set = (k, v) => setForm(p => ({ ...p, [k]: v }));

  function submit() {
    let config;
    try { config = JSON.parse(form.config); } catch { onSave = () => {}; alert('Invalid JSON config'); return; }
    onSave({ ...form, config });
  }

  return (
    <div style={{ padding: 20, maxWidth: 560 }}>
      <h4 style={{ marginBottom: 16 }}>{form.id ? 'Edit Action' : 'New Action'}</h4>
      <div className="form-row"><label>Name</label><input className="input" value={form.name} onChange={e => set('name', e.target.value)} /></div>
      <div className="form-row"><label>Type</label>
        <select className="input" value={form.type} onChange={e => set('type', e.target.value)}>
          {ACTION_TYPES.map(t => <option key={t.value} value={t.value}>{t.label}</option>)}
        </select>
      </div>
      <div className="form-row"><label>Description</label><input className="input" value={form.description} onChange={e => set('description', e.target.value)} /></div>
      <div className="form-row"><label>Config (JSON)</label>
        <textarea className="input mono" rows={4} value={form.config} onChange={e => set('config', e.target.value)} style={{ fontFamily: 'monospace', fontSize: 12 }} />
      </div>
      <div style={{ display: 'flex', gap: 8, marginTop: 16 }}>
        <button className="btn btn-sm" onClick={onCancel}>Cancel</button>
        <button className="btn btn-sm btn-approve" onClick={submit}>Save</button>
      </div>
    </div>
  );
}

// ── Handlers panel ────────────────────────────────────────────────────────────

function HandlersPanel({ handlers, rules, actions, toast, refresh }) {
  const [editing, setEditing] = useState(null);

  const ruleMap   = Object.fromEntries((rules   || []).map(r => [r.id, r]));
  const actionMap = Object.fromEntries((actions || []).map(a => [a.id, a]));

  async function save(h) {
    try {
      if (h.id) {
        await api.updateEventHandler(h.id, h);
        toast('Handler updated');
      } else {
        await api.createEventHandler(h);
        toast('Handler created');
      }
      refresh();
      setEditing(null);
    } catch (e) { toast('Save failed: ' + e.message, true); }
  }

  async function del(id) {
    try { await api.deleteEventHandler(id); toast('Handler deleted'); refresh(); }
    catch (e) { toast('Delete failed: ' + e.message, true); }
  }

  if (editing !== null) {
    return <HandlerEditor handler={editing === 'new' ? {} : editing} rules={rules} actions={actions} onSave={save} onCancel={() => setEditing(null)} />;
  }

  return (
    <>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', padding: '12px 20px', borderBottom: '1px solid var(--border)' }}>
        <div>
          <div style={{ fontWeight: 600, fontSize: 14, marginBottom: 2 }}>Handlers</div>
          <span className="sm muted">Bindings that connect an event to an action. Add a handler to a rule set to activate it on a cluster.</span>
        </div>
        <button className="btn btn-approve btn-sm" onClick={() => setEditing('new')}><Plus size={13} /> New Handler</button>
      </div>
      <table>
        <thead><tr><th>Event</th><th></th><th>Action</th><th>Type</th><th>Enabled</th><th style={{ width: 80 }}></th></tr></thead>
        <tbody>
          {handlers.map(h => {
            const rule   = ruleMap[h.event_rule_id];
            const action = actionMap[h.event_action_id];
            return (
              <tr key={h.id}>
                <td><span className="mono sm">{h.event_rule_name || rule?.name || '—'}</span></td>
                <td><ArrowRight size={12} className="muted" /></td>
                <td><span className="mono sm">{h.event_action_name || action?.name || '—'}</span></td>
                <td>{action && <span className={'badge ' + (TYPE_MAP[action.type]?.badge || '')}>{action.type}</span>}</td>
                <td><span style={{ width: 8, height: 8, borderRadius: '50%', background: h.enabled ? 'var(--green)' : 'var(--text-secondary)', display: 'inline-block' }} /></td>
                <td>
                  <div className="actions">
                    <button className="btn btn-sm" onClick={() => setEditing(h)}><Pencil size={11} /></button>
                    <button className="btn btn-sm btn-reject" onClick={() => del(h.id)}><Trash2 size={11} /></button>
                  </div>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </>
  );
}

function HandlerEditor({ handler, rules, actions, onSave, onCancel }) {
  const [form, setForm] = useState({
    event_rule_id:   handler.event_rule_id   || '',
    event_action_id: handler.event_action_id || '',
    enabled: handler.enabled !== false,
    id: handler.id,
  });
  const set = (k, v) => setForm(p => ({ ...p, [k]: v }));

  return (
    <div style={{ padding: 20, maxWidth: 480 }}>
      <h4 style={{ marginBottom: 16 }}>{form.id ? 'Edit Handler' : 'New Handler'}</h4>
      <div className="form-row"><label>Event Rule</label>
        <select className="input" value={form.event_rule_id} onChange={e => set('event_rule_id', e.target.value)}>
          <option value="">— select —</option>
          {(rules || []).map(r => <option key={r.id} value={r.id}>{r.name}</option>)}
        </select>
      </div>
      <div className="form-row"><label>Action</label>
        <select className="input" value={form.event_action_id} onChange={e => set('event_action_id', e.target.value)}>
          <option value="">— select —</option>
          {(actions || []).map(a => <option key={a.id} value={a.id}>{a.name} ({a.type})</option>)}
        </select>
      </div>
      <div className="form-row"><label>Enabled</label><input type="checkbox" checked={form.enabled} onChange={e => set('enabled', e.target.checked)} /></div>
      <div style={{ display: 'flex', gap: 8, marginTop: 16 }}>
        <button className="btn btn-sm" onClick={onCancel}>Cancel</button>
        <button className="btn btn-sm btn-approve" onClick={() => onSave(form)}>Save</button>
      </div>
    </div>
  );
}

// ── Rule Sets panel ───────────────────────────────────────────────────────────

function RuleSetsPanel({ ruleSets, handlers, toast, refresh }) {
  const [editing, setEditing] = useState(null); // null | ruleSet | 'new'
  const [managing, setManaging] = useState(null); // ruleSet being managed

  async function save(rs) {
    try {
      if (rs.id) {
        await api.updateEventRuleSet(rs.id, rs);
        toast('Rule set updated');
      } else {
        await api.createEventRuleSet(rs);
        toast('Rule set created');
      }
      refresh();
      setEditing(null);
    } catch (e) { toast('Save failed: ' + e.message, true); }
  }

  async function del(id) {
    try { await api.deleteEventRuleSet(id); toast('Rule set deleted'); refresh(); }
    catch (e) { toast('Delete failed: ' + e.message, true); }
  }

  if (editing !== null) {
    return <RuleSetEditor rs={editing === 'new' ? {} : editing} onSave={save} onCancel={() => setEditing(null)} />;
  }

  if (managing !== null) {
    return <RuleSetHandlerManager ruleSet={managing} allHandlers={handlers} toast={toast} onClose={() => { setManaging(null); refresh(); }} />;
  }

  return (
    <>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', padding: '10px 16px' }}>
        <span className="sm muted">Rule sets are named groups of handlers assigned to profiles to control which automations run on a cluster.</span>
        <button className="btn btn-approve btn-sm" onClick={() => setEditing('new')}><Plus size={13} /> New Rule Set</button>
      </div>
      {ruleSets.length === 0 ? (
        <div className="empty-state" style={{ padding: '40px 20px' }}>
          <Zap size={48} strokeWidth={1.2} />
          <h3>No rule sets</h3>
          <p>Create a rule set to assign a group of handlers to a profile.</p>
        </div>
      ) : (
        <table>
          <thead><tr><th>Name</th><th>Description</th><th></th></tr></thead>
          <tbody>
            {ruleSets.map(rs => (
              <tr key={rs.id}>
                <td>
                  <span className="mono">{rs.name}</span>
                  {rs.builtin && <span className="badge" style={{ marginLeft: 6, fontSize: 9 }}>built-in</span>}
                </td>
                <td className="sm muted">{rs.description || '—'}</td>
                <td>
                  <div className="actions">
                    <button className="btn btn-sm" onClick={() => setManaging(rs)}><Shield size={11} /> Handlers</button>
                    {!rs.builtin && <button className="btn btn-sm" onClick={() => setEditing(rs)}><Pencil size={11} /></button>}
                    {!rs.builtin && <button className="btn btn-sm btn-reject" onClick={() => del(rs.id)}><Trash2 size={11} /></button>}
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}

function RuleSetEditor({ rs, onSave, onCancel }) {
  const [form, setForm] = useState({ name: rs.name || '', description: rs.description || '', id: rs.id });
  const set = (k, v) => setForm(p => ({ ...p, [k]: v }));
  return (
    <div style={{ padding: 20, maxWidth: 480 }}>
      <h4 style={{ marginBottom: 16 }}>{form.id ? 'Edit Rule Set' : 'New Rule Set'}</h4>
      <div className="form-row"><label>Name</label><input className="input" value={form.name} onChange={e => set('name', e.target.value)} /></div>
      <div className="form-row"><label>Description</label><input className="input" value={form.description} onChange={e => set('description', e.target.value)} /></div>
      <div style={{ display: 'flex', gap: 8, marginTop: 16 }}>
        <button className="btn btn-sm" onClick={onCancel}>Cancel</button>
        <button className="btn btn-sm btn-approve" onClick={() => onSave(form)}>Save</button>
      </div>
    </div>
  );
}

function RuleSetHandlerManager({ ruleSet, allHandlers, toast, onClose }) {
  const [included, setIncluded] = useState(null); // loaded lazily
  const [loading, setLoading] = useState(true);

  useState(() => {
    api.listRuleSetHandlers(ruleSet.id)
      .then(data => { setIncluded(new Set((data || []).map(h => h.id))); setLoading(false); })
      .catch(() => { setIncluded(new Set()); setLoading(false); });
  });

  async function toggle(handlerId, currently) {
    try {
      if (currently) {
        await api.removeHandlerFromRuleSet(ruleSet.id, handlerId);
      } else {
        await api.addHandlerToRuleSet(ruleSet.id, handlerId);
      }
      setIncluded(prev => {
        const next = new Set(prev);
        currently ? next.delete(handlerId) : next.add(handlerId);
        return next;
      });
    } catch (e) { toast('Failed: ' + e.message, true); }
  }

  if (loading) return <div style={{ padding: 20 }}>Loading...</div>;

  return (
    <div style={{ padding: 20 }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 16 }}>
        <h4>Handlers in <span className="mono">{ruleSet.name}</span></h4>
        <button className="btn btn-sm" onClick={onClose}><X size={13} /> Done</button>
      </div>
      <table>
        <thead><tr><th style={{ width: 32 }}></th><th>Event</th><th></th><th>Action</th></tr></thead>
        <tbody>
          {allHandlers.map(h => {
            const on = included.has(h.id);
            return (
              <tr key={h.id} style={{ opacity: on ? 1 : 0.5 }}>
                <td>
                  <input type="checkbox" checked={on} onChange={() => toggle(h.id, on)} />
                </td>
                <td><span className="mono sm">{h.event_rule_name}</span></td>
                <td><ArrowRight size={11} className="muted" /></td>
                <td><span className="mono sm">{h.event_action_name}</span></td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
