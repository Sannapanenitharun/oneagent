import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// One origin for everything during development. The agent exposes two
// separate listeners — a dashboard API and an OTLP ingest port — and without
// this you have three URLs and three ports to keep straight. Proxying folds
// them behind http://localhost:5173.
//
// Proxying rather than adding CORS to the agent is deliberate. The dashboard
// endpoint is unauthenticated and exposes this host's metrics, logs and trace
// contents; permissive CORS on it would let any page the operator happens to
// visit read all of that. The proxy keeps the browser same-origin and loosens
// nothing in production.
const AGENT = process.env.AGENT_I_URL || "http://127.0.0.1:8088";
const OTLP = process.env.AGENT_I_OTLP_URL || "http://127.0.0.1:4319";

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      // Dashboard snapshot — the single endpoint every view reads from.
      "/api": { target: AGENT, changeOrigin: true },
      "/healthz": { target: AGENT, changeOrigin: true },

      // OTLP ingest. Lets an instrumented app send to
      // http://localhost:5173/v1/traces instead of needing the receiver's own
      // port, so there is one address to configure. Dev convenience only —
      // see the note in README about not routing production telemetry
      // through a dev server.
      "/v1": { target: OTLP, changeOrigin: true },

      // The agent's own built-in page, kept reachable at /agent for comparing
      // against this UI without switching ports.
      "/agent": {
        target: AGENT,
        changeOrigin: true,
        rewrite: (path) => path.replace(/^\/agent/, "") || "/",
      },
    },
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
  },
});
