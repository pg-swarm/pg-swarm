const SAT_COLORS = {
  connected: 'green', approved: 'amber', pending: 'amber',
  disconnected: 'red', offline: 'red',
};

const CLUSTER_COLORS = {
  running: 'green', creating: 'amber', degraded: 'amber',
  paused: 'amber', failed: 'red', deleting: 'gray',
};

export function SatBadge({ state }) {
  return <Badge label={state} color={SAT_COLORS[state] || 'gray'} />;
}

export function ClusterBadge({ state }) {
  return <Badge label={state} color={CLUSTER_COLORS[state] || 'gray'} />;
}

function Badge({ label, color }) {
  return (
    <span className={`badge badge-${color}`}>
      <span className="dot" />
      {label}
    </span>
  );
}
