// Mock data for local dashboard testing without the Go backend.
// Timestamps are generated relative to "now" so everything looks fresh.

function ago(seconds) {
  return new Date(Date.now() - seconds * 1000).toISOString();
}

function uuid() {
  return 'xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx'.replace(/[xy]/g, (c) => {
    const r = (Math.random() * 16) | 0;
    return (c === 'x' ? r : (r & 0x3) | 0x8).toString(16);
  });
}

// --- Satellites -----------------------------------------------------------

const SAT_IDS = {
  edge1: 'sat-a1b2c3d4-0001-4000-8000-000000000001',
  edge2: 'sat-a1b2c3d4-0002-4000-8000-000000000002',
  edge3: 'sat-a1b2c3d4-0003-4000-8000-000000000003',
  edge4: 'sat-a1b2c3d4-0004-4000-8000-000000000004',
  edge5: 'sat-a1b2c3d4-0005-4000-8000-000000000005',
};

export const satellites = [
  {
    id: SAT_IDS.edge1,
    hostname: 'edge-node-india-south-1a',
    k8s_cluster_name: 'edge-india-south',
    region: 'india-south-1',
    state: 'connected',
    last_heartbeat: ago(5),
    labels: { region: 'india-south-1', tier: 'production', rack: 'r1' },
    storage_classes: [
      { name: 'gp3', provisioner: 'ebs.csi.aws.com', reclaim_policy: 'Delete', volume_binding_mode: 'WaitForFirstConsumer', is_default: true },
      { name: 'io2', provisioner: 'ebs.csi.aws.com', reclaim_policy: 'Delete', volume_binding_mode: 'WaitForFirstConsumer', is_default: false },
      { name: 'local-nvme', provisioner: 'kubernetes.io/no-provisioner', reclaim_policy: 'Retain', volume_binding_mode: 'WaitForFirstConsumer', is_default: false },
    ],
    tier_mappings: { fast: 'local-nvme', standard: 'gp3', replicated: 'io2' },
    created_at: ago(86400 * 30),
    updated_at: ago(5),
  },
  {
    id: SAT_IDS.edge2,
    hostname: 'edge-node-india-north-1b',
    k8s_cluster_name: 'edge-india-north',
    region: 'india-north-1',
    state: 'connected',
    last_heartbeat: ago(8),
    labels: { region: 'india-north-1', tier: 'production', rack: 'r2' },
    storage_classes: [
      { name: 'gp3', provisioner: 'ebs.csi.aws.com', reclaim_policy: 'Delete', volume_binding_mode: 'WaitForFirstConsumer', is_default: true },
      { name: 'standard', provisioner: 'kubernetes.io/aws-ebs', reclaim_policy: 'Delete', volume_binding_mode: 'Immediate', is_default: false },
    ],
    tier_mappings: { fast: 'gp3', standard: 'standard' },
    created_at: ago(86400 * 25),
    updated_at: ago(8),
  },
  {
    id: SAT_IDS.edge3,
    hostname: 'edge-node-india-west-1a',
    k8s_cluster_name: 'edge-india-west',
    region: 'india-west-1',
    state: 'connected',
    last_heartbeat: ago(12),
    labels: { region: 'india-west-1', tier: 'staging' },
    storage_classes: [
      { name: 'gp3', provisioner: 'ebs.csi.aws.com', reclaim_policy: 'Delete', volume_binding_mode: 'WaitForFirstConsumer', is_default: true },
    ],
    tier_mappings: { standard: 'gp3' },
    created_at: ago(86400 * 15),
    updated_at: ago(12),
  },
  {
    id: SAT_IDS.edge4,
    hostname: 'edge-node-india-east-1c',
    k8s_cluster_name: 'edge-india-east',
    region: 'india-east-1',
    state: 'offline',
    last_heartbeat: ago(300),
    labels: { region: 'india-east-1', tier: 'production' },
    storage_classes: [
      { name: 'gp3', provisioner: 'ebs.csi.aws.com', reclaim_policy: 'Delete', volume_binding_mode: 'WaitForFirstConsumer', is_default: true },
      { name: 'io1', provisioner: 'ebs.csi.aws.com', reclaim_policy: 'Delete', volume_binding_mode: 'WaitForFirstConsumer', is_default: false },
    ],
    tier_mappings: { fast: 'io1', standard: 'gp3' },
    created_at: ago(86400 * 20),
    updated_at: ago(300),
  },
  {
    id: SAT_IDS.edge5,
    hostname: 'edge-node-india-central-1a',
    k8s_cluster_name: 'edge-india-central',
    region: 'india-central-1',
    state: 'pending',
    last_heartbeat: null,
    labels: {},
    storage_classes: [],
    tier_mappings: {},
    created_at: ago(120),
    updated_at: ago(120),
  },
];

// --- Profiles -------------------------------------------------------------

const PROFILE_IDS = {
  prod: 'prof-00000001-0000-4000-8000-000000000001',
  staging: 'prof-00000002-0000-4000-8000-000000000002',
  minimal: 'prof-00000003-0000-4000-8000-000000000003',
};

export const profiles = [
  {
    id: PROFILE_IDS.prod,
    name: 'production-ha',
    locked: false,
    recovery_rule_set_id: 'rs-prod-strict',
    config: JSON.stringify({
      postgres: { version: '17', image: 'postgres:17-alpine' },
      storage: { size: '50Gi', storage_class: 'tier:fast' },
      wal_storage: { size: '10Gi', storage_class: 'tier:fast' },
      replicas: 3,
      resources: { cpu_request: '500m', cpu_limit: '2', memory_request: '1Gi', memory_limit: '4Gi' },
      failover: { enabled: true, health_check_interval_seconds: 5 },
      archive: { mode: 'custom', archive_command: 'aws s3 cp %p s3://backups/wal/%f', restore_command: 'aws s3 cp s3://backups/wal/%f %p' },
      pg_params: { shared_buffers: '1GB', work_mem: '64MB', max_connections: '200' },
      databases: [
        { name: 'appdb', user: 'app_user', password: '***' },
        { name: 'analytics', user: 'analyst', password: '***' },
      ],
    }),
    created_at: ago(86400 * 30),
    updated_at: ago(86400 * 2),
  },
  {
    id: PROFILE_IDS.staging,
    name: 'staging',
    locked: false,
    recovery_rule_set_id: 'rs-default',
    config: JSON.stringify({
      postgres: { version: '17', image: 'postgres:17-alpine' },
      storage: { size: '20Gi' },
      replicas: 2,
      resources: { cpu_request: '250m', cpu_limit: '1', memory_request: '512Mi', memory_limit: '2Gi' },
      failover: { enabled: true },
      pg_params: { shared_buffers: '256MB' },
      databases: [{ name: 'staging_app', user: 'app_user', password: '***' }],
    }),
    created_at: ago(86400 * 20),
    updated_at: ago(86400 * 5),
  },
  {
    id: PROFILE_IDS.minimal,
    name: 'dev-single',
    locked: false,
    config: JSON.stringify({
      postgres: { version: '16', image: 'postgres:16' },
      storage: { size: '5Gi' },
      replicas: 1,
      resources: {},
      failover: { enabled: false },
    }),
    created_at: ago(86400 * 10),
    updated_at: ago(86400 * 10),
  },
];

// --- Deployment Rules -----------------------------------------------------

const RULE_IDS = {
  prodUs: 'rule-00000001-0000-4000-8000-000000000001',
  prodEu: 'rule-00000002-0000-4000-8000-000000000002',
  staging: 'rule-00000003-0000-4000-8000-000000000003',
};

export const deploymentRules = [
  {
    id: RULE_IDS.prodUs,
    name: 'prod-india-south',
    namespace: 'pg-clusters',
    cluster_name: 'orders-db',
    profile_id: PROFILE_IDS.prod,
    label_selector: { region: 'india-south-1', tier: 'production' },
    created_at: ago(86400 * 28),
    updated_at: ago(86400 * 2),
  },
  {
    id: RULE_IDS.prodEu,
    name: 'prod-india-north',
    namespace: 'pg-clusters',
    cluster_name: 'orders-db',
    profile_id: PROFILE_IDS.prod,
    label_selector: { region: 'india-north-1', tier: 'production' },
    created_at: ago(86400 * 25),
    updated_at: ago(86400 * 3),
  },
  {
    id: RULE_IDS.staging,
    name: 'staging-india-west',
    namespace: 'staging',
    cluster_name: 'staging-db',
    profile_id: PROFILE_IDS.staging,
    label_selector: { region: 'india-west-1', tier: 'staging' },
    created_at: ago(86400 * 14),
    updated_at: ago(86400 * 7),
  },
];

// --- Clusters -------------------------------------------------------------

const CLUSTER_IDS = {
  ordersUsEast: 'clus-00000001-0000-4000-8000-000000000001',
  ordersEuWest: 'clus-00000002-0000-4000-8000-000000000002',
  stagingAp:    'clus-00000003-0000-4000-8000-000000000003',
  ordersUsWest: 'clus-00000004-0000-4000-8000-000000000004',
  analyticsUs:  'clus-00000005-0000-4000-8000-000000000005',
  paymentsUs:   'clus-00000006-0000-4000-8000-000000000006',
  paymentsEu:   'clus-00000007-0000-4000-8000-000000000007',
  inventoryUs:  'clus-00000008-0000-4000-8000-000000000008',
  inventoryAp:  'clus-00000009-0000-4000-8000-000000000009',
  sessionsUs:   'clus-0000000a-0000-4000-8000-000000000010',
  sessionsEu:   'clus-0000000b-0000-4000-8000-000000000011',
  auditUs:      'clus-0000000c-0000-4000-8000-000000000012',
  cacheUs:      'clus-0000000d-0000-4000-8000-000000000013',
  cacheEu:      'clus-0000000e-0000-4000-8000-000000000014',
  metricsUs:    'clus-0000000f-0000-4000-8000-000000000015',
  notifyUs:     'clus-00000010-0000-4000-8000-000000000016',
  searchAp:     'clus-00000011-0000-4000-8000-000000000017',
};

export const clusters = [
  {
    id: CLUSTER_IDS.ordersUsEast,
    name: 'orders-db',
    namespace: 'pg-clusters',
    satellite_id: SAT_IDS.edge1,
    state: 'running',
    paused: false,
    config: profiles[0].config,
    deployment_rule_id: RULE_IDS.prodUs,
    config_version: 3,
    created_at: ago(86400 * 28),
    updated_at: ago(60),
  },
  {
    id: CLUSTER_IDS.ordersEuWest,
    name: 'orders-db',
    namespace: 'pg-clusters',
    satellite_id: SAT_IDS.edge2,
    state: 'running',
    paused: false,
    config: profiles[0].config,
    deployment_rule_id: RULE_IDS.prodEu,
    config_version: 3,
    created_at: ago(86400 * 25),
    updated_at: ago(45),
  },
  {
    id: CLUSTER_IDS.stagingAp,
    name: 'staging-db',
    namespace: 'staging',
    satellite_id: SAT_IDS.edge3,
    state: 'running',
    paused: false,
    config: profiles[1].config,
    deployment_rule_id: RULE_IDS.staging,
    config_version: 1,
    created_at: ago(86400 * 14),
    updated_at: ago(90),
  },
  {
    id: CLUSTER_IDS.ordersUsWest,
    name: 'orders-db',
    namespace: 'pg-clusters',
    satellite_id: SAT_IDS.edge4,
    state: 'degraded',
    paused: false,
    config: profiles[0].config,
    deployment_rule_id: null,
    config_version: 2,
    created_at: ago(86400 * 20),
    updated_at: ago(300),
  },
  {
    id: CLUSTER_IDS.analyticsUs,
    name: 'analytics-db',
    namespace: 'pg-clusters',
    satellite_id: SAT_IDS.edge1,
    state: 'creating',
    paused: false,
    config: profiles[2].config,
    deployment_rule_id: null,
    config_version: 1,
    created_at: ago(180),
    updated_at: ago(60),
  },
  {
    id: CLUSTER_IDS.paymentsUs,
    name: 'payments-db',
    namespace: 'pg-clusters',
    satellite_id: SAT_IDS.edge1,
    state: 'running',
    paused: false,
    config: profiles[0].config,
    deployment_rule_id: RULE_IDS.prodUs,
    config_version: 2,
    created_at: ago(86400 * 22),
    updated_at: ago(30),
  },
  {
    id: CLUSTER_IDS.paymentsEu,
    name: 'payments-db',
    namespace: 'pg-clusters',
    satellite_id: SAT_IDS.edge2,
    state: 'running',
    paused: false,
    config: profiles[0].config,
    deployment_rule_id: RULE_IDS.prodEu,
    config_version: 2,
    created_at: ago(86400 * 22),
    updated_at: ago(35),
  },
  {
    id: CLUSTER_IDS.inventoryUs,
    name: 'inventory-db',
    namespace: 'pg-clusters',
    satellite_id: SAT_IDS.edge1,
    state: 'running',
    paused: false,
    config: profiles[0].config,
    deployment_rule_id: RULE_IDS.prodUs,
    config_version: 1,
    created_at: ago(86400 * 18),
    updated_at: ago(50),
  },
  {
    id: CLUSTER_IDS.inventoryAp,
    name: 'inventory-db',
    namespace: 'pg-clusters',
    satellite_id: SAT_IDS.edge3,
    state: 'running',
    paused: false,
    config: profiles[1].config,
    deployment_rule_id: RULE_IDS.staging,
    config_version: 1,
    created_at: ago(86400 * 12),
    updated_at: ago(70),
  },
  {
    id: CLUSTER_IDS.sessionsUs,
    name: 'sessions-db',
    namespace: 'pg-clusters',
    satellite_id: SAT_IDS.edge1,
    state: 'running',
    paused: false,
    config: profiles[0].config,
    deployment_rule_id: RULE_IDS.prodUs,
    config_version: 1,
    created_at: ago(86400 * 16),
    updated_at: ago(20),
  },
  {
    id: CLUSTER_IDS.sessionsEu,
    name: 'sessions-db',
    namespace: 'pg-clusters',
    satellite_id: SAT_IDS.edge2,
    state: 'running',
    paused: true,
    config: profiles[0].config,
    deployment_rule_id: RULE_IDS.prodEu,
    config_version: 1,
    created_at: ago(86400 * 16),
    updated_at: ago(3600),
  },
  {
    id: CLUSTER_IDS.auditUs,
    name: 'audit-db',
    namespace: 'compliance',
    satellite_id: SAT_IDS.edge1,
    state: 'running',
    paused: false,
    config: profiles[0].config,
    deployment_rule_id: null,
    config_version: 1,
    created_at: ago(86400 * 10),
    updated_at: ago(40),
  },
  {
    id: CLUSTER_IDS.cacheUs,
    name: 'cache-db',
    namespace: 'pg-clusters',
    satellite_id: SAT_IDS.edge1,
    state: 'running',
    paused: false,
    config: profiles[2].config,
    deployment_rule_id: null,
    config_version: 1,
    created_at: ago(86400 * 8),
    updated_at: ago(55),
  },
  {
    id: CLUSTER_IDS.cacheEu,
    name: 'cache-db',
    namespace: 'pg-clusters',
    satellite_id: SAT_IDS.edge2,
    state: 'degraded',
    paused: false,
    config: profiles[2].config,
    deployment_rule_id: null,
    config_version: 1,
    created_at: ago(86400 * 8),
    updated_at: ago(200),
  },
  {
    id: CLUSTER_IDS.metricsUs,
    name: 'metrics-db',
    namespace: 'monitoring',
    satellite_id: SAT_IDS.edge1,
    state: 'running',
    paused: false,
    config: profiles[1].config,
    deployment_rule_id: null,
    config_version: 1,
    created_at: ago(86400 * 6),
    updated_at: ago(25),
  },
  {
    id: CLUSTER_IDS.notifyUs,
    name: 'notify-db',
    namespace: 'pg-clusters',
    satellite_id: SAT_IDS.edge1,
    state: 'running',
    paused: false,
    config: profiles[2].config,
    deployment_rule_id: null,
    config_version: 1,
    created_at: ago(86400 * 4),
    updated_at: ago(65),
  },
  {
    id: CLUSTER_IDS.searchAp,
    name: 'search-db',
    namespace: 'pg-clusters',
    satellite_id: SAT_IDS.edge3,
    state: 'running',
    paused: false,
    config: profiles[1].config,
    deployment_rule_id: RULE_IDS.staging,
    config_version: 1,
    created_at: ago(86400 * 3),
    updated_at: ago(80),
  },
];

// --- Health ---------------------------------------------------------------

function instance(podName, role, opts = {}) {
  return {
    pod_name: podName,
    role,
    ready: opts.ready !== false,
    pg_start_time: opts.pg_start_time || ago(86400 * 7),
    error_message: opts.error || '',
    replication_lag_seconds: opts.lag_s || 0,
    replication_lag_bytes: opts.lag_b || 0,
    wal_receiver_active: role === 'replica' ? (opts.wal_active !== false) : false,
    timeline_id: opts.timeline || 1,
    connections_used: opts.conn_used || 42,
    connections_max: opts.conn_max || 200,
    connections_active: opts.conn_active || 18,
    disk_used_bytes: opts.disk || 8_500_000_000,
    wal_disk_bytes: opts.wal_disk || 350_000_000,
    index_hit_ratio: opts.idx_hit || 0.9985,
    txn_commit_ratio: opts.txn_ratio || 0.998,
    database_stats: opts.db_stats || [
      { database_name: 'appdb', size_bytes: 5_200_000_000, cache_hit_ratio: 0.9991 },
      { database_name: 'analytics', size_bytes: 2_800_000_000, cache_hit_ratio: 0.9872 },
    ],
    table_stats: opts.table_stats || [
      { schema_name: 'public', table_name: 'orders', table_size_bytes: 1_800_000_000, index_size_bytes: 420_000_000, seq_scan: 150, idx_scan: 98000, n_live_tup: 5_200_000 },
      { schema_name: 'public', table_name: 'users', table_size_bytes: 320_000_000, index_size_bytes: 90_000_000, seq_scan: 12, idx_scan: 250000, n_live_tup: 980_000 },
      { schema_name: 'public', table_name: 'sessions', table_size_bytes: 750_000_000, index_size_bytes: 180_000_000, seq_scan: 5, idx_scan: 180000, n_live_tup: 2_100_000 },
    ],
    slow_queries: opts.slow_queries || [
      { query: 'SELECT o.*, u.email FROM orders o JOIN users u ON o.user_id = u.id WHERE o.created_at > $1 ORDER BY o.total DESC LIMIT $2', database_name: 'appdb', calls: 12500, mean_exec_time_ms: 45.2, total_exec_time_ms: 565000 },
      { query: 'SELECT count(*) FROM sessions WHERE last_active > now() - interval \'1 hour\'', database_name: 'appdb', calls: 86400, mean_exec_time_ms: 8.1, total_exec_time_ms: 699840 },
    ],
  };
}

export const health = [
  {
    cluster_name: 'orders-db',
    satellite_id: SAT_IDS.edge1,
    state: 'running',
    instances: [
      instance('orders-db-0', 'primary', { conn_used: 160, conn_active: 95, disk: 42_000_000_000, wal_disk: 520_000_000 }),
      instance('orders-db-1', 'replica', { lag_s: 0.02, lag_b: 16384, disk: 30_000_000_000 }),
      instance('orders-db-2', 'replica', { lag_s: 0.05, lag_b: 32768, disk: 12_320_000_000 }),
    ],
  },
  {
    cluster_name: 'orders-db',
    satellite_id: SAT_IDS.edge2,
    state: 'running',
    instances: [
      instance('orders-db-0', 'primary', { conn_used: 120, conn_active: 55, disk: 35_000_000_000 }),
      instance('orders-db-1', 'replica', { lag_s: 0.08, lag_b: 65536, disk: 28_000_000_000, conn_used: 110, conn_active: 48 }),
      instance('orders-db-2', 'replica', { lag_s: 0.03, lag_b: 24576, disk: 11_180_000_000 }),
    ],
  },
  {
    cluster_name: 'staging-db',
    satellite_id: SAT_IDS.edge3,
    state: 'running',
    instances: [
      instance('staging-db-0', 'primary', {
        conn_used: 15, conn_active: 6, disk: 3_200_000_000, conn_max: 100,
        db_stats: [{ database_name: 'staging_app', size_bytes: 2_800_000_000, cache_hit_ratio: 0.972 }],
        table_stats: [
          { schema_name: 'public', table_name: 'orders', table_size_bytes: 900_000_000, index_size_bytes: 200_000_000, seq_scan: 80, idx_scan: 45000, n_live_tup: 2_100_000 },
        ],
        slow_queries: [],
      }),
      instance('staging-db-1', 'replica', {
        lag_s: 0.1, lag_b: 81920, disk: 3_180_000_000, conn_max: 100, conn_used: 5, conn_active: 2,
        db_stats: [{ database_name: 'staging_app', size_bytes: 2_800_000_000, cache_hit_ratio: 0.968 }],
        table_stats: [],
        slow_queries: [],
      }),
    ],
  },
  {
    cluster_name: 'orders-db',
    satellite_id: SAT_IDS.edge4,
    state: 'degraded',
    instances: [
      instance('orders-db-0', 'primary', { conn_used: 185, conn_active: 102, disk: 45_000_000_000 }),
      instance('orders-db-1', 'replica', { lag_s: 45.2, lag_b: 52_428_800, disk: 40_000_000_000, idx_hit: 0.95, conn_used: 155, conn_active: 78 }),
      instance('orders-db-2', 'failed_primary', {
        ready: false,
        error: 'FATAL: could not access status of transaction 847293',
        lag_s: 0, lag_b: 0, wal_active: false, disk: 10_500_000_000,
      }),
    ],
  },
  {
    cluster_name: 'analytics-db',
    satellite_id: SAT_IDS.edge1,
    state: 'creating',
    instances: [],
  },
  // payments-db us-east: 5 nodes
  {
    cluster_name: 'payments-db',
    satellite_id: SAT_IDS.edge1,
    state: 'running',
    instances: [
      instance('payments-db-0', 'primary', { conn_used: 90, conn_active: 45, disk: 18_000_000_000 }),
      instance('payments-db-1', 'replica', { lag_s: 0.01, lag_b: 8192, disk: 17_800_000_000 }),
      instance('payments-db-2', 'replica', { lag_s: 0.03, lag_b: 24576, disk: 17_900_000_000 }),
      instance('payments-db-3', 'replica', { lag_s: 0.02, lag_b: 16384, disk: 28_000_000_000, conn_used: 110, conn_active: 52 }),
      instance('payments-db-4', 'replica', { lag_s: 0.05, lag_b: 40960, disk: 17_600_000_000, conn_used: 35 }),
    ],
  },
  // payments-db eu-west: 3 nodes, one replica amber disk
  {
    cluster_name: 'payments-db',
    satellite_id: SAT_IDS.edge2,
    state: 'running',
    instances: [
      instance('payments-db-0', 'primary', { conn_used: 70, conn_active: 35, disk: 15_000_000_000 }),
      instance('payments-db-1', 'replica', { lag_s: 0.04, lag_b: 32768, disk: 30_000_000_000 }),
      instance('payments-db-2', 'replica', { lag_s: 0.02, lag_b: 16384, disk: 14_500_000_000 }),
    ],
  },
  // inventory-db us-east: 5 nodes (scrollable), mixed health
  {
    cluster_name: 'inventory-db',
    satellite_id: SAT_IDS.edge1,
    state: 'running',
    instances: [
      instance('inventory-db-0', 'primary', { conn_used: 140, conn_active: 72, disk: 38_000_000_000 }),
      instance('inventory-db-1', 'replica', { lag_s: 0.02, lag_b: 16384, disk: 37_500_000_000, conn_used: 60 }),
      instance('inventory-db-2', 'replica', { lag_s: 0.06, lag_b: 49152, disk: 37_800_000_000, conn_used: 55 }),
      instance('inventory-db-3', 'replica', { lag_s: 1.2, lag_b: 524288, disk: 42_000_000_000, conn_used: 130, conn_active: 68 }),
      instance('inventory-db-4', 'replica', { lag_s: 0.04, lag_b: 32768, disk: 20_000_000_000, conn_used: 45 }),
    ],
  },
  // inventory-db ap-south: 2 nodes
  {
    cluster_name: 'inventory-db',
    satellite_id: SAT_IDS.edge3,
    state: 'running',
    instances: [
      instance('inventory-db-0', 'primary', { conn_used: 30, conn_active: 12, disk: 8_000_000_000, conn_max: 100 }),
      instance('inventory-db-1', 'replica', { lag_s: 0.1, lag_b: 81920, disk: 7_800_000_000, conn_max: 100, conn_used: 20 }),
    ],
  },
  // sessions-db us-east: 7 nodes, high connections on some
  {
    cluster_name: 'sessions-db',
    satellite_id: SAT_IDS.edge1,
    state: 'running',
    instances: [
      instance('sessions-db-0', 'primary', { conn_used: 175, conn_active: 110, disk: 25_000_000_000 }),
      instance('sessions-db-1', 'replica', { lag_s: 0.01, lag_b: 8192, disk: 24_500_000_000, conn_used: 160, conn_active: 95 }),
      instance('sessions-db-2', 'replica', { lag_s: 0.02, lag_b: 16384, disk: 24_800_000_000, conn_used: 50 }),
      instance('sessions-db-3', 'replica', { lag_s: 0.04, lag_b: 32768, disk: 24_200_000_000, conn_used: 145, conn_active: 80 }),
      instance('sessions-db-4', 'replica', { lag_s: 0.01, lag_b: 8192, disk: 24_600_000_000, conn_used: 40 }),
      instance('sessions-db-5', 'replica', { lag_s: 0.08, lag_b: 65536, disk: 24_900_000_000, conn_used: 55 }),
      instance('sessions-db-6', 'replica', { lag_s: 0.03, lag_b: 24576, disk: 24_100_000_000, conn_used: 48 }),
    ],
  },
  // sessions-db eu-west: paused, 3 nodes
  {
    cluster_name: 'sessions-db',
    satellite_id: SAT_IDS.edge2,
    state: 'paused',
    instances: [
      instance('sessions-db-0', 'primary', { conn_used: 5, conn_active: 1, disk: 22_000_000_000 }),
      instance('sessions-db-1', 'replica', { lag_s: 0, lag_b: 0, disk: 21_800_000_000, conn_used: 2 }),
      instance('sessions-db-2', 'replica', { lag_s: 0, lag_b: 0, disk: 21_900_000_000, conn_used: 2 }),
    ],
  },
  // audit-db: 5 nodes, disk filling up
  {
    cluster_name: 'audit-db',
    satellite_id: SAT_IDS.edge1,
    state: 'running',
    instances: [
      instance('audit-db-0', 'primary', { conn_used: 35, conn_active: 15, disk: 44_000_000_000 }),
      instance('audit-db-1', 'replica', { lag_s: 0.5, lag_b: 262144, disk: 43_500_000_000, conn_used: 20 }),
      instance('audit-db-2', 'replica', { lag_s: 0.3, lag_b: 196608, disk: 43_800_000_000, conn_used: 18 }),
      instance('audit-db-3', 'replica', { lag_s: 0.1, lag_b: 81920, disk: 42_000_000_000, conn_used: 22 }),
      instance('audit-db-4', 'replica', { lag_s: 0.8, lag_b: 393216, disk: 44_500_000_000, conn_used: 15 }),
    ],
  },
  // cache-db us-east: 1 node (standalone)
  {
    cluster_name: 'cache-db',
    satellite_id: SAT_IDS.edge1,
    state: 'running',
    instances: [
      instance('cache-db-0', 'primary', { conn_used: 80, conn_active: 40, disk: 2_500_000_000, conn_max: 100 }),
    ],
  },
  // cache-db eu-west: 1 node, degraded
  {
    cluster_name: 'cache-db',
    satellite_id: SAT_IDS.edge2,
    state: 'degraded',
    instances: [
      instance('cache-db-0', 'primary', { conn_used: 95, conn_active: 60, disk: 4_200_000_000, conn_max: 100, error: 'FATAL: too many connections for role "app_user"' }),
    ],
  },
  // metrics-db: 2 nodes
  {
    cluster_name: 'metrics-db',
    satellite_id: SAT_IDS.edge1,
    state: 'running',
    instances: [
      instance('metrics-db-0', 'primary', { conn_used: 45, conn_active: 22, disk: 12_000_000_000, conn_max: 100 }),
      instance('metrics-db-1', 'replica', { lag_s: 0.15, lag_b: 131072, disk: 11_800_000_000, conn_max: 100, conn_used: 30 }),
    ],
  },
  // notify-db: 1 node
  {
    cluster_name: 'notify-db',
    satellite_id: SAT_IDS.edge1,
    state: 'running',
    instances: [
      instance('notify-db-0', 'primary', { conn_used: 25, conn_active: 8, disk: 1_200_000_000, conn_max: 100 }),
    ],
  },
  // search-db ap-south: 2 nodes, high lag
  {
    cluster_name: 'search-db',
    satellite_id: SAT_IDS.edge3,
    state: 'running',
    instances: [
      instance('search-db-0', 'primary', { conn_used: 55, conn_active: 28, disk: 9_000_000_000, conn_max: 100 }),
      instance('search-db-1', 'replica', { lag_s: 120, lag_b: 8_388_608, disk: 8_500_000_000, conn_max: 100, conn_used: 40 }),
    ],
  },
];

// --- Events ---------------------------------------------------------------

export const events = [
  { id: uuid(), cluster_name: 'analytics-db', satellite_id: SAT_IDS.edge1, severity: 'info', message: 'Cluster creation initiated', created_at: ago(180) },
  { id: uuid(), cluster_name: 'orders-db', satellite_id: SAT_IDS.edge4, severity: 'error', message: 'Instance orders-db-2 entered failed_primary state: FATAL: could not access status of transaction 847293', created_at: ago(295) },
  { id: uuid(), cluster_name: 'orders-db', satellite_id: SAT_IDS.edge4, severity: 'warning', message: 'Replication lag on orders-db-1 exceeded 30s (current: 45.2s)', created_at: ago(310) },
  { id: uuid(), cluster_name: 'orders-db', satellite_id: SAT_IDS.edge4, severity: 'critical', message: 'Satellite edge-us-west heartbeat lost, cluster health unknown', created_at: ago(300) },
  { id: uuid(), cluster_name: 'orders-db', satellite_id: SAT_IDS.edge1, severity: 'info', message: 'Automatic switchover completed: orders-db-1 promoted to primary', created_at: ago(3600) },
  { id: uuid(), cluster_name: 'orders-db', satellite_id: SAT_IDS.edge1, severity: 'warning', message: 'Connection pool utilization at 85% (170/200)', created_at: ago(3700) },
  { id: uuid(), cluster_name: 'orders-db', satellite_id: SAT_IDS.edge2, severity: 'info', message: 'Config version updated from 2 to 3, rolling restart initiated', created_at: ago(7200) },
  { id: uuid(), cluster_name: 'staging-db', satellite_id: SAT_IDS.edge3, severity: 'info', message: 'Base backup completed successfully (2.8 GB in 4m12s)', created_at: ago(14400) },
  { id: uuid(), cluster_name: 'orders-db', satellite_id: SAT_IDS.edge1, severity: 'info', message: 'Incremental backup completed (128 MB delta)', created_at: ago(3600) },
  { id: uuid(), cluster_name: 'orders-db', satellite_id: SAT_IDS.edge2, severity: 'info', message: 'WAL archiving healthy, 247 segments archived in last 24h', created_at: ago(21600) },
  { id: uuid(), cluster_name: null, satellite_id: SAT_IDS.edge5, severity: 'info', message: 'New satellite registered: edge-eu-central (pending approval)', created_at: ago(120) },
  { id: uuid(), cluster_name: null, satellite_id: SAT_IDS.edge4, severity: 'error', message: 'Satellite edge-us-west connection lost', created_at: ago(300) },
];

// --- PostgreSQL Versions --------------------------------------------------

export const postgresVersions = [
  { id: 'pgv-001', version: '17.2', variant: 'alpine', image_tag: 'postgres:17.2-alpine', is_default: true },
  { id: 'pgv-002', version: '17.2', variant: 'bookworm', image_tag: 'postgres:17.2-bookworm', is_default: false },
  { id: 'pgv-003', version: '16.6', variant: 'alpine', image_tag: 'postgres:16.6-alpine', is_default: false },
  { id: 'pgv-004', version: '16.6', variant: 'bookworm', image_tag: 'postgres:16.6-bookworm', is_default: false },
  { id: 'pgv-005', version: '15.10', variant: 'alpine', image_tag: 'postgres:15.10-alpine', is_default: false },
];

export const postgresVariants = [
  { id: 'pgvar-001', name: 'alpine', description: 'Minimal Alpine-based image' },
  { id: 'pgvar-002', name: 'bookworm', description: 'Debian Bookworm-based image' },
  { id: 'pgvar-003', name: 'postgis', description: 'PostGIS spatial extension' },
];

// --- Storage Tiers --------------------------------------------------------

export const storageTiers = [
  { id: 'tier-001', name: 'fast', description: 'High-performance NVMe or provisioned IOPS storage', created_at: ago(86400 * 30), updated_at: ago(86400 * 30) },
  { id: 'tier-002', name: 'standard', description: 'General-purpose SSD storage', created_at: ago(86400 * 30), updated_at: ago(86400 * 30) },
  { id: 'tier-003', name: 'replicated', description: 'Multi-AZ replicated block storage', created_at: ago(86400 * 20), updated_at: ago(86400 * 20) },
  { id: 'tier-004', name: 'archive', description: 'Low-cost storage for backups and cold data', created_at: ago(86400 * 15), updated_at: ago(86400 * 15) },
];

// --- Backup Profiles ------------------------------------------------------

const BACKUP_IDS = {
  s3Prod: 'bp-00000001-0000-4000-8000-000000000001',
  localDev: 'bp-00000002-0000-4000-8000-000000000002',
};

export const backupProfiles = [
  {
    id: BACKUP_IDS.s3Prod,
    name: 'prod-s3-hourly',
    description: 'Production backup: daily base + hourly incremental to S3',
    config: JSON.stringify({
      physical: { base_schedule: '0 0 4 * * *', incremental_schedule: '0 0 * * * *', wal_archive_enabled: true, archive_timeout_seconds: 120 },
      logical: { schedule: '0 0 2 * * 0', databases: ['appdb', 'analytics'], format: 'custom' },
      destination: { type: 's3', s3: { bucket: 'pg-swarm-backups-prod', region: 'india-south-1', path_prefix: 'backups/' } },
      retention: { base_backup_count: 7, wal_retention_days: 14, logical_backup_count: 4 },
    }),
    created_at: ago(86400 * 28),
  },
  {
    id: BACKUP_IDS.localDev,
    name: 'local-daily',
    description: 'Dev/staging backup: daily logical dump to local PVC',
    config: JSON.stringify({
      logical: { schedule: '0 0 3 * * *', databases: [], format: 'custom' },
      destination: { type: 'local', local: { path: '/backup-storage' } },
      retention: { logical_backup_count: 3 },
    }),
    created_at: ago(86400 * 14),
  },
];

// --- Backups (inventory) --------------------------------------------------

export const backups = [
  // orders-db (us-east) — full backup history
  { id: uuid(), cluster_id: CLUSTER_IDS.ordersUsEast, backup_profile_id: BACKUP_IDS.s3Prod, type: 'base', status: 'completed', backup_path: 'orders-db/base/20260315_040012', size_bytes: 12_800_000_000, started_at: ago(43200), completed_at: ago(42900), pg_version: '17.2' },
  { id: uuid(), cluster_id: CLUSTER_IDS.ordersUsEast, backup_profile_id: BACKUP_IDS.s3Prod, type: 'incremental', status: 'completed', backup_path: 'orders-db/incremental/20260315_100005', size_bytes: 134_217_728, started_at: ago(18000), completed_at: ago(17940), pg_version: '17.2' },
  { id: uuid(), cluster_id: CLUSTER_IDS.ordersUsEast, backup_profile_id: BACKUP_IDS.s3Prod, type: 'incremental', status: 'completed', backup_path: 'orders-db/incremental/20260315_110003', size_bytes: 98_304_000, started_at: ago(14400), completed_at: ago(14360), pg_version: '17.2' },
  { id: uuid(), cluster_id: CLUSTER_IDS.ordersUsEast, backup_profile_id: BACKUP_IDS.s3Prod, type: 'incremental', status: 'completed', backup_path: 'orders-db/incremental/20260315_120001', size_bytes: 112_640_000, started_at: ago(10800), completed_at: ago(10755), pg_version: '17.2' },
  { id: uuid(), cluster_id: CLUSTER_IDS.ordersUsEast, backup_profile_id: BACKUP_IDS.s3Prod, type: 'incremental', status: 'completed', backup_path: 'orders-db/incremental/20260315_130002', size_bytes: 87_040_000, started_at: ago(7200), completed_at: ago(7168), pg_version: '17.2' },
  { id: uuid(), cluster_id: CLUSTER_IDS.ordersUsEast, backup_profile_id: BACKUP_IDS.s3Prod, type: 'incremental', status: 'running', backup_path: 'orders-db/incremental/20260315_140001', size_bytes: 0, started_at: ago(120), completed_at: null, pg_version: '17.2' },
  { id: uuid(), cluster_id: CLUSTER_IDS.ordersUsEast, backup_profile_id: BACKUP_IDS.s3Prod, type: 'logical', status: 'completed', backup_path: 'orders-db/logical/20260309_020015', size_bytes: 3_200_000_000, started_at: ago(86400 * 6), completed_at: ago(86400 * 6 - 480), pg_version: '17.2' },
  { id: uuid(), cluster_id: CLUSTER_IDS.ordersUsEast, backup_profile_id: BACKUP_IDS.s3Prod, type: 'logical', status: 'completed', backup_path: 'orders-db/logical/20260316_020008', size_bytes: 3_350_000_000, started_at: ago(3600), completed_at: ago(3120), pg_version: '17.2' },
  { id: uuid(), cluster_id: CLUSTER_IDS.ordersUsEast, backup_profile_id: BACKUP_IDS.s3Prod, type: 'base', status: 'completed', backup_path: 'orders-db/base/20260308_040010', size_bytes: 12_500_000_000, started_at: ago(86400 * 7), completed_at: ago(86400 * 7 - 320), pg_version: '17.2' },

  // orders-db (eu-west) — some backups
  { id: uuid(), cluster_id: CLUSTER_IDS.ordersEuWest, backup_profile_id: BACKUP_IDS.s3Prod, type: 'base', status: 'completed', backup_path: 'orders-db-eu/base/20260315_040018', size_bytes: 10_200_000_000, started_at: ago(43200), completed_at: ago(42850), pg_version: '17.2' },
  { id: uuid(), cluster_id: CLUSTER_IDS.ordersEuWest, backup_profile_id: BACKUP_IDS.s3Prod, type: 'incremental', status: 'completed', backup_path: 'orders-db-eu/incremental/20260315_100010', size_bytes: 95_000_000, started_at: ago(18000), completed_at: ago(17950), pg_version: '17.2' },
  { id: uuid(), cluster_id: CLUSTER_IDS.ordersEuWest, backup_profile_id: BACKUP_IDS.s3Prod, type: 'logical', status: 'completed', backup_path: 'orders-db-eu/logical/20260316_020012', size_bytes: 2_900_000_000, started_at: ago(3600), completed_at: ago(3200), pg_version: '17.2' },

  // staging-db — logical only
  { id: uuid(), cluster_id: CLUSTER_IDS.stagingAp, backup_profile_id: BACKUP_IDS.localDev, type: 'logical', status: 'completed', backup_path: 'staging-db/logical/20260315_030008', size_bytes: 950_000_000, started_at: ago(46800), completed_at: ago(46500), pg_version: '17.2' },
  { id: uuid(), cluster_id: CLUSTER_IDS.stagingAp, backup_profile_id: BACKUP_IDS.localDev, type: 'logical', status: 'completed', backup_path: 'staging-db/logical/20260314_030012', size_bytes: 940_000_000, started_at: ago(86400 + 46800), completed_at: ago(86400 + 46500), pg_version: '17.2' },
  { id: uuid(), cluster_id: CLUSTER_IDS.stagingAp, backup_profile_id: BACKUP_IDS.localDev, type: 'logical', status: 'completed', backup_path: 'staging-db/logical/20260313_030010', size_bytes: 935_000_000, started_at: ago(86400 * 2 + 46800), completed_at: ago(86400 * 2 + 46500), pg_version: '17.2' },

  // payments-db (us-east)
  { id: uuid(), cluster_id: CLUSTER_IDS.paymentsUs, backup_profile_id: BACKUP_IDS.s3Prod, type: 'base', status: 'completed', backup_path: 'payments-db/base/20260315_040020', size_bytes: 5_600_000_000, started_at: ago(43200), completed_at: ago(43050), pg_version: '17.2' },
  { id: uuid(), cluster_id: CLUSTER_IDS.paymentsUs, backup_profile_id: BACKUP_IDS.s3Prod, type: 'incremental', status: 'completed', backup_path: 'payments-db/incremental/20260315_100008', size_bytes: 45_000_000, started_at: ago(18000), completed_at: ago(17980), pg_version: '17.2' },
  { id: uuid(), cluster_id: CLUSTER_IDS.paymentsUs, backup_profile_id: BACKUP_IDS.s3Prod, type: 'incremental', status: 'failed', backup_path: 'payments-db/incremental/20260315_110004', size_bytes: 0, started_at: ago(14400), completed_at: ago(14350), pg_version: '17.2' },
  { id: uuid(), cluster_id: CLUSTER_IDS.paymentsUs, backup_profile_id: BACKUP_IDS.s3Prod, type: 'incremental', status: 'completed', backup_path: 'payments-db/incremental/20260315_120006', size_bytes: 52_000_000, started_at: ago(10800), completed_at: ago(10770), pg_version: '17.2' },

  // inventory-db (us-east)
  { id: uuid(), cluster_id: CLUSTER_IDS.inventoryUs, backup_profile_id: BACKUP_IDS.s3Prod, type: 'base', status: 'completed', backup_path: 'inventory-db/base/20260315_040025', size_bytes: 11_200_000_000, started_at: ago(43200), completed_at: ago(42880), pg_version: '17.2' },
  { id: uuid(), cluster_id: CLUSTER_IDS.inventoryUs, backup_profile_id: BACKUP_IDS.s3Prod, type: 'logical', status: 'completed', backup_path: 'inventory-db/logical/20260316_020020', size_bytes: 4_100_000_000, started_at: ago(3600), completed_at: ago(3050), pg_version: '17.2' },

  // sessions-db (us-east)
  { id: uuid(), cluster_id: CLUSTER_IDS.sessionsUs, backup_profile_id: BACKUP_IDS.s3Prod, type: 'base', status: 'completed', backup_path: 'sessions-db/base/20260315_040030', size_bytes: 7_800_000_000, started_at: ago(43200), completed_at: ago(42950), pg_version: '17.2' },
  { id: uuid(), cluster_id: CLUSTER_IDS.sessionsUs, backup_profile_id: BACKUP_IDS.s3Prod, type: 'incremental', status: 'completed', backup_path: 'sessions-db/incremental/20260315_100015', size_bytes: 220_000_000, started_at: ago(18000), completed_at: ago(17920), pg_version: '17.2' },
];

// --- Restores -------------------------------------------------------------

export const restores = [
  { id: uuid(), cluster_id: CLUSTER_IDS.ordersUsEast, restore_type: 'logical', backup_path: 'orders-db/logical/20260309_020015', target_database: 'appdb', status: 'completed', started_at: ago(86400 * 5), completed_at: ago(86400 * 5 - 600) },
  { id: uuid(), cluster_id: CLUSTER_IDS.ordersUsEast, restore_type: 'logical', backup_path: 'orders-db/logical/20260316_020008', target_database: 'analytics', status: 'completed', started_at: ago(1800), completed_at: ago(1500) },
  { id: uuid(), cluster_id: CLUSTER_IDS.stagingAp, restore_type: 'logical', backup_path: 'staging-db/logical/20260314_030012', target_database: 'staging_app', status: 'completed', started_at: ago(86400), completed_at: ago(86400 - 180) },
  { id: uuid(), cluster_id: CLUSTER_IDS.paymentsUs, restore_type: 'physical', backup_path: 'payments-db/base/20260315_040020', target_database: null, status: 'completed', started_at: ago(86400 * 2), completed_at: ago(86400 * 2 - 900) },
  { id: uuid(), cluster_id: CLUSTER_IDS.ordersEuWest, restore_type: 'pitr', backup_path: 'orders-db-eu/base/20260315_040018', target_database: null, status: 'completed', started_at: ago(86400 * 3), completed_at: ago(86400 * 3 - 1200) },
];

// --- Recovery Rule Sets ---------------------------------------------------

const BUILTIN_RULES = [
  { name: 'stale-wal-recovery', pattern: 'invalid record length at .* expected at least \\d+, got 0', severity: 'critical', action: 'restart', cooldown_seconds: 60, category: 'WAL & Checkpoint', enabled: true, builtin: true, threshold: 1, threshold_window_seconds: 0 },
  { name: 'checkpoint-missing', pattern: 'could not locate a valid checkpoint record', severity: 'critical', action: 'rebasebackup', cooldown_seconds: 120, category: 'WAL & Checkpoint', enabled: true, builtin: true, threshold: 1, threshold_window_seconds: 0 },
  { name: 'wal-read-error', pattern: 'could not read WAL at .* invalid record length', severity: 'critical', action: 'restart', cooldown_seconds: 60, category: 'WAL & Checkpoint', enabled: true, builtin: true, threshold: 1, threshold_window_seconds: 0 },
  { name: 'wal-size-mismatch', pattern: 'WAL file .* has size \\d+, should be \\d+', severity: 'critical', action: 'restart', cooldown_seconds: 60, category: 'WAL & Checkpoint', enabled: true, builtin: true, threshold: 1, threshold_window_seconds: 0 },
  { name: 'wal-prevlink-corrupt', pattern: 'record with incorrect prev-link', severity: 'critical', action: 'restart', cooldown_seconds: 60, category: 'WAL & Checkpoint', enabled: true, builtin: true, threshold: 1, threshold_window_seconds: 0 },
  { name: 'timeline-not-in-history', pattern: 'requested starting point .* is not in this server.s history', severity: 'critical', action: 'rewind', cooldown_seconds: 120, category: 'Timeline & Replication', enabled: true, builtin: true, threshold: 1, threshold_window_seconds: 0 },
  { name: 'ahead-of-flush', pattern: 'requested starting point .* is ahead of the WAL flush position', severity: 'critical', action: 'rewind', cooldown_seconds: 120, category: 'Timeline & Replication', enabled: true, builtin: true, threshold: 1, threshold_window_seconds: 0 },
  { name: 'timeline-not-child', pattern: 'requested timeline \\d+ is not a child of this server.s history', severity: 'critical', action: 'rewind', cooldown_seconds: 120, category: 'Timeline & Replication', enabled: true, builtin: true, threshold: 1, threshold_window_seconds: 0 },
  { name: 'timeline-fork', pattern: 'new timeline \\d+ is not a child of database system timeline \\d+', severity: 'critical', action: 'rewind', cooldown_seconds: 120, category: 'Timeline & Replication', enabled: true, builtin: true, threshold: 1, threshold_window_seconds: 0 },
  { name: 'wal-stream-timeline', pattern: 'could not receive data from WAL stream:.*timeline', severity: 'critical', action: 'rewind', cooldown_seconds: 120, category: 'Timeline & Replication', enabled: true, builtin: true, threshold: 1, threshold_window_seconds: 0 },
  { name: 'page-corruption', pattern: 'invalid page in block \\d+ of relation', severity: 'critical', action: 'event', cooldown_seconds: 300, category: 'Corruption', enabled: true, builtin: true, threshold: 1, threshold_window_seconds: 0 },
  { name: 'read-block-error', pattern: 'could not read block \\d+ in file', severity: 'critical', action: 'event', cooldown_seconds: 300, category: 'Corruption', enabled: true, builtin: true, threshold: 1, threshold_window_seconds: 0 },
  { name: 'disk-full', pattern: 'could not write to file.*No space left on device', severity: 'critical', action: 'event', cooldown_seconds: 60, category: 'Corruption', enabled: true, builtin: true, threshold: 1, threshold_window_seconds: 0 },
  { name: 'fsync-error', pattern: 'could not fsync file', severity: 'critical', action: 'event', cooldown_seconds: 60, category: 'Corruption', enabled: true, builtin: true, threshold: 1, threshold_window_seconds: 0 },
  { name: 'too-many-clients', pattern: 'FATAL:.*sorry, too many clients already', severity: 'error', action: 'event', cooldown_seconds: 30, category: 'Connections', enabled: true, builtin: true, threshold: 3, threshold_window_seconds: 60 },
  { name: 'reserved-slots-full', pattern: 'FATAL:.*remaining connection slots are reserved', severity: 'error', action: 'event', cooldown_seconds: 30, category: 'Connections', enabled: true, builtin: true, threshold: 1, threshold_window_seconds: 0 },
  { name: 'out-of-memory', pattern: 'ERROR:.*out of memory', severity: 'error', action: 'event', cooldown_seconds: 30, category: 'Connections', enabled: true, builtin: true, threshold: 1, threshold_window_seconds: 0 },
  { name: 'shared-memory', pattern: 'out of shared memory', severity: 'error', action: 'event', cooldown_seconds: 60, category: 'Connections', enabled: true, builtin: true, threshold: 1, threshold_window_seconds: 0 },
  { name: 'slot-invalidated', pattern: 'replication slot .* has been invalidated', severity: 'critical', action: 'rebasebackup', cooldown_seconds: 300, category: 'Replication Slots', enabled: true, builtin: true, threshold: 1, threshold_window_seconds: 0 },
  { name: 'wal-removed', pattern: 'requested WAL segment .* has already been removed', severity: 'critical', action: 'rebasebackup', cooldown_seconds: 300, category: 'Replication Slots', enabled: true, builtin: true, threshold: 1, threshold_window_seconds: 0 },
  { name: 'slot-missing', pattern: 'replication slot .* does not exist', severity: 'warning', action: 'event', cooldown_seconds: 120, category: 'Replication Slots', enabled: true, builtin: true, threshold: 1, threshold_window_seconds: 0 },
  { name: 'walsender-timeout', pattern: 'terminating walsender process due to replication timeout', severity: 'warning', action: 'event', cooldown_seconds: 60, category: 'Replication Slots', enabled: true, builtin: true, threshold: 1, threshold_window_seconds: 0 },
  { name: 'recovery-conflict', pattern: 'canceling statement due to conflict with recovery', severity: 'info', action: 'event', cooldown_seconds: 30, category: 'Replication Slots', enabled: true, builtin: true, threshold: 1, threshold_window_seconds: 0 },
  { name: 'auth-failure', pattern: 'FATAL:.*password authentication failed for user', severity: 'warning', action: 'event', cooldown_seconds: 30, category: 'Authentication', enabled: true, builtin: true, threshold: 5, threshold_window_seconds: 120 },
  { name: 'hba-rejection', pattern: 'FATAL:.*no pg_hba.conf entry for', severity: 'warning', action: 'event', cooldown_seconds: 30, category: 'Authentication', enabled: true, builtin: true, threshold: 3, threshold_window_seconds: 60 },
  { name: 'stale-backup-label', pattern: 'FATAL:.*could not open file.*backup_label', severity: 'error', action: 'restart', cooldown_seconds: 60, category: 'Recovery', enabled: true, builtin: true, threshold: 1, threshold_window_seconds: 0 },
  { name: 'filenode-map-missing', pattern: 'could not open file.*pg_filenode\\.map', severity: 'critical', action: 'rebasebackup', cooldown_seconds: 120, category: 'Recovery', enabled: true, builtin: true, threshold: 1, threshold_window_seconds: 0 },
  { name: 'wal-level-minimal', pattern: 'WAL was generated with .wal_level=minimal., cannot continue recovering', severity: 'critical', action: 'rebasebackup', cooldown_seconds: 120, category: 'Recovery', enabled: true, builtin: true, threshold: 1, threshold_window_seconds: 0 },
  { name: 'wal-dir-missing', pattern: 'FATAL:.*could not open directory.*pg_wal', severity: 'critical', action: 'rebasebackup', cooldown_seconds: 120, category: 'Recovery', enabled: true, builtin: true, threshold: 1, threshold_window_seconds: 0 },
  { name: 'version-mismatch', pattern: 'database files are incompatible with server', severity: 'critical', action: 'rebasebackup', cooldown_seconds: 300, category: 'Recovery', enabled: true, builtin: true, threshold: 1, threshold_window_seconds: 0 },
  { name: 'catalog-corruption', pattern: 'cache lookup failed for (relation|type|function|operator)', severity: 'critical', action: 'rebasebackup', cooldown_seconds: 300, category: 'Recovery', enabled: true, builtin: true, threshold: 1, threshold_window_seconds: 0 },
  { name: 'tablespace-missing', pattern: 'could not open tablespace directory', severity: 'critical', action: 'rebasebackup', cooldown_seconds: 120, category: 'Recovery', enabled: true, builtin: true, threshold: 1, threshold_window_seconds: 0 },
  { name: 'recovery-complete', pattern: 'consistent recovery state reached', severity: 'info', action: 'event', cooldown_seconds: 0, category: 'Recovery', enabled: true, builtin: true, threshold: 1, threshold_window_seconds: 0 },
  { name: 'streaming-started', pattern: 'started streaming WAL from primary', severity: 'info', action: 'event', cooldown_seconds: 0, category: 'Streaming', enabled: true, builtin: true, threshold: 1, threshold_window_seconds: 0 },
  { name: 'primary-unreachable', pattern: 'FATAL:.*could not connect to the primary server', severity: 'warning', action: 'event', cooldown_seconds: 30, category: 'Streaming', enabled: true, builtin: true, threshold: 3, threshold_window_seconds: 90 },
  { name: 'replication-terminated', pattern: 'replication terminated by primary server', severity: 'warning', action: 'event', cooldown_seconds: 60, category: 'Streaming', enabled: true, builtin: true, threshold: 1, threshold_window_seconds: 0 },
  { name: 'max-walsenders', pattern: 'FATAL:.*number of requested standby connections exceeds max_wal_senders', severity: 'error', action: 'event', cooldown_seconds: 120, category: 'Streaming', enabled: true, builtin: true, threshold: 1, threshold_window_seconds: 0 },
  { name: 'archive-failed', pattern: 'archive command failed with exit code', severity: 'error', action: 'event', cooldown_seconds: 60, category: 'Archive', enabled: true, builtin: true, threshold: 1, threshold_window_seconds: 0 },
];

export const recoveryRuleSets = [
  {
    id: 'rs-default',
    name: 'Default',
    description: 'Built-in recovery rules for PostgreSQL HA clusters — all 36 rules enabled',
    builtin: true,
    rules: BUILTIN_RULES.map(r => ({ ...r })),
    created_at: ago(86400 * 60),
    updated_at: ago(86400 * 2),
  },
  {
    id: 'rs-prod-strict',
    name: 'production-strict',
    description: 'Aggressive auto-recovery: page corruption triggers rebasebackup instead of event-only',
    builtin: false,
    rules: [
      ...BUILTIN_RULES.map(r => {
        if (r.name === 'page-corruption') return { ...r, action: 'rebasebackup', builtin: false };
        if (r.name === 'read-block-error') return { ...r, action: 'rebasebackup', builtin: false };
        if (r.name === 'recovery-conflict') return { ...r, enabled: false, builtin: false };
        return { ...r, builtin: false };
      }),
      { name: 'deadlock-alert', pattern: 'deadlock detected', severity: 'warning', action: 'event', cooldown_seconds: 30, category: 'Custom', enabled: true, builtin: false, threshold: 1, threshold_window_seconds: 0 },
      { name: 'long-lock-wait', pattern: 'process \\d+ still waiting for .* lock on', severity: 'warning', action: 'event', cooldown_seconds: 60, category: 'Custom', enabled: true, builtin: false, threshold: 3, threshold_window_seconds: 120 },
    ],
    created_at: ago(86400 * 10),
    updated_at: ago(86400),
  },
];

// --- Satellite logs -------------------------------------------------------

export function generateLogs(satelliteId, limit = 200) {
  const levels = ['info', 'info', 'info', 'info', 'warn', 'debug', 'error'];
  const messages = [
    ['info', 'Heartbeat sent to central'],
    ['info', 'Config reconciliation complete, 0 changes'],
    ['info', 'Health check completed for all clusters'],
    ['info', 'WAL archiver status: healthy'],
    ['info', 'Connection pool stats: 42/200 used'],
    ['debug', 'Reconcile loop iteration #4821 started'],
    ['debug', 'Fetching cluster configs from central'],
    ['debug', 'StatefulSet orders-db: 3/3 pods ready'],
    ['warn', 'Replication lag on orders-db-1 approaching threshold: 0.8s'],
    ['warn', 'Disk usage at 72% on data volume'],
    ['warn', 'Connection pool utilization above 70%'],
    ['error', 'Failed to connect to central: context deadline exceeded (retrying in 5s)'],
    ['error', 'Pod orders-db-2 readiness probe failed'],
  ];

  const logs = [];
  for (let i = 0; i < limit; i++) {
    const [level, msg] = messages[Math.floor(Math.random() * messages.length)];
    logs.push({
      timestamp: ago(i * 3 + Math.random() * 3),
      level,
      message: msg,
      source: 'satellite/' + satelliteId.slice(0, 12),
    });
  }
  return logs.sort((a, b) => new Date(b.timestamp) - new Date(a.timestamp));
}
