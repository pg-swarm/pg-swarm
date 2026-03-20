import { useEffect, useRef } from 'react';
import { Circle, CheckCircle, XCircle, Loader, RotateCcw, X, Shield, AlertTriangle, ArrowRight, ArrowUpRight, Crown, Play } from 'lucide-react';

const TOTAL_STEPS = 9;

const STEP_DEFS = [
  { step: 1, name: 'verify_target',  label: 'Verify target' },
  { step: 2, name: 'find_primary',   label: 'Discover primary' },
  { step: 3, name: 'check_status',   label: 'Check replica status' },
  { step: 4, name: 'fence_primary',  label: 'Fence primary' },
  { step: 5, name: 'checkpoint',     label: 'Checkpoint' },
  { step: 6, name: 'transfer_lease', label: 'Transfer lease' },
  { step: 7, name: 'promote',        label: 'Promote replica' },
  { step: 8, name: 'label_pods',     label: 'Label pods' },
  { step: 9, name: 'renew_lease',    label: 'Renew lease' },
];

const STATUS_CONF = {
  pending:      { icon: Circle,      color: 'var(--text-secondary)', spin: false },
  starting:     { icon: Loader,      color: 'var(--blue)',           spin: true },
  completed:    { icon: CheckCircle,  color: 'var(--green)',          spin: false },
  failed:       { icon: XCircle,     color: 'var(--red)',            spin: false },
  rolling_back: { icon: RotateCcw,   color: 'var(--amber)',          spin: true },
};

function fmtTime(ts) {
  if (!ts) return '';
  const d = typeof ts === 'string' ? new Date(ts) : ts;
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false });
}

// Build per-step state from the log array
function buildStepState(log) {
  const map = {};
  if (!Array.isArray(log)) return map;
  for (const entry of log) {
    if (!map[entry.step]) map[entry.step] = {};
    const s = map[entry.step];
    if (entry.status === 'starting') {
      s.startedAt = entry.timestamp;
      s.target_pod = entry.target_pod;
      s.ponr = entry.ponr;
    }
    if (entry.status === 'completed' || entry.status === 'failed' || entry.status === 'rolling_back') {
      s.endedAt = entry.timestamp;
      s.detail = entry.detail;
    }
    s.status = entry.status;
  }
  return map;
}

function StepRow({ def, state }) {
  const status = state?.status || 'pending';
  const conf = STATUS_CONF[status] || STATUS_CONF.pending;
  const Icon = conf.icon;

  return (
    <div className={`so-log-entry so-log-${status}`}>
      <Icon size={13} style={{ color: conf.color, flexShrink: 0 }} className={conf.spin ? 'so-spin' : ''} />
      <span className="so-log-label">{def.label}</span>
      {state?.target_pod && <span className="so-log-pod mono">{state.target_pod}</span>}
      {state?.ponr && status === 'starting' && (
        <span className="so-log-ponr"><AlertTriangle size={10} /> PONR</span>
      )}
      {status === 'starting' && (
        <span className="so-log-status-text">pending...</span>
      )}
      {status !== 'pending' && status !== 'starting' && state?.detail && (
        <span className="so-log-detail">
          <ArrowRight size={10} />
          {state.detail}
        </span>
      )}
      <span className="so-log-times mono">
        {state?.startedAt && fmtTime(state.startedAt)}
        {state?.endedAt && <> → {fmtTime(state.endedAt)}</>}
      </span>
    </div>
  );
}

export default function SwitchoverProgressModal({ operation, instances, onStart, onClose }) {
  if (!operation) return null;
  const logRef = useRef(null);

  const started = operation.started;
  const steps = operation.steps || {};
  const log = Array.isArray(operation.log) ? operation.log : [];
  const stepState = buildStepState(log);
  const completedCount = Object.values(steps).filter(s => s.status === 'completed').length;
  const failed = Object.values(steps).some(s => s.status === 'failed');
  const pct = operation.done && operation.success
    ? 100
    : Math.round((completedCount / TOTAL_STEPS) * 100);

  const ponrReached = steps[7] && (steps[7].status === 'starting' || steps[7].status === 'completed');

  // Auto-scroll to the active step
  useEffect(() => {
    if (logRef.current) {
      const active = logRef.current.querySelector('.so-log-starting');
      if (active) active.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
    }
  }, [log.length]);

  return (
    <div className="confirm-overlay" onClick={onClose}>
      <div className="confirm-modal" onClick={e => e.stopPropagation()} style={{ width: 580 }}>
        <div className="confirm-header">
          <h3>Switchover: {operation.cluster_name}</h3>
          <button className="modal-close" onClick={onClose}><X size={18} /></button>
        </div>
        <div className="confirm-body">

          {/* Pre-start view */}
          {!started && (
            <div className="so-prestart">
              <div className="so-prestart-flow">
                <div className="so-prestart-node">
                  <Crown size={16} style={{ color: 'var(--amber)' }} />
                  <span className="so-prestart-role">Primary</span>
                  <span className="mono so-prestart-pod">{operation.primary_pod || '...'}</span>
                  <span className="so-prestart-action">will demote to replica</span>
                </div>
                <div className="so-prestart-arrow">
                  <ArrowUpRight size={20} style={{ color: 'var(--text-secondary)' }} />
                </div>
                <div className="so-prestart-node">
                  <ArrowUpRight size={16} style={{ color: 'var(--green)' }} />
                  <span className="so-prestart-role">New Primary</span>
                  <span className="mono so-prestart-pod">{operation.target_pod}</span>
                  <span className="so-prestart-action">will be promoted</span>
                </div>
              </div>
              <div className="so-prestart-warning">
                <AlertTriangle size={14} />
                <span>Applications will experience a brief downtime during switchover.</span>
              </div>
            </div>
          )}

          {/* Progress view */}
          {started && (
            <>
              <div className="so-progress-track" style={{ marginTop: 0, paddingTop: 0, borderTop: 'none' }}>
                <div className="so-progress-bar">
                  <div
                    className={'so-progress-fill' + (failed ? ' so-progress-fill-error' : '')}
                    style={{ width: pct + '%' }}
                  />
                </div>
                <div className="so-progress-label">
                  <span>{completedCount} of {TOTAL_STEPS} steps</span>
                  <span className={`so-ponr-badge ${ponrReached ? 'so-ponr-passed' : 'so-ponr-safe'}`}>
                    {ponrReached
                      ? <><AlertTriangle size={11} /> Point of no return</>
                      : <><Shield size={11} /> Rollback available</>
                    }
                  </span>
                </div>
              </div>

              <div className="so-log-scroll" ref={logRef}>
                {STEP_DEFS.map(def => (
                  <StepRow key={def.step} def={def} state={stepState[def.step]} />
                ))}
              </div>

              {operation.done && operation.success && (
                <div className="so-result-banner so-result-success">
                  <CheckCircle size={16} /> Switchover complete
                </div>
              )}
              {operation.done && !operation.success && (
                <div className="so-result-banner so-result-failure">
                  <XCircle size={16} /> {operation.error || 'Switchover failed'}
                </div>
              )}
            </>
          )}
        </div>

        <div className="confirm-footer">
          {!started && (
            <>
              <button className="btn-sm" onClick={onClose}>Cancel</button>
              <button className="btn-sm btn-danger" onClick={onStart}>
                <Play size={12} /> Start Switchover
              </button>
            </>
          )}
          {started && operation.done && (
            <button className="btn-sm" onClick={onClose}>Close</button>
          )}
        </div>
      </div>
    </div>
  );
}
