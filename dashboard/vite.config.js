import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

const useMock = process.env.MOCK === 'true';

export default defineConfig(async () => {
  const plugins = [react()];

  if (useMock) {
    const { default: mockApiPlugin } = await import('./mock/plugin.js');
    plugins.push(mockApiPlugin());
  }

  return {
    plugins,
    base: '/',
    build: {
      outDir: 'static',
      emptyOutDir: true,
    },
    server: {
      // Only proxy to backend when not using mock data
      ...(!useMock && { proxy: { '/api': 'http://localhost:8080' } }),
    },
  };
});
