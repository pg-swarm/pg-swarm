import {
  CheckCircle2, AlertCircle, XCircle, Clock, Pause, Loader, Trash2,
  Wifi, WifiOff
} from 'lucide-react';

const SAT_ICONS = {
  connected: CheckCircle2, approved: Clock, pending: Clock,
  disconnected: WifiOff, offline: XCircle,
};
const SAT_COLORS = {
  connected: 'green', approved: 'amber', pending: 'amber',
  disconnected: 'red', offline: 'red',
};

const CLUSTER_ICONS = {
  running: CheckCircle2, creating: Loader, degraded: AlertCircle,
  paused: Pause, failed: XCircle, deleting: Trash2,
};
const CLUSTER_COLORS = {
  running: 'green', creating: 'amber', degraded: 'amber',
  paused: 'amber', failed: 'red', deleting: 'gray',
};

export function SatBadge({ state }) {
  const Icon = SAT_ICONS[state] || AlertCircle;
  return <Badge label={state} color={SAT_COLORS[state] || 'gray'} icon={Icon} />;
}

export function ClusterBadge({ state }) {
  const Icon = CLUSTER_ICONS[state] || AlertCircle;
  return <Badge label={state} color={CLUSTER_COLORS[state] || 'gray'} icon={Icon} />;
}

function Badge({ label, color, icon: Icon }) {
  return (
    <span className={`badge badge-${color}`}>
      <Icon size={11} />
      {label}
    </span>
  );
}
