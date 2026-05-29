import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { resolve } from 'path'

export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: {
      // Allow the app to import from wailsjs paths whether stubs or generated
      '../wailsjs': resolve(__dirname, 'wailsjs'),
      './wailsjs': resolve(__dirname, 'wailsjs'),
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
})
