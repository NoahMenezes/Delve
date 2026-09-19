import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// Wails serves the built files from frontend/dist (embedded into the
// binary by main.go). Relative base keeps asset paths working under
// the assetserver's custom scheme as well as plain `vite preview`.
export default defineConfig({
  plugins: [react()],
  base: './',
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
});
