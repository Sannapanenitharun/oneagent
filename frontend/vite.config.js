import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import http from "node:http";
import https from "node:https";

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
// CORS hole this design avoids.
const AGENT = process.env.AGENT_I_URL || "http://127.0.0.1:8088";
const OTLP = process.env.AGENT_I_OTLP_URL || "http://127.0.0.1:4319";

// AGENT_I_HOSTS seeds the host list on first run:
//
//   AGENT_I_HOSTS="ec2-prod-1=http://127.0.0.1:8089,ec2-prod-2=http://127.0.0.1:8090"
//
// It is only a seed. The list lives in the browser from then on and is edited
// in the UI — see src/hosts.js. This used to be the whole mechanism, which
// meant every added server was a dev-server restart and the list of hosts was
// something you maintained in a shell command instead of in the app looking at
// them.
function parseHosts(spec) {
  if (!spec || !spec.trim()) return [];
  return spec
    .split(/[\n,]/)
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
}

const SEED = parseHosts(process.env.AGENT_I_HOSTS);

// How long to wait on an agent before calling it unreachable. Longer than a
// loopback round trip by a wide margin, short enough that a dead SSH tunnel
// does not hold the fleet poll open — the fleet view waits on all hosts at
// once, so one hung socket would stall the whole table.
const PROXY_TIMEOUT_MS = 8000;

// Addresses the proxy refuses to fetch.
//
// This forwards to whatever address the page asks for, which is what makes a
// runtime host list possible. That also makes it a request-forwarding service
// listening on localhost, so the cloud metadata endpoints are blocked
// explicitly: they are the one target where "reachable from this machine, no
// credentials needed" turns into credential disclosure. Everything else is
// allowed, because reaching arbitrary internal addresses is the entire job.
const BLOCKED_HOSTNAMES = new Set([
  "169.254.169.254", // AWS/Azure/OpenStack IMDS
  "[fd00:ec2::254]",
  "fd00:ec2::254",
  "metadata.google.internal",
  "metadata.goog",
]);

function parseTarget(raw) {
  if (!raw) return null;
  let u;
  try {
    u = new URL(String(raw));
  } catch {
    return null;
  }
  if (u.protocol !== "http:" && u.protocol !== "https:") return null;
  if (BLOCKED_HOSTNAMES.has(u.hostname.toLowerCase())) return null;
  return u;
}

function proxyError(res, status, message) {
  if (res.headersSent) {
    res.destroy();
    return;
  }
  res.statusCode = status;
  res.setHeader("content-type", "application/json");
  // The frontend reads this and shows it verbatim. A proxy that fails with a
  // bare 502 makes a dead tunnel and a stopped agent look identical, and those
  // have different fixes.
  res.end(JSON.stringify({ error: message }));
}

// agentProxy forwards /h/<path> to the agent named by the X-Agent-Target
// header.
//
// The target rides in a header rather than the path because a URL inside a
// path has to be encoded, and an encoded '/' is normalised by enough layers
// that it is not worth relying on. Vite's built-in `proxy` option cannot do
// this at all: its targets are fixed when the config is evaluated, which is
// precisely the limitation being removed.
function agentProxy() {
  return {
    name: "agent-i-dynamic-proxy",
    configureServer(server) {
      // Mounting on "/h" strips that prefix from req.url, so the agent sees
      // the plain /api/snapshot and /healthz paths it serves and knows nothing
      // about this indirection.
      server.middlewares.use("/h", (req, res) => {
        const target = parseTarget(req.headers["x-agent-target"]);
        if (!target) {
          proxyError(res, 400, `not a usable agent address: ${req.headers["x-agent-target"] || "(none)"}`);
          return;
        }

        const mod = target.protocol === "https:" ? https : http;
        const upstream = mod.request(
          {
            protocol: target.protocol,
            hostname: target.hostname,
            port: target.port || (target.protocol === "https:" ? 443 : 80),
            path: req.url || "/",
            method: req.method,
            headers: {
              host: target.host,
              accept: req.headers.accept || "*/*",
              "user-agent": "agent-i-dashboard",
            },
          },
          (up) => {
            res.statusCode = up.statusCode || 502;
            for (const [k, v] of Object.entries(up.headers)) {
              // Hop-by-hop headers describe the upstream connection, not this
              // one, and re-emitting them corrupts the response framing.
              if (k === "connection" || k === "transfer-encoding" || k === "keep-alive") continue;
              res.setHeader(k, v);
            }
            up.pipe(res);
          }
        );

        upstream.setTimeout(PROXY_TIMEOUT_MS, () => {
          // A half-open SSH tunnel accepts the connection and then never
          // answers, so without this the request hangs until the browser gives
          // up and the UI cannot say what went wrong.
          upstream.destroy(new Error(`no response within ${PROXY_TIMEOUT_MS}ms`));
        });
        upstream.on("error", (err) => {
          proxyError(res, 502, `${target.origin}: ${err.message || err}`);
        });
        req.pipe(upstream);
      });
    },
  };
}

export default defineConfig({
  plugins: [react(), agentProxy()],
  // Lets a connection error name the address that actually failed. Without it
  // the UI can only say "the agent", which is useless when the upstream is a
  // forwarded port and the question is whether the tunnel is up.
  define: {
    __AGENT_TARGET__: JSON.stringify(AGENT),
    __AGENT_HOSTS__: JSON.stringify(SEED),
  },
  server: {
    port: 5173,
    proxy: {
      // The default host, kept so anything pointing at /api directly — the
      // README, a curl, an existing bookmark — keeps working. Every host the
      // UI reads goes through /h instead.
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
