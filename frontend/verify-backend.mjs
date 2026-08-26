// Regression check for src/backend.js — the mapping from the backend's host
// inventory onto the fleet row shape, and the fallback offered when an agent
// cannot be reached.
//
// Runs with plain node, no test framework, no browser and no backend:
//   npm run test:backend
//
// Both functions here are pure on purpose. The React hooks around them are
// polling and state, which a test can only assert trivially; these are the
// parts that decide what a row says, and a wrong answer in either is a number
// rendered confidently and read as true.

import { backendHostRow, chooseBackendFallback } from "./src/backend.js";

let failed = 0;
const check = (name, cond, detail = "") => {
  if (!cond) { console.error(`  FAIL ${name}${detail ? ` — got ${detail}` : ""}`); failed++; }
  else console.log(`  ok   ${name}`);
};

const NOW = 1787750000000;

const HOST = {
  host_id: "i-00aab1097c1a58ac5",
  agent_id: "teleport",
  last_seen: "2026-08-26T14:33:00Z",
  attributes: {
    "host.id": "i-00aab1097c1a58ac5",
    "host.name": "teleport",
    "host.type": "t3.medium",
    "host.arch": "amd64",
    "host.image.id": "ami-0abcdef",
    "os.name": "Ubuntu",
    "os.version": "24.04",
    "os.description": "Ubuntu 24.04.4 LTS",
    "os.type": "linux",
    "cloud.account.id": "123456789012",
    "cloud.availability_zone": "us-east-1d",
  },
  cpu_pct: 37.5,
  mem_pct: 66.2,
  disk_pct: 47.0,
  load15: 1.42,
};

console.log("backendHostRow — identity");
{
  const r = backendHostRow(HOST, Date.parse(HOST.last_seen) + 1000);
  check("name comes from agent_id", r.host === "teleport", r.host);
  check("instance id is surfaced", r.instanceID === "i-00aab1097c1a58ac5", r.instanceID);
  check("instance type is surfaced", r.instanceType === "t3.medium", r.instanceType);
  check("zone is surfaced", r.zone === "us-east-1d", r.zone);
  check("account is surfaced", r.account === "123456789012", r.account);
  check("ami is surfaced", r.imageID === "ami-0abcdef", r.imageID);
  check("arch is surfaced", r.arch === "amd64", r.arch);
}

console.log("backendHostRow — OS");
{
  const r = backendHostRow(HOST, NOW);
  check("distro and version are preferred", r.os === "Ubuntu 24.04", r.os);
  check("full description is kept for the tooltip", r.osDescription === "Ubuntu 24.04.4 LTS", r.osDescription);

  // The fallback chain matters: os.type is "linux" on every row, which is the
  // uninformative constant this column exists to replace.
  const desc = backendHostRow(
    { host_id: "h", agent_id: "h", last_seen: "2026-08-26T14:33:00Z",
      attributes: { "os.description": "Debian GNU/Linux 12", "os.type": "linux" } },
    NOW
  );
  check("description is used when name/version are absent", desc.os === "Debian GNU/Linux 12", desc.os);

  const bare = backendHostRow(
    { host_id: "h", agent_id: "h", last_seen: "2026-08-26T14:33:00Z",
      attributes: { "os.type": "linux" } },
    NOW
  );
  check("os.type is the last resort", bare.os === "linux", bare.os);

  const none = backendHostRow(
    { host_id: "h", agent_id: "h", last_seen: "2026-08-26T14:33:00Z", attributes: {} },
    NOW
  );
  check("absent OS is a string, not undefined", none.os === "", JSON.stringify(none.os));
}

console.log("backendHostRow — readings vs absences");
{
  const r = backendHostRow(HOST, NOW);
  check("cpu is a number", r.cpu === 37.5, r.cpu);
  check("mem is a number", r.mem === 66.2, r.mem);
  check("disk is a number", r.disk === 47.0, r.disk);
  check("load is a number", r.load15 === 1.42, r.load15);

  // The distinction the backend goes out of its way to preserve: null means
  // the host has not reported the metric, and it must not arrive here as 0,
  // which is a claim that the machine is idle.
  const absent = backendHostRow(
    { ...HOST, cpu_pct: null, mem_pct: null, disk_pct: null, load15: null },
    NOW
  );
  check("absent cpu is NaN, not 0", Number.isNaN(absent.cpu), absent.cpu);
  check("absent mem is NaN, not 0", Number.isNaN(absent.mem), absent.mem);
  check("absent disk is NaN, not 0", Number.isNaN(absent.disk), absent.disk);
  check("absent load is NaN, not 0", Number.isNaN(absent.load15), absent.load15);

  // A genuine zero is a reading and must survive as one.
  const idle = backendHostRow({ ...HOST, cpu_pct: 0, load15: 0 }, NOW);
  check("a real 0 cpu stays 0", idle.cpu === 0, idle.cpu);
  check("a real 0 load stays 0", idle.load15 === 0, idle.load15);

  // IOWait is not computed by the fleet query; it must read as absent rather
  // than as a disk that is never waiting.
  check("iowait is absent, not 0", Number.isNaN(r.iowait), r.iowait);
}

console.log("backendHostRow — liveness");
{
  const seen = Date.parse(HOST.last_seen);
  check("reporting now is active", backendHostRow(HOST, seen + 1000).active === true);
  check("nine minutes silent is still active", backendHostRow(HOST, seen + 9 * 60000).active === true);
  check("eleven minutes silent is inactive", backendHostRow(HOST, seen + 11 * 60000).active === false);
  // An unparseable timestamp must read as stale rather than as live: a host
  // whose age cannot be established has not been established to be reporting.
  const bad = backendHostRow({ ...HOST, last_seen: "not a date" }, NOW);
  check("unparseable last_seen is inactive", bad.active === false);
  check("unparseable last_seen has infinite age", bad.ageSec === Infinity, bad.ageSec);
}

console.log("backendHostRow — degenerate input");
{
  const noAttrs = backendHostRow({ host_id: "i-x", agent_id: "", last_seen: "2026-08-26T14:33:00Z" }, NOW);
  check("missing attributes do not throw", noAttrs.instanceID === "i-x", noAttrs.instanceID);
  check("empty agent_id falls back to the host id", noAttrs.host === "i-x", noAttrs.host);
  // The table sorts these with localeCompare; undefined would take the numeric
  // path and sort nonsensically against the rows that do have a value.
  for (const k of ["instanceType", "zone", "account", "imageID", "arch", "os", "osDescription"]) {
    check(`${k} is a string when absent`, typeof noAttrs[k] === "string", typeof noAttrs[k]);
  }
}

console.log("chooseBackendFallback");
{
  const rows = [
    { instanceID: "i-aaa", host: "teleport", active: true },
    { instanceID: "i-bbb", host: "acct-b-web", active: true },
  ];
  const err = { message: "agent not reachable" };

  check("nothing offered without an error",
    chooseBackendFallback({ readingBackend: false, error: null, backendRows: rows, selectedHost: null }) === null);
  check("nothing offered while already reading the backend",
    chooseBackendFallback({ readingBackend: true, error: err, backendRows: rows, selectedHost: null }) === null);
  check("nothing offered when the backend knows no hosts",
    chooseBackendFallback({ readingBackend: false, error: err, backendRows: [], selectedHost: null }) === null);
  check("nothing offered when rows are missing entirely",
    chooseBackendFallback({ readingBackend: false, error: err, backendRows: undefined, selectedHost: null }) === null);

  // The case that matters: the unreachable agent is a host the backend has.
  const matched = chooseBackendFallback({
    readingBackend: false, error: err, backendRows: rows,
    selectedHost: { url: "http://127.0.0.1:8089", name: "teleport" },
  });
  check("matches the configured name", matched?.hostID === "i-aaa", matched?.hostID);
  check("a name match is marked matched", matched?.matched === true);
  check("carries a label to show", matched?.label === "teleport", matched?.label);

  // Matching must not be defeated by case or padding, which the host manager
  // accepts on input.
  const loose = chooseBackendFallback({
    readingBackend: false, error: err, backendRows: rows,
    selectedHost: { url: "u", name: "  TELEPORT " },
  });
  check("match ignores case and padding", loose?.hostID === "i-aaa", loose?.hostID);

  // No match is still worth an offer, but must not claim to be the same host.
  const unmatched = chooseBackendFallback({
    readingBackend: false, error: err, backendRows: rows,
    selectedHost: { url: "http://127.0.0.1:9999", name: "something-else" },
  });
  check("offers a host even without a match", unmatched?.hostID === "i-aaa", unmatched?.hostID);
  check("an unmatched offer is not claimed as matched", unmatched?.matched === false);

  // A host with no name cannot be matched, and must not match by accident.
  const unnamed = chooseBackendFallback({
    readingBackend: false, error: err, backendRows: rows,
    selectedHost: { url: "http://127.0.0.1:8089", name: "" },
  });
  check("an unnamed host does not claim a match", unnamed?.matched === false);

  // A row with no id cannot be opened, so offering it would be a dead button.
  const idless = chooseBackendFallback({
    readingBackend: false, error: err,
    backendRows: [{ instanceID: "", host: "nameless" }], selectedHost: null,
  });
  check("a row with no id is not offered", idless === null, JSON.stringify(idless));
}

console.log(failed === 0 ? "\nOK - all backend checks passed" : `\n${failed} check(s) failed`);
process.exit(failed ? 1 : 0);
