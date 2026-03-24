import { createContext, useContext, useState, useEffect, useCallback, useRef } from 'react';
import { api } from '../api';

const DataContext = createContext(null);

const EMPTY = {
  satellites: [], clusters: [], health: [], events: [], profiles: [],
  deploymentRules: [], postgresVersions: [], postgresVariants: [],
  storageTiers: [], recoveryRuleSets: [],
};

function wsUrl() {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  return proto + '//' + location.host + '/api/v1/ws';
}

export function DataProvider({ children }) {
  const [data, setData] = useState(EMPTY);
  const [lastRefresh, setLastRefresh] = useState(null);
  const [wsConnected, setWsConnected] = useState(false);
  const [activeOperations, setActiveOperations] = useState({});
  const wsRef = useRef(null);
  const pollRef = useRef(null);

  // REST polling fallback — same as before.
  const refresh = useCallback(async () => {
    try {
      const safe = (p) => p.catch(() => null);
      const [satellites, clusters, health, events, profiles, deploymentRules, postgresVersions, postgresVariants, storageTiers, recoveryRuleSets] = await Promise.all([
        safe(api.satellites()), safe(api.clusters()), safe(api.health()), safe(api.events(50)), safe(api.profiles()), safe(api.deploymentRules()), safe(api.postgresVersions()), safe(api.postgresVariants()), safe(api.storageTiers()), safe(api.recoveryRuleSets()),
      ]);
      setData({
        satellites:        satellites || [],
        clusters:          clusters || [],
        health:            health || [],
        events:            events || [],
        profiles:          profiles || [],
        deploymentRules:   deploymentRules || [],
        postgresVersions:  postgresVersions || [],
        postgresVariants:  postgresVariants || [],
        storageTiers:      storageTiers || [],
        recoveryRuleSets:  recoveryRuleSets || [],
      });
      setLastRefresh(new Date());
    } catch (err) {
      console.error('refresh failed:', err);
    }
  }, []);

  // Start REST polling.
  const startPolling = useCallback(() => {
    if (pollRef.current) return;
    refresh();
    pollRef.current = setInterval(refresh, 10000);
  }, [refresh]);

  // Stop REST polling.
  const stopPolling = useCallback(() => {
    if (pollRef.current) {
      clearInterval(pollRef.current);
      pollRef.current = null;
    }
  }, []);

  // Apply a full_state message from WebSocket.
  const applyWsState = useCallback((state) => {
    setData({
      satellites:        state.satellites || [],
      clusters:          state.clusters || [],
      health:            state.health || [],
      events:            state.events || [],
      profiles:          state.profiles || [],
      deploymentRules:   state.deploymentRules || [],
      postgresVersions:  state.postgresVersions || [],
      postgresVariants:  state.postgresVariants || [],
      storageTiers:      state.storageTiers || [],
      recoveryRuleSets:  state.recoveryRuleSets || [],
    });
    if (state.activeOperations) {
      setActiveOperations(state.activeOperations);
    }
    setLastRefresh(new Date());
  }, []);

  // WebSocket connection with automatic reconnect.
  useEffect(() => {
    let reconnectTimer = null;
    let alive = true;

    function connect() {
      if (!alive) return;
      try {
        const ws = new WebSocket(wsUrl());
        wsRef.current = ws;

        ws.onopen = () => {
          console.log('ws: connected');
          setWsConnected(true);
          stopPolling();
        };

        ws.onmessage = (e) => {
          try {
            const msg = JSON.parse(e.data);
            if (msg.type === 'full_state' && msg.data) {
              applyWsState(msg.data);
            } else if (msg.type === 'switchover_progress' && msg.data) {
              const p = msg.data;
              const logEntry = {
                step: p.step, step_name: p.step_name, status: p.status,
                target_pod: p.target_pod, detail: p.error_message,
                ponr: p.point_of_no_return, timestamp: p.timestamp || new Date().toISOString(),
              };
              setActiveOperations(prev => {
                const existing = prev[p.operation_id] || { operation_id: p.operation_id, cluster_name: p.cluster_name, steps: {}, log: [] };
                return {
                  ...prev,
                  [p.operation_id]: {
                    ...existing,
                    steps: {
                      ...existing.steps,
                      [p.step]: {
                        step: p.step, step_name: p.step_name, status: p.status,
                        target_pod: p.target_pod, error_message: p.error_message,
                        ponr: p.point_of_no_return,
                      },
                    },
                    log: [...(existing.log || []), logEntry],
                  },
                };
              });
            } else if (msg.type === 'switchover_complete' && msg.data) {
              const d = msg.data;
              setActiveOperations(prev => ({
                ...prev,
                [d.operation_id]: {
                  ...(prev[d.operation_id] || { operation_id: d.operation_id, steps: {}, log: [] }),
                  cluster_name: d.cluster_name,
                  done: true,
                  success: d.success,
                  error: d.error,
                },
              }));
            }
          } catch {}
        };

        ws.onclose = () => {
          console.log('ws: disconnected, falling back to REST polling');
          setWsConnected(false);
          wsRef.current = null;
          startPolling();
          if (alive) {
            reconnectTimer = setTimeout(connect, 3000);
          }
        };

        ws.onerror = () => {
          // onclose will fire after this.
          ws.close();
        };
      } catch {
        // WebSocket constructor failed (e.g. bad URL). Stay on REST.
        startPolling();
      }
    }

    connect();

    return () => {
      alive = false;
      clearTimeout(reconnectTimer);
      stopPolling();
      if (wsRef.current) {
        wsRef.current.close();
        wsRef.current = null;
      }
    };
  }, [applyWsState, startPolling, stopPolling]);

  return (
    <DataContext.Provider value={{ ...data, lastRefresh, refresh, wsConnected, activeOperations }}>
      {children}
    </DataContext.Provider>
  );
}

export function useData() {
  return useContext(DataContext);
}
