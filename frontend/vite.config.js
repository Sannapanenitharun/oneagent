import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The agent's dashboard server has no CORS headers, by design — it is a
// loopback-only debug endpoint and adding permissive CORS to it would let any
// page you happen to visit read this host's metrics, logs and trace contents.
// Proxying instead keeps the browser same-origin, so no backend change is
// needed and nothing is loosened in production.
const AGENT = process.env.AGENT_I_URL || "http://127.0.0.1:8088";

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      "/api": { target: AGENT, changeOrigin: true },
      "/healthz": { target: AGENT, changeOrigin: true },
    },
  },
  build: {
    // Built assets are what the Go binary embeds for production, where the
    // agent serves the UI itself and the proxy above is not involved.
    outDir: "dist",
    emptyOutDir: true,
  },
});
