// Regression check for src/hosts.js — the host list parsing and normalisation.
//
// Runs with plain node, no test framework and no browser:
//   npm run test:hosts
//
// This is the code that decides which machine the dashboard talks to, and it
// is deliberately forgiving about what it accepts. Forgiving parsing is
// exactly the kind that goes subtly wrong, so the tolerances are pinned here.

import {
  normalizeURL, parseHostSpec, normalizeHosts, hostLabel, toHostSpec,
} from "./src/hosts.js";

let failed = 0;
const check = (name, cond) => {
  if (!cond) { console.error(`  FAIL ${name}`); failed++; }
  else console.log(`  ok   ${name}`);
};

console.log("normalizeURL");
check("keeps a full URL", normalizeURL("http://127.0.0.1:8089") === "http://127.0.0.1:8089");
check("adds the missing scheme", normalizeURL("127.0.0.1:8089") === "http://127.0.0.1:8089");
check("keeps https", normalizeURL("https://agent.internal:8443") === "https://agent.internal:8443");
check("trims surrounding space", normalizeURL("  127.0.0.1:8089  ") === "http://127.0.0.1:8089");
// A trailing slash or a pasted API path would otherwise be concatenated with
// the path the UI appends, producing /api/snapshot/api/snapshot.
check("drops a trailing slash", normalizeURL("http://127.0.0.1:8089/") === "http://127.0.0.1:8089");
check("drops a pasted path", normalizeURL("http://127.0.0.1:8089/api/snapshot") === "http://127.0.0.1:8089");
check("keeps a non-default port", normalizeURL("localhost:9999") === "http://localhost:9999");
check("rejects empty", normalizeURL("") === "");
check("rejects whitespace", normalizeURL("   ") === "");
check("rejects a non-http scheme", normalizeURL("ftp://example.com") === "");
check("rejects javascript:", normalizeURL("javascript:alert(1)") === "");

console.log("parseHostSpec");
const NEWLINES = parseHostSpec("a=http://127.0.0.1:8089\nb=http://127.0.0.1:8090");
check("splits on newlines", NEWLINES.length === 2);
check("reads the name", NEWLINES[0].name === "a" && NEWLINES[1].name === "b");
check("reads the url", NEWLINES[1].url === "http://127.0.0.1:8090");

const COMMAS = parseHostSpec("a=http://127.0.0.1:8089,b=http://127.0.0.1:8090");
check("splits on commas too", COMMAS.length === 2);

const MIXED = parseHostSpec("a=127.0.0.1:8089\n\n  \nb=127.0.0.1:8090,c=127.0.0.1:8091");
check("mixed separators and blank lines", MIXED.length === 3);
check("names survive mixing", MIXED.map((h) => h.name).join("") === "abc");

check("bare url gets an empty name", parseHostSpec("127.0.0.1:8089")[0].name === "");
// Pasting out of a YAML file or a markdown runbook is how these lists travel.
check("tolerates a list bullet", parseHostSpec("- a=127.0.0.1:8089")[0].name === "a");
check("tolerates an asterisk bullet", parseHostSpec("* a=127.0.0.1:8089")[0].name === "a");
// A URL may legitimately contain '=' in a query string; a name never does.
check(
  "splits on the FIRST equals only",
  parseHostSpec("x=http://h:80/?a=b")[0].name === "x"
);
check("drops unparseable entries", parseHostSpec("a=ftp://nope\nb=127.0.0.1:8090").length === 1);
check("empty input is an empty list", parseHostSpec("").length === 0);

console.log("normalizeHosts");
// The URL is the identity, not the name: the same agent listed twice would be
// polled twice and appear twice in the fleet table.
const DUPES = normalizeHosts([
  { name: "first", url: "http://127.0.0.1:8089" },
  { name: "second", url: "http://127.0.0.1:8089/" },
  { name: "other", url: "127.0.0.1:8090" },
]);
check("dedupes by normalised url", DUPES.length === 2);
check("first entry wins", DUPES[0].name === "first");
check("normalises while deduping", DUPES[1].url === "http://127.0.0.1:8090");
check("drops entries with no usable url", normalizeHosts([{ name: "x", url: "" }]).length === 0);
check("survives a non-array", normalizeHosts(null).length === 0);
check("survives junk entries", normalizeHosts([null, undefined, 3]).length === 0);

console.log("hostLabel");
const NAMED = { name: "prod-web-1", url: "http://127.0.0.1:8089" };
const BARE = { name: "", url: "http://127.0.0.1:8089" };
// The configured name has to win: it is the only label available for a host
// whose tunnel is down, because an unreachable agent reports no agent_id.
check("configured name wins over agent_id", hostLabel(NAMED, "i-0abc") === "prod-web-1");
check("falls back to agent_id", hostLabel(BARE, "i-0abc") === "i-0abc");
check("falls back to the address", hostLabel(BARE, "") === "127.0.0.1:8089");
check("survives no host", hostLabel(null, "x") === "");

console.log("toHostSpec round trip");
const SPEC = "prod-1=http://127.0.0.1:8089\nprod-2=http://127.0.0.1:8090";
check("round trips", toHostSpec(parseHostSpec(SPEC)) === SPEC);
check("bare urls round trip", toHostSpec(parseHostSpec("127.0.0.1:8089")) === "http://127.0.0.1:8089");

console.log(failed === 0 ? "\nOK — all host checks passed" : `\n${failed} check(s) failed`);
process.exit(failed ? 1 : 0);
