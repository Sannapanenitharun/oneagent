// Regression check for the diagnostic logic in src/api.js.
//
// Runs with plain node, no test framework, no dev server and no agent:
//   npm run test:api
//
// These two functions exist to answer "what went wrong", and a confident wrong
// answer here costs real time: the message names a layer, and whoever reads it
// goes and inspects that layer. The original version of this told anyone who
// hit a 404 to check their SSH tunnel, while the actual cause was a different
// process listening on the port they had typed. The tunnel was fine.

import { describeSnapshotFailure, isAgentHealthz } from "./src/api.js";

let failed = 0;
const check = (name, cond, detail = "") => {
  if (!cond) { console.error(`  FAIL ${name}${detail ? ` - got ${detail}` : ""}`); failed++; }
  else console.log(`  ok   ${name}`);
};

const T = "http://127.0.0.1:8090";

console.log("describeSnapshotFailure - the proxy's own failures");
{
  // The proxy reports a refused connection as a JSON body carrying `error`.
  // That is the one case where the fault really is in front of the agent.
  const f = describeSnapshotFailure(502, "connect ECONNREFUSED 127.0.0.1:8090", T);
  check("proxy failure is unreachable", f.kind === "unreachable", f.kind);
  check("proxy failure names the proxy", f.detail.includes("proxy could not reach"), f.detail);
  check("proxy failure quotes the cause", f.detail.includes("ECONNREFUSED"), f.detail);
  check("proxy failure names the address", f.detail.includes(T), f.detail);

  // A proxy error must win over the status-based guesses below: the proxy
  // synthesises statuses of its own, and reading one as an upstream answer
  // would describe a connection that never happened.
  const f404 = describeSnapshotFailure(404, "no response within 8000ms", T);
  check("proxy error wins over a 404", f404.kind === "unreachable", f404.kind);
}

console.log("describeSnapshotFailure - something is listening but is not an agent");
{
  // The case that prompted this: obsagent-intake on the port, answering 200 on
  // / and /healthz, 404 on /api/snapshot.
  const f = describeSnapshotFailure(404, "", T);
  check("a bare 404 is its own kind", f.kind === "not-an-agent", f.kind);
  check("message says what it is", f.message === "not an agent", f.message);
  check("detail says something IS listening", f.detail.includes("Something is listening"), f.detail);
  check("detail points at the address", f.detail.includes("check the address"), f.detail);
  // The specific regression: it must NOT send anyone to look at a tunnel.
  check("does not blame a tunnel", !/tunnel is down|SSH tunnel is down/.test(f.detail), f.detail);
}

console.log("describeSnapshotFailure - an agent that refuses");
{
  for (const status of [401, 403]) {
    const f = describeSnapshotFailure(status, "", T);
    check(`${status} is unauthorized`, f.kind === "unauthorized", f.kind);
    check(`${status} mentions the auth token`, f.detail.includes("auth token"), f.detail);
  }
}

console.log("describeSnapshotFailure - anything else");
{
  const f = describeSnapshotFailure(500, "", T);
  check("an unexplained status is unreachable", f.kind === "unreachable", f.kind);
  check("it reports the status it saw", f.detail.includes("500"), f.detail);
  // Still the honest reading: something answered, so the address is the more
  // likely fault than the link to it.
  check("it favours the address over the tunnel",
    f.detail.includes("wrong address"), f.detail);

  const f503 = describeSnapshotFailure(503, "", T);
  check("503 is handled too", f503.detail.includes("503"), f503.detail);
}

console.log("describeSnapshotFailure - shape");
{
  for (const [label, args] of [
    ["proxy", [502, "boom", T]],
    ["404", [404, "", T]],
    ["401", [401, "", T]],
    ["other", [500, "", T]],
  ]) {
    const f = describeSnapshotFailure(...args);
    check(`${label} has all three fields`,
      typeof f.kind === "string" && typeof f.message === "string" && typeof f.detail === "string",
      JSON.stringify(f));
  }
}

console.log("isAgentHealthz");
{
  // What an agent actually returns: the literal string "ok".
  check("agent ok is healthy", isAgentHealthz(true, "text/plain; charset=utf-8", "ok\n") === true);
  check("whitespace is tolerated", isAgentHealthz(true, "text/plain", "  ok  ") === true);
  check("case is tolerated", isAgentHealthz(true, "text/plain", "OK") === true);

  // The regression: another project's daemon on the port, answering 200
  // text/plain with its own banner. Status alone called this healthy, which
  // put a green dot on a host whose dashboard could never load.
  check("a foreign 200 is not healthy",
    isAgentHealthz(true, "text/plain; charset=utf-8", "obsagent-intake\nreceived logs=91") === false);

  // The other long-standing hazard: an unproxied path falling through to the
  // SPA, which answers 200 with index.html.
  check("index.html is not healthy",
    isAgentHealthz(true, "text/html; charset=utf-8", "<!doctype html><html>") === false);

  // A JSON health endpoint from some other service, e.g. {"status":"ok"} —
  // close enough to look right, and not the agent.
  check("json status ok is not healthy",
    isAgentHealthz(true, "application/json", '{"status":"ok"}') === false);

  check("a non-ok status is not healthy", isAgentHealthz(false, "text/plain", "ok") === false);
  check("an empty body is not healthy", isAgentHealthz(true, "text/plain", "") === false);
  check("a missing body is not healthy", isAgentHealthz(true, "text/plain", undefined) === false);
  check("a missing content type is tolerated", isAgentHealthz(true, undefined, "ok") === true);
  // "ok" must be the whole body, not merely present in it.
  check("ok inside a longer body is not healthy",
    isAgentHealthz(true, "text/plain", "status: ok, uptime 4d") === false);
}

console.log(failed === 0 ? "\nOK - all api checks passed" : `\n${failed} check(s) failed`);
process.exit(failed ? 1 : 0);
