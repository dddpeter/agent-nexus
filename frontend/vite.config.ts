import { defineConfig } from 'vitest/config'
import vue from '@vitejs/plugin-vue'

// 开发期前后端分离：vite dev server 伺服前端（HMR），/api 反代到 Go 后端；
// 生产由 `vite build` 产出 ../web/dist，go:embed 进单二进制。
export default defineConfig({
  plugins: [vue()],
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: process.env.NEXUS_BACKEND ?? 'http://127.0.0.1:8080',
        changeOrigin: true,
      },
    },
  },
  build: {
    outDir: '../web/dist',
    emptyOutDir: true,
  },
  test: {
    environment: 'jsdom',
    include: ['src/**/*.test.ts'],
  },
})
