/// <reference types="vitest/config" />
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// `pnpm dev` proxies the APIs: /api -> a hub's HTTP listener (LONGARM_HUB_URL,
// default http://127.0.0.1:7480); /local and /tray -> a running
// `longarm-tray --serve` (LONGARM_TRAY_URL).
const hub = process.env.LONGARM_HUB_URL ?? 'http://127.0.0.1:7480'
const tray = process.env.LONGARM_TRAY_URL ?? 'http://127.0.0.1:7490'

export default defineConfig({
  plugins: [react()],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    sourcemap: false,
    chunkSizeWarningLimit: 400,
  },
  server: {
    proxy: {
      '/api': { target: hub, changeOrigin: true },
      '/local': { target: tray, changeOrigin: true },
      '/trayapi': { target: tray, changeOrigin: true },
    },
  },
  test: {
    environment: 'jsdom',
    globals: true,
    include: ['src/**/*.test.{ts,tsx}'],
  },
})
