// The list of agents this UI reads, held at runtime rather than baked in.
//
// It used to come from AGENT_I_HOSTS, which Vite resolved once at config time
// and froze into the bundle. That made adding a server a dev-server restart,
// and it made the list a thing you edited in a shell command rather than in
// the UI you were looking at — which is the wrong place for it when the hosts
// live in different accounts and get added one at a time as tunnels come up.
//
// AGENT_I_HOSTS is still read, but only to seed an empty list on first run, so
// an existing setup starts exactly where it left off.
//
// Everything here is a pure function over plain data apart from the storage
// helpers, because this is the part with the parsing rules and parsing rules
// are what break silently.

const STORAGE_KEY = "agent-i.hosts.v1";

// Seeded from vite.config.js. Absent in tests and in any build that did not
// define it, which is why every read is guarded.
function seedHosts() {
  if (typeof __AGENT_HOSTS__ === "undefined" || !Array.isArray(__AGENT_HOSTS__)) return [];
  return normalizeHosts(__AGENT_HOSTS__);
}

// normalizeURL makes a typed-in address usable, or returns "" if it cannot.
//
// The tolerance here is deliberate: the addresses people paste come from ssh
// commands and terminal output, so they arrive with no scheme, a trailing
// slash, or a path glued on from a copied curl. Rejecting those outright would
// mean the common case is an error message, when what was meant is
// unambiguous.
export function normalizeURL(raw) {
  const text = String(raw || "").trim();
  if (!text) return "";

  // A bare host:port is what `ssh -L 8089:...` leaves you thinking in, so it
  // is accepted and assumed to be plain http — the agent's dashboard serves
  // no TLS.
  const withScheme = /^[a-zA-Z][a-zA-Z0-9+.-]*:\/\//.test(text) ? text : `http://${text}`;

  let u;
  try {
    u = new URL(withScheme);
  } catch {
    return "";
  }
  if (u.protocol !== "http:" && u.protocol !== "https:") return "";
  if (!u.hostname) return "";

  // Only the origin is kept. A pasted /api/snapshot or a trailing slash would
  // otherwise be concatenated with the path this UI appends, producing
  // /api/snapshot/api/snapshot and a 404 that looks like a dead agent.
  return u.origin;
}

// parseHostSpec reads one or many host entries.
//
// Accepts the AGENT_I_HOSTS comma form and newline-separated pasting, because
// a list of ten servers is something you paste a block of, not something you
// type commas into.
//
//   web-1=http://127.0.0.1:8089
//   web-2=127.0.0.1:8090
//   127.0.0.1:8091
//
// Names are optional. A bare URL labels itself from the agent_id in its own
// snapshot — but only while it is reachable, which is why naming them is still
// worth doing: a host whose tunnel is down reports no agent_id and would
// otherwise show as a bare address in the picker.
export function parseHostSpec(text) {
  return String(text || "")
    .split(/[\n,]/)
    .map((entry) => entry.trim())
    .filter(Boolean)
    // Tolerate a leading "- " from a pasted YAML or markdown list.
    .map((entry) => entry.replace(/^[-*]\s+/, ""))
    .map((entry) => {
      // First '=' only: a URL may legitimately contain '=' in a query string,
      // and a name never does.
      const eq = entry.indexOf("=");
      if (eq === -1) return { name: "", url: entry };
      return { name: entry.slice(0, eq).trim(), url: entry.slice(eq + 1).trim() };
    })
    .map((h) => ({ name: h.name, url: normalizeURL(h.url) }))
    .filter((h) => h.url);
}

// normalizeHosts cleans a list and drops duplicates.
//
// The URL is the identity, not the name: two entries pointing at the same
// address are the same agent whatever they are called, and keeping both would
// poll it twice and list it twice in the fleet. The first wins, so a named
// entry is not replaced by a later bare one.
export function normalizeHosts(list) {
  const out = [];
  const seen = new Set();
  for (const entry of Array.isArray(list) ? list : []) {
    const url = normalizeURL(entry?.url);
    if (!url || seen.has(url)) continue;
    seen.add(url);
    out.push({ name: String(entry?.name || "").trim(), url });
  }
  return out;
}

// hostLabel is what to call a host in the UI.
//
// A configured name always wins, because it is the only label available for a
// host whose tunnel is down. The live agent_id is the fallback for entries
// added as a bare URL, and the address itself is the last resort.
export function hostLabel(host, agentID) {
  if (!host) return "";
  if (host.name) return host.name;
  if (agentID) return agentID;
  return host.url.replace(/^https?:\/\//, "");
}

// toHostSpec renders the list back into the paste format, so what you exported
// can be pasted into another machine or into AGENT_I_HOSTS unchanged.
export function toHostSpec(hosts) {
  return normalizeHosts(hosts)
    .map((h) => (h.name ? `${h.name}=${h.url}` : h.url))
    .join("\n");
}

// --- persistence ---

export function loadHosts() {
  let stored = null;
  try {
    stored = window.localStorage.getItem(STORAGE_KEY);
  } catch {
    // Private browsing, or storage disabled. The list still works for this
    // session; it just will not survive a reload.
    return seedHosts();
  }
  if (!stored) return seedHosts();
  try {
    const parsed = normalizeHosts(JSON.parse(stored));
    // An empty stored list is a real choice — "I removed them all" — but it
    // leaves the UI with nothing to talk to, so fall back to the seed rather
    // than rendering a dashboard with no host.
    return parsed.length > 0 ? parsed : seedHosts();
  } catch {
    return seedHosts();
  }
}

export function saveHosts(hosts) {
  try {
    window.localStorage.setItem(STORAGE_KEY, JSON.stringify(normalizeHosts(hosts)));
  } catch {
    // Nothing to do and nothing worth interrupting the user over.
  }
}

export function resetHosts() {
  try {
    window.localStorage.removeItem(STORAGE_KEY);
  } catch {
    /* see saveHosts */
  }
  return seedHosts();
}

// configuredHosts exposes what AGENT_I_HOSTS supplied, so the manager can
// offer to restore it after the list has been edited.
export function configuredHosts() {
  return seedHosts();
}
