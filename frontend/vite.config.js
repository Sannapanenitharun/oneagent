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
//
// It is also why multi-host support has to be built here rather than in the
// browser: the page cannot fetch a second agent directly without exactly the
// CORS hole this design avoids, so every host gets its own proxy route.
const AGENT = process.env.AGENT_I_URL || "http://127.0.0.1:8088";
const OTLP = process.env.AGENT_I_OTLP_URL || "http://127.0.0.1:4319";

// AGENT_I_HOSTS lists the agents this UI can switch between:
//
//   AGENT_I_HOSTS="ec2-prod-1=http://127.0.0.1:8089,ec2-prod-2=http://127.0.0.1:8090"
//
// Each agent needs its own local port, which in practice means its own SSH
// forward — see scripts/dev-tunnel.sh / dev-tunnel.ps1 and their -LocalPort
// option. Unset, this falls back to the single AGENT_I_URL, so an existing
// one-host setup behaves exactly as before.
//
// The name is optional: a bare URL is accepted and the host then labels
// itself from the agent_id in its own snapshot. Naming them here is still
// better, because a host whose tunnel is down never reports an agent_id and
// would otherwise appear in the picker as an unhelpful bare URL.
function parseHosts(spec, fallback) {
  if (!spec || !spec.trim()) return [{ name: "", url: fallback }];

  const hosts = spec
    .split(",")
    .map((entry) => entry.trim())
    .filter(Boolean)
    .map((entry) => {
      // First '=' only: a URL may legitimately contain '=' in a query string,
      // and the name never does.
      const eq = entry.indexOf("=");
      if (eq === -1) return { name: "", url: entry };
      return { name: entry.slice(0, eq).trim(), url: entry.slice(eq + 1).trim() };
    })
    .filter((h) => h.url);

  return hosts.length > 0 ? hosts : [{ name: "", url: fallback }];
}

const HOSTS = parseHosts(process.env.AGENT_I_HOSTS, AGENT);

// One proxy route per host. The rewrite strips the /h/<i> prefix so each
// agent still sees the plain /api/snapshot and /healthz paths it serves —
// the agent knows nothing about this indirection.
const hostProxies = {};
HOSTS.forEach((host, i) => {
  const prefix = `/h/${i}`;
  const opts = {
    target: host.url,
    changeOrigin: true,
    rewrite: (path) => path.slice(prefix.length) || "/",
  };
  hostProxies[`${prefix}/api`] = opts;
  hostProxies[`${prefix}/healthz`] = opts;
});

export default defineConfig({
  plugins: [react()],
  // Lets a connection error name the address that actually failed. Without it
  // the UI can only say "the agent", which is useless when the upstream is a
  // forwarded port and the question is whether the tunnel is up.
  define: {
    __AGENT_TARGET__: JSON.stringify(AGENT),
    __AGENT_HOSTS__: JSON.stringify(HOSTS),
  },
  server: {
    port: 5173,
    proxy: {
      // Per-host routes first: Vite matches longest-prefix, but keeping these
      // above /api makes the precedence obvious to a reader.
      ...hostProxies,

      // Dashboard snapshot for the default host. Retained so anything
      // pointing at /api directly — the README, a curl, an existing bookmark
      // — keeps working whether or not AGENT_I_HOSTS is set.
      "/api": { target: HOSTS[0].url, changeOrigin: true },
      "/healthz": { target: HOSTS[0].url, changeOrigin: true },

      // OTLP ingest. Lets an instrumented app send to
      // http://localhost:5173/v1/traces instead of needing the receiver's own
      // port, so there is one address to configure. Dev convenience only —
      // see the note in README about not routing production telemetry
      // through a dev server.
      "/v1": { target: OTLP, changeOrigin: true },

      // The agent's own built-in page, kept reachable at /agent for comparing
      // against this UI without switching ports.
      "/agent": {
        target: HOSTS[0].url,
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
