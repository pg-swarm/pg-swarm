import { createContext, useContext, useState, useEffect, useCallback } from 'react';
import { api } from '../api';

const DataContext = createContext(null);

export function DataProvider({ children }) {
  const [data, setData] = useState({
    satellites: [], clusters: [], health: [], events: [],
  });
  const [lastRefresh, setLastRefresh] = useState(null);

  const refresh = useCallback(async () => {
    try {
      const [satellites, clusters, health, events] = await Promise.all([
        api.satellites(), api.clusters(), api.health(), api.events(50),
      ]);
      setData({
        satellites: satellites || [],
        clusters:   clusters || [],
        health:     health || [],
        events:     events || [],
      });
      setLastRefresh(new Date());
    } catch (err) {
      console.error('refresh failed:', err);
    }
  }, []);

  useEffect(() => {
    refresh();
    const id = setInterval(refresh, 10000);
    return () => clearInterval(id);
  }, [refresh]);

  return (
    <DataContext.Provider value={{ ...data, lastRefresh, refresh }}>
      {children}
    </DataContext.Provider>
  );
}

export function useData() {
  return useContext(DataContext);
}
