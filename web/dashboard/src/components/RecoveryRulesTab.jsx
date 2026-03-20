import { useState } from 'react';
import { Plus, Pencil, Trash2, Copy, Shield, X } from 'lucide-react';
import { useData } from '../context/DataContext';
import { api } from '../api';

// Normalize a ruleset from the backend: `config` (JSON string or array) → `rules` (array).
function normalizeRuleSet(rs) {
  if (rs.rules && Array.isArray(rs.rules)) return rs;
  let rules = [];
  if (rs.config) {
    try {
      rules = typeof rs.config === 'string' ? JSON.parse(rs.config) : rs.config;
    } catch { rules = []; }
  }
  return { ...rs, rules: Array.isArray(rules) ? rules : [] };
}

const ACTIONS = [
  { value: 'event', label: 'Report Event' },
  { value: 'restart', label: 'Restart Instance' },
  { value: 'rewind', label: 'Resync Instance' },
  { value: 'rebasebackup', label: 'Rebuild Instance' },
  { value: 'exec', label: 'Run Command' },
];

const ACTION_DESCS = {
  event: 'Send event to dashboard — no disruption to the instance',
  restart: 'Stop and restart the instance — wrapper handles recovery',
  rewind: 'Resync diverged data from primary without full rebuild',
  rebasebackup: 'Delete data directory and rebuild from primary',
  exec: 'Execute a custom command in the postgres container',
};

const SEVERITIES = ['critical', 'error', 'warning', 'info'];

const SEV_CLASS = { critical: 'badge-red', error: 'badge-amber', warning: 'badge-amber', info: 'badge-blue' };
const ACT_CLASS = { event: '', restart: 'badge-amber', rewind: 'badge-amber', rebasebackup: 'badge-red', exec: 'badge-blue' };
const ACT_LABEL = { event: 'Report', restart: 'Restart', rewind: 'Resync', rebasebackup: 'Rebuild', exec: 'Command' };

function formatCooldown(s) {
  if (s === 0) return 'none';
  if (s < 60) return s + 's';
  return Math.floor(s / 60) + 'm' + (s % 60 ? s % 60 + 's' : '');
}

function groupByCategory(rules) {
  const groups = {};
  for (const rule of rules) {
    const cat = rule.category || 'Custom';
    if (!groups[cat]) groups[cat] = [];
    groups[cat].push(rule);
  }
  return groups;
}

// ── Main component (Admin tab) ───────────────────────────────────────────────

export default function RecoveryRulesTab({ toast }) {
  const { recoveryRuleSets: rawRuleSets, refresh } = useData();
  const ruleSets = (rawRuleSets || []).map(normalizeRuleSet);
  const [editingId, setEditingId] = useState(null);
  const [editingRuleSet, setEditingRuleSet] = useState(null);

  // Use Default from backend as the template for new rulesets
  const defaultRS = ruleSets.find(rs => rs.builtin) || { rules: [] };

  function startCreate() {
    setEditingRuleSet({
      name: '',
      description: '',
      builtin: false,
      rules: defaultRS.rules.map(r => ({ ...r, builtin: false })),
    });
    setEditingId(null);
  }

  function cloneRuleSet(rs) {
    setEditingRuleSet({
      name: rs.name + ' (copy)',
      description: rs.description,
      builtin: false,
      rules: rs.rules.map(r => ({ ...r, builtin: false })),
    });
    setEditingId(null);
  }

  function startEdit(rs) {
    setEditingRuleSet({ ...rs, rules: rs.rules.map(r => ({ ...r })) });
    setEditingId(rs.id);
  }

  async function saveRuleSet() {
    if (!editingRuleSet.name.trim()) {
      toast('Name is required', true);
      return;
    }
    // Backend stores rules in `config` (JSON), not `rules` (array)
    const payload = {
      name: editingRuleSet.name,
      description: editingRuleSet.description,
      builtin: editingRuleSet.builtin || false,
      config: editingRuleSet.rules || [],
    };
    try {
      if (editingId) {
        await api.updateRecoveryRuleSet(editingId, payload);
        toast('RuleSet updated');
      } else {
        await api.createRecoveryRuleSet(payload);
        toast('RuleSet created');
      }
      refresh();
    } catch (e) {
      toast('Save failed: ' + e.message, true);
    }
    setEditingRuleSet(null);
    setEditingId(null);
  }

  async function deleteRuleSet(id) {
    try {
      await api.deleteRecoveryRuleSet(id);
      toast('RuleSet deleted');
      refresh();
    } catch (e) {
      toast('Delete failed: ' + e.message, true);
    }
  }

  // If editing, show the editor. Otherwise show the list.
  if (editingRuleSet) {
    return <RuleSetEditor
      ruleSet={editingRuleSet}
      onChange={setEditingRuleSet}
      onSave={saveRuleSet}
      onCancel={() => { setEditingRuleSet(null); setEditingId(null); }}
      isNew={!editingId}
    />;
  }

  return (
    <>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', padding: '12px 20px' }}>
        <span className="sm muted">Recovery rule sets define how the failover sidecar reacts to PostgreSQL log patterns. Assign a rule set to a profile to activate it.</span>
        <button className="btn btn-approve" onClick={startCreate}><Plus size={14} /> New RuleSet</button>
      </div>

      {ruleSets.length === 0 ? (
        <div className="empty-state" style={{ padding: '40px 20px' }}>
          <Shield size={48} strokeWidth={1.2} />
          <h3>No recovery rule sets</h3>
          <p>Create a rule set to enable log-based automatic recovery.</p>
        </div>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Description</th>
              <th>Rules</th>
              <th>Active</th>
              <th>Auto-Recovery</th>
              <th style={{ width: 240 }}>Actions</th>
            </tr>
          </thead>
          <tbody>
            {ruleSets.map(rs => {
              const enabled = rs.rules.filter(r => r.enabled).length;
              const autoRecovery = rs.rules.filter(r => r.enabled && r.action !== 'event').length;
              return (
                <tr key={rs.id}>
                  <td>
                    <span className="mono">{rs.name}</span>
                    {rs.builtin && <span className="badge" style={{ marginLeft: 6, fontSize: 9, padding: '1px 5px' }}>built-in</span>}
                  </td>
                  <td className="sm muted">{rs.description || '-'}</td>
                  <td>{rs.rules.length}</td>
                  <td><span className="badge badge-green" style={{ fontSize: 11 }}>{enabled}</span></td>
                  <td><span className={'badge ' + (autoRecovery > 0 ? 'badge-amber' : '')} style={{ fontSize: 11 }}>{autoRecovery}</span></td>
                  <td>
                    <div className="actions">
                      <button className="btn btn-sm" onClick={() => startEdit(rs)}><Pencil size={11} /> Edit</button>
                      <button className="btn btn-sm" onClick={() => cloneRuleSet(rs)}><Copy size={11} /> Clone</button>
                      {!rs.builtin && (
                        <button className="btn btn-sm btn-reject" onClick={() => deleteRuleSet(rs.id)}><Trash2 size={11} /> Delete</button>
                      )}
                    </div>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
    </>
  );
}

// ── RuleSet Editor ───────────────────────────────────────────────────────────

function RuleSetEditor({ ruleSet, onChange, onSave, onCancel, isNew }) {
  const [collapsedCategories, setCollapsedCategories] = useState({});
  const [editingRule, setEditingRule] = useState(null);
  const [editingRuleIndex, setEditingRuleIndex] = useState(-1);
  const [testInput, setTestInput] = useState('');
  const [testResult, setTestResult] = useState(null);
  const [patternError, setPatternError] = useState('');

  const rules = ruleSet.rules;
  const grouped = groupByCategory(rules);
  const enabledCount = rules.filter(r => r.enabled).length;
  const autoCount = rules.filter(r => r.enabled && r.action !== 'event').length;

  function setField(key, val) {
    onChange({ ...ruleSet, [key]: val });
  }

  function toggleCategory(cat) {
    setCollapsedCategories(prev => ({ ...prev, [cat]: !prev[cat] }));
  }

  function toggleRule(index) {
    const updated = [...rules];
    updated[index] = { ...updated[index], enabled: !updated[index].enabled };
    onChange({ ...ruleSet, rules: updated });
  }

  function startAddRule() {
    setEditingRule({ name: '', pattern: '', severity: 'warning', action: 'event', cooldown_seconds: 60, exec_command: '', enabled: true, builtin: false, category: 'Custom' });
    setEditingRuleIndex(-1);
    setPatternError('');
    setTestResult(null);
    setTestInput('');
  }

  function startEditRule(rule, index) {
    if (rule.builtin) return; // built-in rules cannot be edited, only cloned
    setEditingRule({ ...rule });
    setEditingRuleIndex(index);
    setPatternError('');
    setTestResult(null);
    setTestInput('');
  }

  function cloneRuleAsCustom(rule) {
    setEditingRule({ ...rule, name: rule.name + '-custom', builtin: false, category: 'Custom' });
    setEditingRuleIndex(-1);
    setPatternError('');
    setTestResult(null);
    setTestInput('');
  }

  function saveEditingRule() {
    if (!editingRule.name.trim() || !editingRule.pattern.trim()) return;
    try { new RegExp(editingRule.pattern); } catch (e) { setPatternError(e.message); return; }

    const updated = [...rules];
    if (editingRuleIndex >= 0) {
      updated[editingRuleIndex] = { ...editingRule };
    } else {
      updated.push({ ...editingRule });
    }
    onChange({ ...ruleSet, rules: updated });
    setEditingRule(null);
    setEditingRuleIndex(-1);
  }

  function deleteRule(index) {
    onChange({ ...ruleSet, rules: rules.filter((_, i) => i !== index) });
  }

  function testPattern() {
    if (!editingRule?.pattern || !testInput) { setTestResult(null); return; }
    try {
      const re = new RegExp(editingRule.pattern);
      setPatternError('');
      setTestResult(re.test(testInput));
    } catch (e) { setPatternError(e.message); setTestResult(null); }
  }

  return (
    <div style={{ padding: '0 20px 20px' }}>
      {/* Header bar */}
      <div className="card-head-bar" style={{ margin: '0 -20px', padding: '12px 20px' }}>
        <span className="card-head-title">{isNew ? 'New RuleSet' : 'Edit: ' + ruleSet.name}</span>
        <div className="actions">
          <button className="btn" onClick={onCancel}>Cancel</button>
          <button className="btn btn-approve" onClick={onSave}>{isNew ? 'Create' : 'Save'}</button>
        </div>
      </div>

      {/* Name & description */}
      <section className="form-section" style={{ marginTop: 16 }}>
        <h4>RuleSet</h4>
        <div className="form-grid">
          <div className="form-row">
            <label>Name</label>
            <input className="input" value={ruleSet.name} onChange={e => setField('name', e.target.value)} placeholder="e.g. production" disabled={ruleSet.builtin} />
          </div>
          <div className="form-row">
            <label>Description</label>
            <input className="input" value={ruleSet.description} onChange={e => setField('description', e.target.value)} placeholder="Optional description" />
          </div>
        </div>
      </section>

      {/* Rules */}
      <section className="form-section" style={{ marginTop: 16 }}>
        <h4>Rules</h4>
        <div style={{ display: 'flex', gap: 12, marginBottom: 14 }}>
          <span className="tag">{enabledCount} / {rules.length} enabled</span>
          <span className="tag">{autoCount} with auto-recovery</span>
          <button className="btn-link" onClick={startAddRule}><Plus size={12} /> Add Custom Rule</button>
        </div>

        {Object.entries(grouped).map(([category, catRules]) => {
          const collapsed = collapsedCategories[category];
          const enabledInCat = catRules.filter(r => r.enabled).length;

          return (
            <div className="pg-category" key={category}>
              <div className="pg-category-head" onClick={() => toggleCategory(category)}>
                <span className="pg-category-arrow">{collapsed ? '\u25b6' : '\u25bc'}</span>
                <span className="pg-category-title">{category}</span>
                <span className="muted sm">{catRules.length} rules</span>
                {enabledInCat > 0 && <span className="tab-badge">{enabledInCat} active</span>}
              </div>
              {!collapsed && (
                <div className="pg-category-body" style={{ padding: '6px 0 10px' }}>
                  <table className="hba-table" style={{ width: '100%', tableLayout: 'fixed' }}>
                    <thead>
                      <tr>
                        <th style={{ width: 32 }}></th>
                        <th style={{ width: 32 }}>#</th>
                        <th style={{ width: '18%' }}>Name</th>
                        <th>Pattern</th>
                        <th style={{ width: 66 }}>Severity</th>
                        <th style={{ width: 90 }}>Action</th>
                        <th style={{ width: 56 }}>CD</th>
                        <th style={{ width: 90 }}></th>
                      </tr>
                    </thead>
                    <tbody>
                      {catRules.map(rule => {
                        const gi = rules.indexOf(rule);
                        return (
                          <tr key={rule.name} style={{ opacity: rule.enabled ? 1 : 0.4 }}>
                            <td><input type="checkbox" checked={rule.enabled} onChange={() => toggleRule(gi)} /></td>
                            <td style={{ fontSize: 11, color: 'var(--text-secondary)' }}>{gi + 1}</td>
                            <td><span className="mono" style={{ fontSize: 12 }}>{rule.name}</span></td>
                            <td style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}><code style={{ fontSize: 11, color: 'var(--text-secondary)' }} title={rule.pattern}>{rule.pattern}</code></td>
                            <td style={{ whiteSpace: 'nowrap' }}><span className={'badge ' + (SEV_CLASS[rule.severity] || '')} style={{ fontSize: 10 }}>{rule.severity}</span></td>
                            <td style={{ whiteSpace: 'nowrap' }}><span className={'badge ' + (ACT_CLASS[rule.action] || '')} style={{ fontSize: 10 }}>{ACT_LABEL[rule.action] || rule.action}</span></td>
                            <td style={{ fontSize: 11, whiteSpace: 'nowrap' }}>{formatCooldown(rule.cooldown_seconds)}</td>
                            <td>
                              <div className="actions" style={{ display: 'flex', gap: 4 }}>
                                {rule.builtin ? (
                                  <button className="btn btn-sm" onClick={() => cloneRuleAsCustom(rule)} title="Clone as custom rule"><Copy size={11} /> Clone</button>
                                ) : (
                                  <>
                                    <button className="btn btn-sm" onClick={() => startEditRule(rule, gi)}><Pencil size={11} /> Edit</button>
                                    <button className="btn-icon" onClick={() => deleteRule(gi)}>&times;</button>
                                  </>
                                )}
                              </div>
                            </td>
                          </tr>
                        );
                      })}
                    </tbody>
                  </table>
                </div>
              )}
            </div>
          );
        })}
      </section>

      {/* ── Rule edit modal ─────────────────────────────────────────────────── */}
      {editingRule && (
        <div className="confirm-overlay" onClick={() => setEditingRule(null)}>
          <div className="confirm-modal" onClick={e => e.stopPropagation()}>
            <div className="confirm-header">
              <h3>{editingRuleIndex >= 0 ? 'Edit Rule' : 'New Custom Rule'}</h3>
              <button className="modal-close" onClick={() => setEditingRule(null)}><X size={18} /></button>
            </div>
            <div className="confirm-body">
              <div className="form-grid">
                <div className="form-row">
                  <label>Name</label>
                  <input className="input" value={editingRule.name}
                    onChange={e => setEditingRule(r => ({ ...r, name: e.target.value.replace(/[^a-z0-9-]/g, '') }))}
                    placeholder="e.g. custom-deadlock" disabled={editingRule.builtin} />
                </div>
                <div className="form-row">
                  <label>Severity</label>
                  <select className="input" value={editingRule.severity} onChange={e => setEditingRule(r => ({ ...r, severity: e.target.value }))}>
                    {SEVERITIES.map(s => <option key={s} value={s}>{s}</option>)}
                  </select>
                </div>
              </div>
              <div className="form-row" style={{ marginTop: 12 }}>
                <label>Pattern (Regex)</label>
                <input className={'input mono' + (patternError ? ' input-error' : '')} value={editingRule.pattern}
                  onChange={e => { setEditingRule(r => ({ ...r, pattern: e.target.value })); setPatternError(''); }}
                  placeholder="e.g. deadlock detected" style={{ fontSize: 12 }} />
                {patternError && <span className="form-error">Invalid regex: {patternError}</span>}
              </div>
              <div className="form-grid" style={{ marginTop: 12 }}>
                <div className="form-row">
                  <label>Action</label>
                  <select className="input" value={editingRule.action} onChange={e => setEditingRule(r => ({ ...r, action: e.target.value }))}>
                    {ACTIONS.map(a => <option key={a.value} value={a.value}>{a.label}</option>)}
                  </select>
                  <span className="muted" style={{ fontSize: 11 }}>{ACTION_DESCS[editingRule.action]}</span>
                </div>
                <div className="form-row">
                  <label>Cooldown (seconds)</label>
                  <input className="input" type="number" min="0" value={editingRule.cooldown_seconds}
                    onChange={e => setEditingRule(r => ({ ...r, cooldown_seconds: parseInt(e.target.value) || 0 }))} />
                </div>
              </div>
              {editingRule.action === 'exec' && (
                <div className="form-row" style={{ marginTop: 12 }}>
                  <label>Exec Command</label>
                  <input className="input mono" value={editingRule.exec_command || ''}
                    onChange={e => setEditingRule(r => ({ ...r, exec_command: e.target.value }))}
                    placeholder="e.g. pg_ctl reload -D $PGDATA" style={{ fontSize: 12 }} />
                </div>
              )}
              <div className="form-section" style={{ marginTop: 14, padding: 12 }}>
                <div className="form-row">
                  <label>Test Pattern</label>
                  <div style={{ display: 'flex', gap: 8 }}>
                    <input className="input mono" value={testInput}
                      onChange={e => { setTestInput(e.target.value); setTestResult(null); }}
                      placeholder="Paste a sample log line\u2026" style={{ flex: 1, fontSize: 11 }} />
                    <button className="btn" onClick={testPattern}>Test</button>
                  </div>
                  {testResult !== null && (
                    <div style={{ marginTop: 6 }}>
                      {testResult
                        ? <span className="badge badge-green" style={{ fontSize: 11 }}><span className="dot" /> Match</span>
                        : <span className="badge badge-red" style={{ fontSize: 11 }}><span className="dot" /> No match</span>}
                    </div>
                  )}
                </div>
              </div>
            </div>
            <div className="confirm-footer">
              <button className="btn" onClick={() => setEditingRule(null)}>Cancel</button>
              <button className="btn btn-approve" onClick={saveEditingRule}>{editingRuleIndex >= 0 ? 'Update Rule' : 'Add Rule'}</button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
