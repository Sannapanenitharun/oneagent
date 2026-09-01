import React, { useState, useEffect, useMemo } from "react";
import {
  LineChart, Line, AreaChart, Area, BarChart, Bar,
  XAxis, YAxis, CartesianGrid, Tooltip, ResponsiveContainer,
} from "recharts";
import {
  Activity, AlertTriangle, XCircle, Server,
  Cpu, MemoryStick, Gauge, Search, Bell, ChevronRight,
  LayoutDashboard, ScrollText, Waypoints, HardDrive,
  Network, PlugZap, Pause, Play, Sun, Moon, Monitor,
  ChevronUp, ChevronDown, X, Braces, Settings, Box,
} from "lucide-react";

import { useSnapshot, useHostHealth, useAllSnapshots } from "./api";
import { useBackendFleet, useBackendSnapshot, chooseBackendFallback } from "./backend";
import { loadHosts, saveHosts, parseHostSpec, readHostSpec, toHostSpec, hostLabel, configuredHosts } from "./hosts";
import { useTheme } from "./useTheme";
import {
  deriveTraces, deriveEdges, layoutTopology, deriveTopologyNodes,
  deriveLogs, deriveInfra, deriveTraffic, deriveAllSeries, globalStats,
  fmtRps, hostMetricPanels, fmtMetric, MAX_SERIES_PER_PANEL, flattenFields, hostRow, fmtAge,
  hostStatus, statusRank, deriveContainers, containerLogCounts, fmtBytes,
} from "./adapters";

const statusColor = { healthy: "var(--good)", degraded: "var(--warn)", down: "var(--crit)" };
const lvlColor = { ERROR: "var(--crit)", WARN: "var(--warn)", INFO: "var(--ink-3)", DEBUG: "var(--ink-4)", TRACE: "var(--ink-4)" };

// Categorical series slots, in the validated order. Each theme supplies its
// own six hues (see index.css) — the same hex cannot serve both grounds, since
// a colour bright enough to read on near-black is too pale on white.
// Assigned in fixed order and never generated: a seventh service reuses slot 1
// rather than inventing a hue nobody checked.
const SERVICE_PALETTE = ["var(--s1)", "var(--s2)", "var(--s3)", "var(--s4)", "var(--s5)", "var(--s6)"];

// Deterministic per-service colour, the same idea as Jaeger's colorGenerator:
// hash the name to a fixed slot so a service keeps its colour across the
// waterfall, flame graph and topology views, and across reloads.
function serviceColor(name) {
  let hash = 0;
  for (let i = 0; i < (name || "").length; i++) hash = (hash * 31 + name.charCodeAt(i)) >>> 0;
  return SERVICE_PALETTE[hash % SERVICE_PALETTE.length];
}

function StatusDot({ status }) {
  return (
    <span
      className="inline-block w-2 h-2 rounded-full mr-2 flex-shrink-0"
      style={{ background: statusColor[status], boxShadow: status !== "healthy" ? `0 0 8px ${statusColor[status]}` : "none" }}
    />
  );
}

function Panel({ title, right, children, className = "" }) {
  return (
    <div className={`bg-[var(--surface)] border border-[var(--n4)] rounded-lg ${className}`}>
      <div className="flex items-center justify-between px-4 py-3 border-b border-[var(--n4)]">
        <h3 className="text-[11px] tracking-widest uppercase text-[var(--ink-3)] font-mono">{title}</h3>
        {right}
      </div>
      <div className="p-4">{children}</div>
    </div>
  );
}

function KpiTile({ icon: Icon, label, value, sub, tone = "normal" }) {
  const toneColor = tone === "bad" ? "var(--crit)" : tone === "warn" ? "var(--warn)" : "var(--ink)";
  return (
    <div className="bg-[var(--surface)] border border-[var(--n4)] rounded-lg px-4 py-3 flex flex-col gap-1">
      <div className="flex items-center gap-2 text-[var(--ink-3)]">
        <Icon size={13} />
        <span className="text-[10px] tracking-widest uppercase font-mono">{label}</span>
      </div>
      <div className="font-mono text-2xl leading-none" style={{ color: toneColor }}>{value}</div>
      {sub && <div className="text-[11px] text-[var(--ink-3)]">{sub}</div>}
    </div>
  );
}

function ChartTooltip({ active, payload, label, unit }) {
  if (!active || !payload || !payload.length) return null;
  return (
    <div className="bg-[var(--n2)] border border-[var(--n5)] rounded px-2 py-1.5 text-[11px] font-mono">
      <div className="text-[var(--ink-3)]">{label}</div>
      <div className="text-[var(--ink)]">{payload[0].value}{unit}</div>
    </div>
  );
}

// A multi-series host metric panel: one line per label combination, a legend
// naming every one of them, and a tooltip listing all series at the hovered
// instant rather than only the line under the cursor — on a per-device chart
// the comparison between devices is the reason to look at it.
function MultiSeriesTooltip({ active, payload, label, unit }) {
  if (!active || !payload || !payload.length) return null;
  const shown = payload.filter((p) => p.value != null).sort((a, b) => b.value - a.value);
  if (!shown.length) return null;
  return (
    <div className="bg-[var(--surface)] border border-[var(--n5)] rounded px-2.5 py-2 text-[11px] font-mono shadow-lg">
      <div className="text-[var(--ink-3)] mb-1">{label}</div>
      <div className="flex flex-col gap-0.5">
        {shown.map((p) => (
          <div key={p.dataKey} className="flex items-center gap-2 justify-between">
            <span className="flex items-center gap-1.5 min-w-0">
              <span className="w-2 h-2 rounded-full flex-shrink-0" style={{ background: p.color }} />
              {/* Text stays on ink tokens; the swatch beside it carries the
                  identity. Colouring the label too makes a legend of values. */}
              <span className="text-[var(--ink-2)] truncate">{p.dataKey}</span>
            </span>
            <span className="text-[var(--ink)] tabular-nums">{fmtMetric(p.value, unit)}</span>
          </div>
        ))}
      </div>
    </div>
  );
}

function MetricPanel({ panel, height = 170 }) {
  const { title, unit, domain, needs, rows, keys, series, points } = panel;

  let body;
  if (!series.length) {
    body = <EmptyHint>needs {needs}</EmptyHint>;
  } else if (points < 2) {
    // A cumulative counter yields its first rate only on the second sample.
    body = <EmptyHint>waiting for a second sample</EmptyHint>;
  } else {
    body = (
      <>
        <ResponsiveContainer width="100%" height={height}>
          <LineChart data={rows} margin={{ top: 4, right: 8, left: 0, bottom: 0 }}>
            <CartesianGrid stroke="var(--n3)" vertical={false} />
            <XAxis dataKey="time" tick={{ fill: "var(--ink-5)", fontSize: 10 }}
              axisLine={{ stroke: "var(--n4)" }} tickLine={false} minTickGap={28} />
            <YAxis
              width={52} domain={domain || ["auto", "auto"]}
              tick={{ fill: "var(--ink-5)", fontSize: 10 }}
              axisLine={false} tickLine={false}
              tickFormatter={(v) => fmtMetric(v, unit)}
            />
            <Tooltip content={<MultiSeriesTooltip unit={unit} />} cursor={{ stroke: "var(--n5)" }} />
            {keys.map((k, i) => (
              <Line
                key={k} type="monotone" dataKey={k}
                stroke={SERVICE_PALETTE[i % SERVICE_PALETTE.length]}
                strokeWidth={1.5} dot={false} isAnimationActive={false}
                // A gap means "no sample", which must not be drawn as a line
                // through it — on an errors chart that reads as zero errors.
                connectNulls={false}
              />
            ))}
          </LineChart>
        </ResponsiveContainer>
        {/* Always present for 2+ series: identity must never be colour alone. */}
        <div className="flex flex-wrap gap-x-3 gap-y-1 mt-2">
          {keys.map((k, i) => (
            <span key={k} className="flex items-center gap-1.5 text-[10px] font-mono text-[var(--ink-3)]">
              <span className="w-2 h-2 rounded-full flex-shrink-0"
                style={{ background: SERVICE_PALETTE[i % SERVICE_PALETTE.length] }} />
              {k}
            </span>
          ))}
        </div>
      </>
    );
  }
  return <Panel title={title}>{body}</Panel>;
}

function GaugeBar({ value, warn = 70, bad = 90 }) {
  const color = value >= bad ? "var(--crit)" : value >= warn ? "var(--warn)" : "var(--accent)";
  return (
    <div className="flex items-center gap-2">
      <div className="w-20 h-1.5 rounded-full bg-[var(--n3)] overflow-hidden">
        <div className="h-full rounded-full" style={{ width: `${Math.min(100, value)}%`, background: color }} />
      </div>
      <span className="font-mono text-[11px] w-8 text-right" style={{ color }}>{value}%</span>
    </div>
  );
}

// Shown wherever the UI has a view but the agent has no data to fill it.
// Naming the exact missing capability beats an empty chart: it turns "this is
// broken" into "this needs X", which is actionable.
function NotWired({ title, needs, why }) {
  return (
    <Panel title={title}>
      <div className="flex flex-col items-center text-center py-8 gap-2">
        <PlugZap size={20} className="text-[var(--ink-5)]" />
        <div className="text-[13px] text-[var(--ink-2)]">Not available from the agent yet</div>
        <div className="text-[12px] text-[var(--ink-3)] max-w-md leading-relaxed">{why}</div>
        <div className="text-[11px] font-mono text-[var(--accent)] mt-1">needs: {needs}</div>
      </div>
    </Panel>
  );
}

function EmptyHint({ children }) {
  return <div className="text-[var(--ink-5)] text-[12px] py-6 text-center font-mono">{children}</div>;
}

// Three explicit options rather than a two-state toggle, so "follow my OS"
// stays reachable after someone has picked a side once.
function ThemeSwitch({ theme, setTheme }) {
  const opts = [
    { id: "light", icon: Sun, label: "Light" },
    { id: "system", icon: Monitor, label: "System" },
    { id: "dark", icon: Moon, label: "Dark" },
  ];
  return (
    <div className="flex items-center rounded border border-[var(--n4)] overflow-hidden" role="group" aria-label="Colour theme">
      {opts.map(({ id, icon: Icon, label }) => {
        const active = theme === id;
        return (
          <button
            key={id}
            onClick={() => setTheme(id)}
            title={label}
            aria-pressed={active}
            className="w-7 h-7 flex items-center justify-center"
            style={{
              background: active ? "var(--accent)" : "transparent",
              color: active ? "var(--surface)" : "var(--ink-3)",
            }}
          >
            <Icon size={13} />
          </button>
        );
      })}
    </div>
  );
}

// TopologyGraph draws the derived service map.
//
// Three things are encoded, because a map where every dependency looks
// identical answers none of the questions a map is opened to answer:
//
//   thickness  call volume, relative to the busiest edge — which path
//              carries the traffic
//   colour     failure, on the edge and on the node it points at — which
//              path is breaking, following the convention every commercial
//              map uses of colouring the failing call rather than only the
//              failing service
//   shape      whether the far side is instrumented. A dashed node is an
//              inferred dependency: a database, queue or third-party API
//              that never reported a span and is known only because
//              something called it.
function TopologyGraph({ nodes, edges, positions, height, selected, onSelect }) {
  if (!nodes.length) return <EmptyHint>no services — send spans to the agent&apos;s OTLP receiver</EmptyHint>;
  const maxCalls = Math.max(1, ...edges.map((e) => e.calls));

  return (
    <svg viewBox={`0 0 460 ${height}`} className="w-full" style={{ height: Math.max(220, height + 30) }}>
      <defs>
        <marker id="arrow" markerWidth="8" markerHeight="8" refX="7" refY="4" orient="auto">
          <path d="M0,0 L8,4 L0,8 z" fill="var(--n5)" />
        </marker>
        <marker id="arrow-bad" markerWidth="8" markerHeight="8" refX="7" refY="4" orient="auto">
          <path d="M0,0 L8,4 L0,8 z" fill="var(--crit)" />
        </marker>
      </defs>

      {edges.map((e, i) => {
        const a = positions[e.from], b = positions[e.to];
        if (!a || !b) return null;
        const failing = e.errPct > 1;
        // Square-rooted so one very busy edge does not flatten every other
        // one to a hairline; the eye reads area, not magnitude.
        const w = 1 + 3 * Math.sqrt(e.calls / maxCalls);
        const dim = selected && e.from !== selected && e.to !== selected;
        return (
          <line
            key={i} x1={a.x} y1={a.y} x2={b.x} y2={b.y}
            stroke={failing ? "var(--crit)" : "var(--n5)"}
            strokeWidth={w}
            strokeDasharray={e.virtual ? "4 3" : undefined}
            opacity={dim ? 0.25 : 1}
            markerEnd={failing ? "url(#arrow-bad)" : "url(#arrow)"}
          >
            <title>
              {`${e.from} → ${e.to}\n${e.calls} call${e.calls === 1 ? "" : "s"}, ${e.errPct}% errors\np50 ${e.p50}ms · p99 ${e.p99}ms${e.virtual ? "\ninferred — the far side is not instrumented" : ""}`}
            </title>
          </line>
        );
      })}

      {nodes.map((s) => {
        const p = positions[s.id];
        if (!p) return null;
        const isSelected = selected === s.id;
        const tone = statusColor[s.status] || statusColor.healthy;
        const dim = selected && !isSelected &&
          !edges.some((e) => (e.from === selected && e.to === s.id) || (e.to === selected && e.from === s.id));
        return (
          <g key={s.id} transform={`translate(${p.x},${p.y})`} className="cursor-pointer"
             opacity={dim ? 0.35 : 1} onClick={() => onSelect(s.id)}>
            <title>
              {s.virtual
                ? `${s.label}\ninferred ${s.type} dependency — never reported a span\n${s.calls} inbound call${s.calls === 1 ? "" : "s"}, ${s.err}% errors`
                : `${s.label}\n${fmtRps(s.rps)} rps · ${s.err}% errors · p99 ${s.p99}ms`}
            </title>
            {s.status !== "healthy" && (
              <circle r="20" fill={tone} opacity="0.15">
                <animate attributeName="r" values="14;24;14" dur="2s" repeatCount="indefinite" />
                <animate attributeName="opacity" values="0.25;0;0.25" dur="2s" repeatCount="indefinite" />
              </circle>
            )}
            <circle
              r="13"
              fill="var(--n2)"
              stroke={isSelected ? "var(--accent)" : tone}
              strokeWidth={isSelected ? 2.5 : 1.5}
              strokeDasharray={s.virtual ? "3 2" : undefined}
            />
            {s.virtual
              ? <VirtualGlyph type={s.type} tone={tone} />
              : <circle r="3.5" fill={tone} />}
            <text y="26" textAnchor="middle" className="font-mono" fontSize="9.5"
                  fill={isSelected ? "var(--accent)" : "var(--ink-3)"}>
              {s.label.length > 22 ? `${s.label.slice(0, 21)}…` : s.label}
            </text>
          </g>
        );
      })}
    </svg>
  );
}

// VirtualGlyph marks an inferred node by what it is, so a datastore is
// distinguishable from a queue at a glance rather than only by reading labels.
function VirtualGlyph({ type, tone }) {
  if (type === "database") {
    return (
      <g fill="none" stroke={tone} strokeWidth="1.2">
        <ellipse cx="0" cy="-3" rx="4.5" ry="1.8" />
        <path d="M-4.5,-3 L-4.5,3 A4.5,1.8 0 0 0 4.5,3 L4.5,-3" />
      </g>
    );
  }
  if (type === "messaging") {
    return (
      <g fill="none" stroke={tone} strokeWidth="1.2">
        <rect x="-5" y="-3.5" width="10" height="7" rx="1" />
        <path d="M-5,-3.5 L0,0.5 L5,-3.5" />
      </g>
    );
  }
  return <circle r="3.5" fill="none" stroke={tone} strokeWidth="1.2" />;
}

function ServiceDetail({ svc, edges }) {
  if (!svc) return <EmptyHint>no service selected</EmptyHint>;
  const related = (edges || []).filter((e) => e.from === svc.id || e.to === svc.id);
  return (
    <>
      <div className="flex items-center justify-between mb-3">
        <span className="font-mono text-base">{svc.label}</span>
        <span className="text-[10px] font-mono uppercase px-2 py-0.5 rounded" style={{ color: statusColor[svc.status], background: `color-mix(in srgb, ${statusColor[svc.status]} 12%, transparent)` }}>
          {svc.status}
        </span>
      </div>
      <div className="grid grid-cols-2 gap-3 text-sm mb-4">
        <div><div className="text-[10px] text-[var(--ink-3)] font-mono uppercase">p50</div><div className="font-mono">{svc.p50}ms</div></div>
        <div><div className="text-[10px] text-[var(--ink-3)] font-mono uppercase">p99</div><div className="font-mono" style={{ color: svc.p99 > 300 ? "var(--crit)" : "var(--ink)" }}>{svc.p99}ms</div></div>
        <div><div className="text-[10px] text-[var(--ink-3)] font-mono uppercase">req/s</div><div className="font-mono">{fmtRps(svc.rps)}</div></div>
        <div><div className="text-[10px] text-[var(--ink-3)] font-mono uppercase">error rate</div><div className="font-mono" style={{ color: svc.err > 1 ? "var(--crit)" : "var(--ink)" }}>{svc.err}%</div></div>
      </div>
      {related.length > 0 && (
        <>
          <div className="text-[10px] text-[var(--ink-3)] font-mono uppercase mb-1.5">Upstream / Downstream</div>
          <div className="flex flex-col gap-1">
            {related.map((e, i) => (
              <div key={i} className="flex items-center gap-1.5 text-[11px] font-mono text-[var(--ink-2)]">
                <span className={e.from === svc.id ? "text-[var(--accent)]" : ""}>{e.from}</span>
                <ChevronRight size={11} className="text-[var(--ink-5)]" />
                <span className={e.to === svc.id ? "text-[var(--accent)]" : ""}>{e.to}</span>
                {/* The traffic on the dependency, not just its existence —
                    which of a service's callees is busy, and which is failing,
                    is the question this list is read to answer. */}
                <span className="ml-auto tabular-nums text-[10px] text-[var(--ink-4)]">
                  {e.calls}×
                </span>
                <span className="tabular-nums text-[10px]"
                      style={{ color: e.errPct > 1 ? "var(--crit)" : "var(--ink-4)" }}>
                  {e.errPct}%
                </span>
                <span className="tabular-nums text-[10px] text-[var(--ink-4)]">p99 {e.p99}ms</span>
                {e.virtual && (
                  <span className="text-[9px] uppercase tracking-wide text-[var(--ink-5)]" title="not instrumented — inferred from the caller's span attributes">
                    inferred
                  </span>
                )}
              </div>
            ))}
          </div>
        </>
      )}
    </>
  );
}

function ServiceTable({ services, selected, setSelected }) {
  if (!services.length) return <EmptyHint>no spans received yet</EmptyHint>;
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-[12px] font-mono">
        <thead>
          <tr className="text-[10px] text-[var(--ink-3)] uppercase tracking-wide text-left border-b border-[var(--n4)]">
            <th className="py-2 pr-4 font-normal">Service</th>
            <th className="py-2 pr-4 font-normal">Status</th>
            <th className="py-2 pr-4 font-normal">p50</th>
            <th className="py-2 pr-4 font-normal">p99</th>
            <th className="py-2 pr-4 font-normal">req/s</th>
            <th className="py-2 pr-4 font-normal">error rate</th>
          </tr>
        </thead>
        <tbody>
          {services.map((s) => (
            <tr
              key={s.id}
              onClick={() => setSelected?.(s.id)}
              className="border-b border-[var(--n1)] last:border-0 cursor-pointer hover:bg-[var(--n1)]"
              style={{ background: selected === s.id ? "color-mix(in srgb, var(--accent) 5%, transparent)" : "transparent" }}
            >
              <td className="py-2 pr-4" style={{ color: selected === s.id ? "var(--accent)" : "var(--ink)" }}>{s.label}</td>
              <td className="py-2 pr-4"><StatusDot status={s.status} />{s.status}</td>
              <td className="py-2 pr-4">{s.p50}ms</td>
              <td className="py-2 pr-4" style={{ color: s.p99 > 300 ? "var(--crit)" : "var(--ink)" }}>{s.p99}ms</td>
              <td className="py-2 pr-4">{fmtRps(s.rps)}</td>
              <td className="py-2 pr-4" style={{ color: s.err > 1 ? "var(--crit)" : "var(--ink)" }}>{s.err}%</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Views
// ---------------------------------------------------------------------------

// TracesEmpty replaces the span-derived panels on a host that has never had a
// span, which is the ordinary state until someone instruments an application.
//
// The copy it replaced said "enable traces.enabled and point an app at the
// agent's OTLP receiver" — advice that is wrong half the time it is shown,
// because traces.enabled is true by default and the receiver is already
// listening. What is actually missing is a producer, so this says that, and
// gives the address to point one at rather than making it the reader's job to
// find it in a config file.
function TracesEmpty() {
  return (
    <Panel title="Traces" className="lg:col-span-2">
      <div className="flex flex-col gap-2 py-0.5">
        <div className="text-[12.5px] font-mono text-[var(--ink-3)]">No spans received.</div>
        <div className="text-[11.5px] font-mono text-[var(--ink-4)] leading-relaxed">
          Rate, errors, duration and service health are all derived from spans.
          They appear here once an instrumented application sends to:
        </div>
        <div className="text-[11.5px] font-mono text-[var(--accent)] break-all">
          {window.location.origin}/v1/traces
        </div>
      </div>
    </Panel>
  );
}

// Overview summarises; it does not re-host other views' tables. Where a panel
// shows the same entity another view owns, clicking it navigates there rather
// than selecting in place — a selection that changes nothing on screen is a
// control that lies about what it does.
function OverviewView({ snap, d, openService, openHost, openLogs }) {
  const healthy = d.services.filter((s) => s.status === "healthy").length;
  const infra = d.infra[0];
  // Whether there is anything span-derived to draw at all. Not the same as
  // "this window is quiet": a host with no instrumented application never has
  // spans, and showing it four empty panels forever is not a state, it is a
  // permanent condition the page should name once and move on from.
  const hasTraces = (snap?.spans?.length || 0) > 0 || d.services.length > 0;

  return (
    <>
      {/* Deliberately does NOT restate rate, errors or p99 — those are the
          three charts immediately below, and a number sitting directly above
          its own chart is noise, not a summary. These are the facts the RED
          row cannot show: how much the agent is holding, and over what. */}
      <div className="grid grid-cols-2 md:grid-cols-4 gap-3 mb-4">
        <KpiTile icon={Server} label="Services" value={d.services.length ? `${healthy}/${d.services.length}` : "—"}
          tone={d.services.length && healthy < d.services.length ? "warn" : "normal"} sub="healthy / seen" />
        <KpiTile icon={Waypoints} label="Spans" value={(snap?.spans?.length || 0).toLocaleString()}
          sub={`in the last ${Math.round((snap?.retain_sec || 900) / 60)} min`} />
        <KpiTile icon={Gauge} label="Series" value={d.allSeries.length.toLocaleString()}
          tone={d.seriesDropped > 0 ? "warn" : "normal"} sub={d.seriesDropped > 0 ? `${d.seriesDropped} refused` : "metric streams held"} />
        <KpiTile
          icon={Activity}
          label="Envelopes"
          value={d.envelopes.toLocaleString()}
          sub={
            Number.isFinite(d.envelopesPerSec)
              ? `${d.envelopesPerSec}/s ${d.rateBasis}`
              : "rate unavailable"
          }
        />
      </div>

      {/* Two different caps produce this number, and they have different
          fixes. An agent-sourced snapshot was truncated by the agent's own
          in-memory store, which is configurable. A backend-sourced one was
          truncated by the per-host limit on a snapshot query, which is not —
          so naming dashboard.max_series there sends the operator to a knob
          that cannot affect what they are looking at. */}
      {d.seriesDropped > 0 && (
        <div className="mb-4 border border-[var(--warn)] border-l-2 rounded px-3 py-2 text-[12px] text-[var(--ink-2)] bg-[color-mix(in srgb, var(--warn) 4%, transparent)]">
          {snap?.source === "backend" ? (
            <>
              {d.seriesDropped} series refused — this host reports more distinct series than one
              snapshot carries, so this view is incomplete. Every metric keeps a share, but each
              shows fewer devices and containers than exist. The cap is on the query and is not
              configurable; the fix is to collect less at the agent, starting with{" "}
              <span className="font-mono text-[var(--warn)]">metrics.network.exclude</span> for
              veth and bridge interfaces.
            </>
          ) : (
            <>
              {d.seriesDropped} series refused — the agent's in-memory cap was reached, so this
              view is incomplete. Raise{" "}
              <span className="font-mono text-[var(--warn)]">dashboard.max_series</span> or narrow
              what is collected.
            </>
          )}
        </div>
      )}

      {/* Everything in the RED row and in Service Health is derived from spans,
          so with none received they are four panels of empty axes — most of a
          screen spent saying nothing. Collapsed into one honest notice
          instead, and restored the moment a span arrives. */}
      {/* RED hero row. Rate and errors on the left, duration on the right —
          the convention every platform surveyed follows, and the order an
          operator actually asks the questions in: is it serving, is it
          broken, is it slow. These are taller than everything below them so
          the layout itself says which panels matter; a uniform grid makes you
          scan all of them equally, every time. */}
      {hasTraces && (
      <div className="grid grid-cols-1 lg:grid-cols-5 gap-4">
        <Panel title="Rate" className="lg:col-span-2" right={<span className="text-[10px] font-mono text-[var(--ink-5)]">req/s</span>}>
          {d.traffic.rps.length > 1 ? (
            <ResponsiveContainer width="100%" height={210}>
              <AreaChart data={d.traffic.rps}>
                <defs>
                  <linearGradient id="rpsFill" x1="0" y1="0" x2="0" y2="1">
                    <stop offset="0%" stopColor="var(--accent)" stopOpacity={0.35} />
                    <stop offset="100%" stopColor="var(--accent)" stopOpacity={0} />
                  </linearGradient>
                </defs>
                <CartesianGrid stroke="var(--n3)" vertical={false} />
                <XAxis dataKey="time" tick={{ fill: "var(--ink-5)", fontSize: 10 }} axisLine={{ stroke: "var(--n4)" }} tickLine={false} minTickGap={24} />
                <YAxis width={34} tick={{ fill: "var(--ink-5)", fontSize: 10 }} axisLine={false} tickLine={false} />
                <Tooltip content={<ChartTooltip unit=" req/s" />} />
                <Area type="monotone" dataKey="value" stroke="var(--accent)" strokeWidth={2} fill="url(#rpsFill)" />
              </AreaChart>
            </ResponsiveContainer>
          ) : <EmptyHint>needs spans</EmptyHint>}
        </Panel>

        <Panel title="Errors" className="lg:col-span-1" right={<span className="text-[10px] font-mono text-[var(--ink-5)]">per min</span>}>
          {d.traffic.errors.length > 1 ? (
            <ResponsiveContainer width="100%" height={210}>
              <BarChart data={d.traffic.errors}>
                <CartesianGrid stroke="var(--n3)" vertical={false} />
                <XAxis dataKey="time" tick={{ fill: "var(--ink-5)", fontSize: 10 }} axisLine={{ stroke: "var(--n4)" }} tickLine={false} minTickGap={24} />
                <YAxis width={26} tick={{ fill: "var(--ink-5)", fontSize: 10 }} axisLine={false} tickLine={false} allowDecimals={false} />
                <Tooltip content={<ChartTooltip unit=" errs" />} />
                <Bar dataKey="value" fill="var(--crit)" radius={[2, 2, 0, 0]} />
              </BarChart>
            </ResponsiveContainer>
          ) : <EmptyHint>needs spans</EmptyHint>}
        </Panel>

        <Panel title="Duration" className="lg:col-span-2" right={<span className="text-[10px] font-mono text-[var(--ink-5)]">p99 ms</span>}>
          {d.traffic.latency.length > 1 ? (
            <ResponsiveContainer width="100%" height={210}>
              <LineChart data={d.traffic.latency}>
                <CartesianGrid stroke="var(--n3)" vertical={false} />
                <XAxis dataKey="time" tick={{ fill: "var(--ink-5)", fontSize: 10 }} axisLine={{ stroke: "var(--n4)" }} tickLine={false} minTickGap={24} />
                <YAxis width={38} tick={{ fill: "var(--ink-5)", fontSize: 10 }} axisLine={false} tickLine={false} />
                <Tooltip content={<ChartTooltip unit="ms" />} />
                <Line type="monotone" dataKey="value" stroke="var(--warn)" strokeWidth={2} dot={false} />
              </LineChart>
            </ResponsiveContainer>
          ) : <EmptyHint>needs spans</EmptyHint>}
        </Panel>
      </div>
      )}

      {/* Secondary: context for the row above, deliberately shorter. */}
      <div className="grid grid-cols-1 lg:grid-cols-3 gap-4 mt-4">
        {hasTraces ? (
        <Panel title="Service Health" className="lg:col-span-2">
          {d.services.length ? (
            <div className="flex flex-col gap-1.5">
              {d.services.map((s) => (
                <button key={s.id} onClick={() => openService(s.id)} title={`Open ${s.label} in Service Topology`}
                  className="group flex items-center justify-between px-2.5 py-2 rounded border border-[var(--n3)] text-left hover:border-[var(--accent)]">
                  <span className="flex items-center font-mono text-[12.5px]"><StatusDot status={s.status} />{s.label}</span>
                  <span className="flex items-center gap-3 text-[11px] font-mono text-[var(--ink-3)]">
                    <span>{fmtRps(s.rps)} rps</span>
                    <span style={{ color: s.p99 > 300 ? "var(--crit)" : "var(--ink-3)" }}>{s.p99}ms p99</span>
                    <ChevronRight size={12} className="text-[var(--ink-5)] group-hover:text-[var(--accent)]" />
                  </span>
                </button>
              ))}
            </div>
          ) : (
            <EmptyHint>no services seen in this window</EmptyHint>
          )}
        </Panel>
        ) : (
          <TracesEmpty />
        )}

        <Panel title="This Host" right={infra && (<button onClick={openHost} className="text-[10px] font-mono text-[var(--accent)]">open ↗</button>)}>
          {infra ? (
            <>
              <div className="flex items-center justify-between mb-3">
                <span className="font-mono text-base">{infra.host}</span>
                <span className="text-[10px] font-mono text-[var(--ink-3)]">{infra.role}</span>
              </div>
              <div className="flex flex-col gap-2.5">
                <div className="flex items-center justify-between">
                  <span className="flex items-center gap-1.5 text-[11px] text-[var(--ink-3)] font-mono"><Cpu size={12} /> CPU</span>
                  <GaugeBar value={infra.cpu} />
                </div>
                <div className="flex items-center justify-between">
                  <span className="flex items-center gap-1.5 text-[11px] text-[var(--ink-3)] font-mono"><MemoryStick size={12} /> Memory</span>
                  <GaugeBar value={infra.mem} />
                </div>
                <div className="flex items-center justify-between">
                  <span className="flex items-center gap-1.5 text-[11px] text-[var(--ink-3)] font-mono"><HardDrive size={12} /> Disk (worst)</span>
                  <GaugeBar value={infra.disk} />
                </div>
                <div className="flex items-center justify-between text-[11px] font-mono">
                  <span className="text-[var(--ink-3)]">load 1m</span><span>{infra.load1}</span>
                </div>
              </div>
            </>
          ) : <EmptyHint>no host metrics — enable metrics.enabled</EmptyHint>}
        </Panel>
      </div>

      <div className="mt-4">
        <Panel title="Live Log Stream" right={d.logs.length > 0 && (<button onClick={openLogs} className="text-[10px] font-mono text-[var(--accent)]">open explorer ↗</button>)}>
          {d.logs.length ? (
            <div className="flex flex-col gap-1 max-h-[220px] overflow-y-auto font-mono text-[11.5px]">
              {d.logs.slice(0, 10).map((l, i) => (
                <div key={i} className="flex gap-3 py-1 border-b border-[var(--n1)] last:border-0">
                  <span className="text-[var(--ink-5)] flex-shrink-0">{l.t}</span>
                  <span className="w-12 flex-shrink-0 font-semibold" style={{ color: lvlColor[l.lvl] }}>{l.lvl}</span>
                  <span className="text-[var(--accent)] flex-shrink-0 w-28 truncate">{l.svc}</span>
                  <span className="text-[var(--ink-2)] truncate">{l.msg}</span>
                </div>
              ))}
            </div>
          ) : (
            <EmptyHint>
              no log lines — enable <span className="text-[var(--accent)]">logs.enabled</span> with a matching path in logs.paths
            </EmptyHint>
          )}
        </Panel>
      </div>
    </>
  );
}

// Log detail. Three tabs, and deliberately not the two more a hosted backend
// offers: there is no "metrics at this instant" correlation here because
// nothing links a log line to a metric series, and inventing that link would
// be worse than not offering it.
function LogDetail({ log, logs, index, onClose, onMove }) {
  const [tab, setTab] = useState("overview");

  // Arrow keys move between lines, which is how you actually read a log —
  // scanning down from the one that caught your eye.
  useEffect(() => {
    const onKey = (e) => {
      // The filter box sits directly above this panel, so a global handler
      // that ignores the focused element eats letters as you type: j and k
      // would never reach the input, and "jenkins" would arrive as "enins".
      const el = e.target;
      const typing = el && (el.tagName === "INPUT" || el.tagName === "TEXTAREA" || el.isContentEditable);
      if (e.key === "Escape") {
        if (!typing) onClose();
        return;
      }
      if (typing || e.metaKey || e.ctrlKey || e.altKey) return;
      if (e.key === "ArrowDown" || e.key === "j") { e.preventDefault(); onMove(1); }
      if (e.key === "ArrowUp" || e.key === "k") { e.preventDefault(); onMove(-1); }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose, onMove]);

  const fields = log.structured ? flattenFields(log.structured.value) : [];
  // Neighbours from the same file, which is what makes a line make sense —
  // logs are read in sequence, not in isolation.
  const context = logs
    .map((l, i) => ({ l, i }))
    .filter(({ l }) => l.src === log.src)
    .filter(({ i }) => Math.abs(i - index) <= 6);

  const tabs = [
    { id: "overview", label: log.structured ? `Fields ${fields.length}` : "Overview" },
    { id: "raw", label: "Raw" },
    { id: "context", label: `Context ${context.length}` },
  ];

  return (
    <div className="border border-[var(--n4)] rounded-lg bg-[var(--surface)] flex flex-col max-h-[560px]">
      <div className="flex items-center justify-between px-4 py-2.5 border-b border-[var(--n4)]">
        <div className="flex items-center gap-2 min-w-0">
          <span className="text-[11px] tracking-widest uppercase text-[var(--ink-3)] font-mono">Log detail</span>
          <span className="text-[10px] font-mono px-1.5 py-0.5 rounded" style={{ color: lvlColor[log.lvl], border: `1px solid ${lvlColor[log.lvl]}` }}>{log.lvl}</span>
        </div>
        <div className="flex items-center gap-1">
          <button onClick={() => onMove(-1)} title="Previous (↑)"
            className="px-1.5 py-1 rounded text-[var(--ink-3)] hover:text-[var(--ink)] hover:bg-[var(--n2)]"><ChevronUp size={13} /></button>
          <button onClick={() => onMove(1)} title="Next (↓)"
            className="px-1.5 py-1 rounded text-[var(--ink-3)] hover:text-[var(--ink)] hover:bg-[var(--n2)]"><ChevronDown size={13} /></button>
          <button onClick={onClose} title="Close (Esc)"
            className="px-1.5 py-1 rounded text-[var(--ink-3)] hover:text-[var(--ink)] hover:bg-[var(--n2)]"><X size={13} /></button>
        </div>
      </div>

      <div className="flex items-center gap-1 px-3 border-b border-[var(--n4)]">
        {tabs.map((t) => (
          <button key={t.id} onClick={() => setTab(t.id)}
            className="px-3 py-1.5 text-[11px] font-mono -mb-px border-b-2"
            style={{ color: tab === t.id ? "var(--ink)" : "var(--ink-3)", borderColor: tab === t.id ? "var(--accent)" : "transparent" }}>
            {t.label}
          </button>
        ))}
      </div>

      <div className="overflow-y-auto p-4 font-mono text-[11px]">
        {tab === "overview" && (
          <div className="flex flex-col gap-3">
            <FieldRow label="timestamp" value={new Date(log.tms).toISOString()} />
            <FieldRow label="source" value={log.src} />
            {log.labels && Object.entries(log.labels).map(([k, v]) => <FieldRow key={k} label={k} value={String(v)} />)}
            {log.structured?.prefix && <FieldRow label="prefix" value={log.structured.prefix} />}

            {fields.length > 0 ? (
              <div className="mt-1">
                <div className="text-[10px] tracking-widest uppercase text-[var(--ink-4)] mb-1.5">body fields</div>
                <div className="flex flex-col">
                  {fields.map((f) => <FieldRow key={f.path} label={f.path} value={f.value} dense />)}
                </div>
              </div>
            ) : (
              <div>
                <div className="text-[10px] tracking-widest uppercase text-[var(--ink-4)] mb-1.5">body</div>
                <div className="text-[var(--ink-2)] break-all whitespace-pre-wrap leading-relaxed">{log.msg}</div>
                <div className="text-[10px] text-[var(--ink-5)] mt-2">
                  plain text — no JSON object found in this line, so there are no fields to break out
                </div>
              </div>
            )}
          </div>
        )}

        {tab === "raw" && (
          <pre className="whitespace-pre-wrap break-all text-[var(--ink-2)] leading-relaxed">
            {log.structured ? JSON.stringify(log.structured.value, null, 2) : log.msg}
          </pre>
        )}

        {tab === "context" && (
          <div className="flex flex-col gap-0.5">
            {context.map(({ l, i }) => (
              <div key={i} className="flex gap-2 py-1 px-1.5 rounded"
                style={{ background: i === index ? "color-mix(in srgb, var(--accent) 12%, transparent)" : "transparent" }}>
                <span className="text-[var(--ink-5)] flex-shrink-0">{l.t}</span>
                <span className="text-[var(--ink-2)] break-all">{l.msg}</span>
              </div>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}

function FieldRow({ label, value, dense = false }) {
  return (
    <div className={`flex gap-3 ${dense ? "py-0.5" : ""} items-baseline`}>
      <span className="text-[var(--ink-4)] flex-shrink-0 w-56 truncate" title={label}>{label}</span>
      <span className="text-[var(--ink)] break-all min-w-0">{String(value)}</span>
    </div>
  );
}

function LogsView({ logs, scopeLabel = null, onClearScope }) {
  const [filter, setFilter] = useState("ALL");
  const [q, setQ] = useState("");
  const [selected, setSelected] = useState(null);

  const filtered = logs.filter((l) =>
    (filter === "ALL" || l.lvl === filter) &&
    (q === "" || l.msg.toLowerCase().includes(q.toLowerCase()) || l.svc.toLowerCase().includes(q.toLowerCase()))
  );

  // The poll replaces the array every 5s, so an index would silently come to
  // point at a different line. Identity is the timestamp plus the message —
  // the agent assigns no log ID — and if that line has aged out, the panel
  // closes rather than showing a neighbour as though it were your selection.
  const selectedIdx = selected == null ? -1 : filtered.findIndex((l) => l.tms === selected.tms && l.msg === selected.msg);
  const current = selectedIdx >= 0 ? filtered[selectedIdx] : null;

  const move = (delta) => {
    if (selectedIdx < 0) return;
    const next = filtered[selectedIdx + delta];
    if (next) setSelected({ tms: next.tms, msg: next.msg });
  };

  return (
    <div className="flex flex-col gap-3">
    <Panel
      title="Log Explorer"
      right={
        <div className="flex items-center gap-2">
          {["ALL", "ERROR", "WARN", "INFO"].map((l) => (
            <button key={l} onClick={() => setFilter(l)}
              className="text-[10px] font-mono uppercase px-2 py-0.5 rounded border"
              style={{
                color: filter === l ? "var(--bg)" : lvlColor[l] || "var(--ink-3)",
                background: filter === l ? (lvlColor[l] || "var(--ink-3)") : "transparent",
                borderColor: lvlColor[l] || "var(--ink-5)",
              }}>
              {l}
            </button>
          ))}
        </div>
      }
    >
      {/* The narrowing is stated and removable. A view silently showing a
          subset is the reason someone concludes their logs stopped arriving. */}
      {scopeLabel && (
        <div className="flex items-center gap-2 mb-3 text-[11px]">
          <span className="text-[var(--ink-4)]">showing</span>
          <span className="font-mono px-1.5 py-0.5 rounded border border-[var(--n4)] bg-[var(--sunk)]">
            container/{scopeLabel}
          </span>
          <button onClick={() => onClearScope?.()}
            className="text-[var(--accent)] hover:underline">
            show all
          </button>
        </div>
      )}

      <div className="flex items-center gap-2 bg-[var(--sunk)] border border-[var(--n4)] rounded px-3 py-1.5 mb-3">
        <Search size={13} className="text-[var(--ink-3)]" />
        <input value={q} onChange={(e) => setQ(e.target.value)}
          placeholder="filter by message or source…"
          className="bg-transparent outline-none text-[12px] font-mono flex-1 text-[var(--ink)] placeholder:text-[var(--ink-5)]" />
      </div>
      <div className="text-[10px] font-mono text-[var(--ink-5)] mb-2">
        severity is classified from the line text — the agent forwards log lines verbatim and does not parse levels
      </div>
      <div className="flex flex-col gap-1 font-mono text-[12px] max-h-[520px] overflow-y-auto">
        {filtered.map((l, i) => {
          const isSel = i === selectedIdx;
          return (
            <button
              key={`${l.tms}-${i}`}
              onClick={() => setSelected(isSel ? null : { tms: l.tms, msg: l.msg })}
              aria-expanded={isSel}
              className="flex gap-3 py-1.5 px-1 -mx-1 rounded border-b border-[var(--n1)] last:border-0 items-center text-left hover:bg-[var(--n1)]"
              style={{ background: isSel ? "color-mix(in srgb, var(--accent) 10%, transparent)" : undefined }}
            >
              <span className="text-[var(--ink-5)] flex-shrink-0 w-16">{l.t}</span>
              <span className="w-12 flex-shrink-0 font-semibold" style={{ color: lvlColor[l.lvl] }}>{l.lvl}</span>
              <span className="text-[var(--accent)] flex-shrink-0 w-32 truncate" title={l.svc}>{l.svc}</span>
              <span className="text-[var(--ink-2)] truncate flex-1" title={l.msg}>{l.msg}</span>
              {/* Only advertised where it leads somewhere: a line with a JSON
                  body has fields to open, a plain syslog line mostly does not. */}
              {l.structured && <Braces size={11} className="text-[var(--ink-4)] flex-shrink-0" title="structured body" />}
            </button>
          );
        })}
        {!filtered.length && <EmptyHint>no logs match this filter</EmptyHint>}
      </div>
    </Panel>

    {current && (
      <LogDetail
        log={current} logs={filtered} index={selectedIdx}
        onClose={() => setSelected(null)} onMove={move}
      />
    )}
    </div>
  );
}

// The raw series explorer, and only that.
//
// This used to carry a CPU chart and a network chart too. Both are now in
// Infrastructure's host grid, plotted per state and per device instead of
// summed into one line — strictly more information in one place, so keeping
// reduced copies here would be the same numbers twice with no way to tell
// which was authoritative. The per-service table lives on Service Topology
// for the same reason: there, selecting a row actually does something.
function MetricsView({ d }) {
  return (
    <div className="flex flex-col gap-4">
      <div className="text-[11px] text-[var(--ink-4)] px-0.5">
        Every series the agent is currently holding, as collected. Host charts are
        in <span className="text-[var(--ink-3)]">Infrastructure</span>.
      </div>

      <Panel title={`All Series (${d.allSeries.length})`}>
        <div className="overflow-auto max-h-[420px]">
          <table className="w-full text-[12px] font-mono">
            <thead>
              <tr className="text-[10px] text-[var(--ink-3)] uppercase tracking-wide text-left border-b border-[var(--n4)] sticky top-0 bg-[var(--surface)]">
                <th className="py-2 pr-4 font-normal">Metric</th>
                <th className="py-2 pr-4 font-normal">Labels</th>
                <th className="py-2 pr-4 font-normal text-right">Latest</th>
                <th className="py-2 pr-4 font-normal text-right">Points</th>
              </tr>
            </thead>
            <tbody>
              {d.allSeries.map((s, i) => (
                <tr key={i} className="border-b border-[var(--n1)] last:border-0">
                  <td className="py-1.5 pr-4">
                    {s.name}
                    {s.cumulative && <span className="ml-2 text-[9px] px-1 py-0.5 rounded bg-[var(--n3)] text-[var(--ink-3)]">RATE</span>}
                  </td>
                  <td className="py-1.5 pr-4 text-[10.5px] text-[var(--ink-3)] max-w-[340px] truncate" title={s.labels}>{s.labels || "—"}</td>
                  <td className="py-1.5 pr-4 text-right">{s.latest.toFixed(2)}</td>
                  <td className="py-1.5 pr-4 text-right text-[var(--ink-3)]">{s.points}</td>
                </tr>
              ))}
            </tbody>
          </table>
          {!d.allSeries.length && <EmptyHint>no metrics yet</EmptyHint>}
        </div>
      </Panel>
    </div>
  );
}

function SpanWaterfall({ trace, onSelectSpan, selectedIdx }) {
  const maxDur = trace.duration;
  return (
    <div className="flex flex-col gap-2.5">
      {trace.spans.map((s, i) => (
        <div key={i} onClick={() => onSelectSpan(i)} className="cursor-pointer rounded px-1.5 py-1 -mx-1.5"
          style={{ background: selectedIdx === i ? "color-mix(in srgb, var(--accent) 5%, transparent)" : "transparent" }}>
          <div className="flex justify-between text-[11px] font-mono mb-1" style={{ paddingLeft: s.depth * 14 }}>
            <span className="flex items-center gap-1.5 truncate">
              {s.error && <AlertTriangle size={11} className="text-[var(--crit)] flex-shrink-0" />}
              <span style={{ color: serviceColor(s.svc) }}>{s.svc}</span>
              <span className="text-[var(--ink-5)] truncate">· {s.op}</span>
            </span>
            <span className="text-[var(--ink-3)] flex-shrink-0 pl-2">{s.dur}ms</span>
          </div>
          <div className="w-full h-2 bg-[var(--n1)] rounded-sm relative">
            <div className="absolute h-2 rounded-sm"
              style={{
                left: `${(s.start / maxDur) * 100}%`,
                width: `${Math.max((s.dur / maxDur) * 100, 1)}%`,
                background: s.error ? "var(--crit)" : serviceColor(s.svc),
              }} />
          </div>
        </div>
      ))}
    </div>
  );
}

function FlameGraph({ trace, onSelectSpan, selectedIdx }) {
  const maxDur = trace.duration;
  const maxDepth = Math.max(...trace.spans.map((s) => s.depth), 0);
  return (
    <div className="flex flex-col gap-0.5">
      {Array.from({ length: maxDepth + 1 }).map((_, depth) => (
        <div key={depth} className="relative h-7">
          {trace.spans.map((s, i) => ({ ...s, i })).filter((s) => s.depth === depth).map((s) => (
            <div key={s.i} onClick={() => onSelectSpan(s.i)} title={`${s.svc} · ${s.op} · ${s.dur}ms`}
              className="absolute top-0 h-6 rounded-sm flex items-center px-1.5 overflow-hidden cursor-pointer border"
              style={{
                left: `${(s.start / maxDur) * 100}%`,
                width: `${Math.max((s.dur / maxDur) * 100, 1.5)}%`,
                background: s.error ? "color-mix(in srgb, var(--crit) 20%, transparent)" : `color-mix(in srgb, ${serviceColor(s.svc)} 20%, transparent)`,
                borderColor: selectedIdx === s.i ? "var(--accent)" : s.error ? "color-mix(in srgb, var(--crit) 40%, transparent)" : `color-mix(in srgb, ${serviceColor(s.svc)} 40%, transparent)`,
              }}>
              <span className="text-[10px] font-mono truncate" style={{ color: s.error ? "var(--crit)" : serviceColor(s.svc) }}>{s.svc}</span>
            </div>
          ))}
        </div>
      ))}
      <div className="flex items-center gap-4 mt-2 pt-2 border-t border-[var(--n3)] flex-wrap">
        {[...new Set(trace.spans.map((s) => s.svc))].map((svc) => (
          <div key={svc} className="flex items-center gap-1.5">
            <span className="w-2 h-2 rounded-sm" style={{ background: serviceColor(svc) }} />
            <span className="text-[10px] text-[var(--ink-3)] font-mono">{svc}</span>
          </div>
        ))}
      </div>
    </div>
  );
}

function TracesView({ traces }) {
  const [selectedTrace, setSelectedTrace] = useState(null);
  const [mode, setMode] = useState("waterfall");
  const [selectedSpanIdx, setSelectedSpanIdx] = useState(0);

  const trace = traces.find((t) => t.id === selectedTrace) || traces[0];
  useEffect(() => { setSelectedSpanIdx(0); }, [trace?.id]);

  if (!traces.length) {
    return (
      <NotWired
        title="Traces"
        why="No spans have reached the agent. Set traces.enabled: true and point an instrumented app at the OTLP receiver on 127.0.0.1:4319 — agent-i-auto-instrument can do that for systemd-managed Node and Python services."
        needs="traces.enabled + an app sending OTLP"
      />
    );
  }

  const span = trace.spans[selectedSpanIdx] || trace.spans[0];

  return (
    <div className="grid grid-cols-1 lg:grid-cols-3 gap-4">
      <Panel title={`Recent Traces (${traces.length})`} className="lg:col-span-1">
        <div className="flex flex-col gap-1 max-h-[560px] overflow-y-auto">
          {traces.slice(0, 50).map((t) => (
            <button key={t.id} onClick={() => setSelectedTrace(t.id)}
              className="text-left px-2 py-2 rounded border"
              style={{ borderColor: trace.id === t.id ? "var(--accent)" : "var(--n3)", background: trace.id === t.id ? "color-mix(in srgb, var(--accent) 5%, transparent)" : "transparent" }}>
              <div className="flex items-center justify-between">
                <span className="font-mono text-[11px] text-[var(--accent)] truncate">{t.id.slice(0, 16)}</span>
                <span className="font-mono text-[11px]" style={{ color: t.status === "error" ? "var(--crit)" : "var(--good)" }}>{t.duration}ms</span>
              </div>
              <div className="text-[12px] mt-0.5 text-[var(--ink-2)] truncate">{t.op}</div>
              <div className="flex items-center justify-between">
                <span className="text-[10px] text-[var(--ink-3)] font-mono">{t.root} · {t.spans.length} spans</span>
                {t.status === "error" && <AlertTriangle size={11} className="text-[var(--crit)]" />}
              </div>
            </button>
          ))}
        </div>
      </Panel>

      <div className="lg:col-span-2 flex flex-col gap-4">
        <Panel
          title={`${mode === "waterfall" ? "Waterfall" : "Flame Graph"} · ${trace.id.slice(0, 16)}`}
          right={
            <div className="flex items-center gap-1">
              {["waterfall", "flame"].map((m) => (
                <button key={m} onClick={() => setMode(m)}
                  className="text-[10px] font-mono uppercase px-2 py-0.5 rounded"
                  style={{ color: mode === m ? "var(--bg)" : "var(--ink-3)", background: mode === m ? "var(--accent)" : "transparent" }}>
                  {m === "waterfall" ? "Waterfall" : "Flame Graph"}
                </button>
              ))}
            </div>
          }>
          {mode === "waterfall"
            ? <SpanWaterfall trace={trace} onSelectSpan={setSelectedSpanIdx} selectedIdx={selectedSpanIdx} />
            : <FlameGraph trace={trace} onSelectSpan={setSelectedSpanIdx} selectedIdx={selectedSpanIdx} />}
        </Panel>

        <Panel title="Span Detail" right={span?.error && <span className="text-[10px] font-mono text-[var(--crit)]">ERROR</span>}>
          {span && (
            <>
              <div className="flex items-center justify-between mb-2">
                <span className="font-mono text-[13px]" style={{ color: serviceColor(span.svc) }}>{span.svc}</span>
                <span className="text-[11px] font-mono text-[var(--ink-3)]">{span.dur}ms</span>
              </div>
              <div className="text-[12px] text-[var(--ink-2)] mb-3">{span.op}</div>
              <div className="text-[10px] font-mono text-[var(--ink-5)]">
                depth {span.depth} · starts +{span.start}ms into the trace
              </div>
            </>
          )}
        </Panel>
      </div>
    </div>
  );
}

function TopologyView({ d, selected, setSelected }) {
  // Inferred dependencies are nodes on the map but not services, so they are
  // added here rather than in deriveServices — see deriveTopologyNodes.
  const nodes = useMemo(() => deriveTopologyNodes(d.services, d.edges), [d.services, d.edges]);

  // Laid out twice on purpose. The height a graph needs depends on how many
  // nodes share its busiest column, and that is only known once it has been
  // laid out — so the first pass measures and the second uses the answer.
  // Cheap: these graphs are a handful of nodes, and it is memoised.
  const { positions, height } = useMemo(() => {
    const probe = layoutTopology(nodes, d.edges);
    const perColumn = {};
    for (const id of Object.keys(probe)) {
      const col = Math.round(probe[id].x);
      perColumn[col] = (perColumn[col] || 0) + 1;
    }
    const rows = Math.max(1, ...Object.values(perColumn));
    const h = Math.max(190, rows * 46);
    return { positions: layoutTopology(nodes, d.edges, 460, h), height: h };
  }, [nodes, d.edges]);

  const svc = nodes.find((s) => s.id === selected) || nodes[0];

  return (
    <div className="grid grid-cols-1 lg:grid-cols-3 gap-4">
      <Panel title="Service Topology" className="lg:col-span-2">
        <TopologyGraph nodes={nodes} edges={d.edges} positions={positions} height={height} selected={svc?.id} onSelect={setSelected} />
        <div className="flex items-center justify-between mt-3 pt-3 border-t border-[var(--n3)]">
          <div className="flex items-center gap-4">
            {["healthy", "degraded"].map((s) => (
              <div key={s} className="flex items-center gap-1.5">
                <span className="w-2 h-2 rounded-full" style={{ background: statusColor[s] }} />
                <span className="text-[10px] text-[var(--ink-3)] font-mono uppercase">{s}</span>
              </div>
            ))}
            <div className="flex items-center gap-1.5">
              <span className="w-2 h-2 rounded-full border border-dashed" style={{ borderColor: "var(--ink-4)" }} />
              <span className="text-[10px] text-[var(--ink-3)] font-mono uppercase">inferred</span>
            </div>
          </div>
          <span className="text-[10px] font-mono text-[var(--ink-5)]">
            {d.edges.length} edge{d.edges.length === 1 ? "" : "s"} · thickness is call volume, red is failing
          </span>
        </div>
      </Panel>

      <Panel title="Service Detail" right={svc && <StatusDot status={svc.status} />}>
        <ServiceDetail svc={svc} edges={d.edges} />
      </Panel>

      <Panel title="All Services" className="lg:col-span-3">
        <ServiceTable services={d.services} selected={svc?.id} setSelected={setSelected} />
      </Panel>
    </div>
  );
}

// Host detail: a summary strip, then the metric grid. Ordered the way a host is
// actually read — what it is doing (cpu, memory, load), what it is talking to
// (network), what it is storing (disk) — rather than alphabetically, so the
// panels most likely to explain a problem are the ones you reach first.
// Tabs on a host, not in the sidebar: these are the three signals *for this
// host*, and the point of putting them here is to pivot from "this host looks
// bad" to "what was it logging" without losing which host you were on.
// ContainersView is the table for one host's containers.
//
// Columns are the five facts that decide whether a container needs attention,
// which is the same reasoning behind the host table's column set: what is it,
// how hard is it working, how much memory does it hold, is it talking, and how
// many processes are inside. Everything else a container has — ports, mounts,
// env, labels — describes how it was configured rather than how it is running,
// and belongs on a detail page rather than in a list read at a glance.
function ContainersView({ containers, logs, onShowLogs }) {
  const [q, setQ] = useState("");

  const counts = useMemo(() => containerLogCounts(logs), [logs]);
  const rows = useMemo(() => {
    const needle = q.trim().toLowerCase();
    if (!needle) return containers;
    return containers.filter(
      (c) =>
        c.name.toLowerCase().includes(needle) ||
        c.image.toLowerCase().includes(needle) ||
        c.runtime.toLowerCase().includes(needle)
    );
  }, [containers, q]);

  if (!containers.length) {
    return (
      <NotWired
        title="Containers"
        why="No container metrics received. Set containers.enabled: true in the agent config — and note that a host simply running no containers looks the same here."
        needs="containers.enabled"
      />
    );
  }

  const num = (v, fmt) => (Number.isFinite(v) ? fmt(v) : <span className="text-[var(--ink-4)]">—</span>);

  return (
    <div className="flex flex-col gap-3">
      <div className="flex items-center justify-between gap-3">
        <div className="relative flex-1 max-w-xs">
          <Search size={12} className="absolute left-2.5 top-1/2 -translate-y-1/2 text-[var(--ink-4)]" />
          <input
            value={q}
            onChange={(e) => setQ(e.target.value)}
            placeholder="filter by name, image or runtime"
            className="w-full bg-[var(--surface)] border border-[var(--n4)] rounded pl-7 pr-2 py-1.5 text-[11.5px] font-mono outline-none focus:border-[var(--accent)]"
          />
        </div>
        <span className="text-[10px] font-mono text-[var(--ink-4)]">
          {rows.length === containers.length
            ? `${containers.length} container${containers.length === 1 ? "" : "s"}`
            : `${rows.length} of ${containers.length}`}
        </span>
      </div>

      <div className="overflow-x-auto border border-[var(--n4)] rounded-lg bg-[var(--surface)]">
        <table className="w-full text-[11.5px]">
          <thead>
            <tr className="text-left text-[10px] uppercase tracking-wide text-[var(--ink-4)] border-b border-[var(--n2)]">
              <th className="px-3 py-2 font-medium">Container</th>
              <th className="px-3 py-2 font-medium">Image</th>
              <th className="px-3 py-2 font-medium text-right tabular-nums">CPU</th>
              <th className="px-3 py-2 font-medium text-right tabular-nums">Memory</th>
              <th className="px-3 py-2 font-medium text-right tabular-nums">Net rx/tx</th>
              <th className="px-3 py-2 font-medium text-right tabular-nums">Disk r/w</th>
              <th className="px-3 py-2 font-medium text-right tabular-nums">PIDs</th>
              <th className="px-3 py-2 font-medium text-right tabular-nums">Logs</th>
            </tr>
          </thead>
          <tbody className="font-mono">
            {rows.map((c) => {
              // The agent falls back to the short id when it cannot read the
              // Docker socket. Saying so beats rendering a hex string as
              // though it were the name somebody gave the container.
              const unnamed = c.name === c.id;
              const logCount = counts[c.name] || 0;
              return (
                <tr key={c.id} className="border-b border-[var(--n2)] last:border-0 hover:bg-[var(--n1)]">
                  <td className="px-3 py-2">
                    <div className="flex items-center gap-2">
                      <span className={unnamed ? "text-[var(--ink-3)]" : ""}>
                        {unnamed ? c.id.slice(0, 12) : c.name}
                      </span>
                      <span
                        className="text-[9.5px] uppercase tracking-wide text-[var(--ink-4)] border border-[var(--n4)] rounded px-1 py-px"
                        title={
                          c.runtime === "unknown"
                            ? "The runtime could not be determined from the cgroup name, and is reported as unknown rather than assumed."
                            : `Reported by ${c.runtime}`
                        }
                      >
                        {c.runtime}
                      </span>
                    </div>
                    {unnamed && (
                      <div className="text-[9.5px] text-[var(--ink-4)] mt-0.5">
                        no name — the Docker socket is not readable
                      </div>
                    )}
                  </td>
                  <td className="px-3 py-2 text-[var(--ink-3)] max-w-[220px] truncate" title={c.image}>
                    {c.image || <span className="text-[var(--ink-4)]">—</span>}
                  </td>
                  {/* Percent of ONE core, so a multi-threaded container reads
                      above 100 — the same convention `docker stats` uses and
                      the same one the agent's own metric documents. */}
                  <td className="px-3 py-2 text-right tabular-nums">
                    {num(c.cpu, (v) => `${v.toFixed(1)}%`)}
                  </td>
                  <td className="px-3 py-2 text-right tabular-nums">
                    {num(c.mem, (v) => fmtBytes(v))}
                    {Number.isFinite(c.memPct) && (
                      <span className="text-[var(--ink-4)] ml-1">{c.memPct.toFixed(0)}%</span>
                    )}
                  </td>
                  <td className="px-3 py-2 text-right tabular-nums text-[var(--ink-3)]">
                    {fmtBytes(c.rx, true)} / {fmtBytes(c.tx, true)}
                  </td>
                  <td className="px-3 py-2 text-right tabular-nums text-[var(--ink-3)]">
                    {fmtBytes(c.blkRead, true)} / {fmtBytes(c.blkWrite, true)}
                  </td>
                  <td className="px-3 py-2 text-right tabular-nums">{num(c.pids, (v) => v.toFixed(0))}</td>
                  <td className="px-3 py-2 text-right tabular-nums">
                    {logCount > 0 ? (
                      <button
                        onClick={() => onShowLogs?.(c.name)}
                        className="text-[var(--accent)] hover:underline"
                        title={`Show this container's log lines`}
                      >
                        {logCount}
                      </button>
                    ) : (
                      <span className="text-[var(--ink-4)]">—</span>
                    )}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>

      <div className="text-[11px] text-[var(--ink-4)] leading-relaxed px-0.5">
        CPU is percent of one core, so a container using four threads reads about
        400 — the convention <span className="text-[var(--ink-3)]">docker stats</span> uses.
        Network and disk are rates over the collection interval, not totals since the
        container started. A container with no log count either writes nothing or uses a
        log driver the agent cannot read.
      </div>
    </div>
  );
}

function HostTabs({ tab, setTab, tabs }) {
  return (
    <div className="flex items-center gap-1 border-b border-[var(--n4)]">
      {tabs.map(({ id, label, icon: Icon, count }) => {
        const active = tab === id;
        return (
          <button
            key={id} onClick={() => setTab(id)}
            aria-current={active ? "page" : undefined}
            className="flex items-center gap-1.5 px-3.5 py-2 text-[11px] font-mono -mb-px border-b-2 transition-colors"
            style={{
              color: active ? "var(--ink)" : "var(--ink-3)",
              borderColor: active ? "var(--accent)" : "transparent",
            }}
          >
            <Icon size={12} />
            {label}
            {count != null && (
              <span className="text-[10px] tabular-nums" style={{ color: active ? "var(--ink-3)" : "var(--ink-4)" }}>
                {count}
              </span>
            )}
          </button>
        );
      })}
    </div>
  );
}

function InfrastructureView({ snap, d }) {
  const [tab, setTab] = useState("metrics");
  // Which container the log tab is narrowed to, or null for everything. Held
  // here rather than inside LogsView because it is set from the containers
  // tab, and a filter that only its own view can set could not be crossed to.
  const [logFilter, setLogFilter] = useState(null);
  const panels = useMemo(() => hostMetricPanels(snap), [snap]);

  if (!d.infra.length) {
    return <NotWired title="Infrastructure" why="No host metrics received. Set metrics.enabled: true in the agent config." needs="metrics.enabled" />;
  }
  const n = d.infra[0];
  // The same derivation the fleet table uses, so a host cannot be described one
  // way in the list and another way on its own page.
  const hostFacts = useMemo(() => hostRow(snap) || {}, [snap]);
  const retainMin = snap?.retain_sec ? Math.round(snap.retain_sec / 60) : null;

  return (
    <div className="flex flex-col gap-4">
      {/* Summary strip: identity and the two numbers that decide whether the
          grid below is worth reading. */}
      <div className="bg-[var(--surface)] border border-[var(--n4)] rounded-lg px-4 py-3.5">
        <div className="flex items-center gap-2 mb-3">
          <Server size={14} className="text-[var(--ink-3)]" />
          <span className="font-mono text-sm">{n.host}</span>
        </div>
        <div className="grid grid-cols-2 lg:grid-cols-4 gap-x-6 gap-y-3">
          <Fact label="Status">
            <span className="flex items-center"><StatusDot status={n.status} />
              <span style={{ color: statusColor[n.status] }}>{n.status}</span></span>
          </Fact>
          {/* Read from the host rather than asserted. This said "linux" for
              every machine on the grounds that the agent only builds for
              Linux — true of the binary, and useless as a description of the
              server you opened this page to look at. */}
          <Fact label="Operating system">
            <span title={hostFacts.osDescription || ""}>{hostFacts.os || "unknown"}</span>
          </Fact>
          <Fact label="CPU usage"><GaugeBar value={n.cpu} /></Fact>
          <Fact label="Memory usage"><GaugeBar value={n.mem} /></Fact>
        </div>

        {/* Cloud identity, and only when there is some: off EC2 every one of
            these is empty, and a row of four em-dashes describes the page
            rather than the host. */}
        {(hostFacts.instanceID || hostFacts.zone || hostFacts.account) && (
          <div className="grid grid-cols-2 lg:grid-cols-4 gap-x-6 gap-y-3 mt-3 pt-3 border-t border-[var(--n2)]">
            <Fact label="Instance ID">
              <span className="font-mono text-[11.5px]">{hostFacts.instanceID || "—"}</span>
            </Fact>
            <Fact label="Instance type">
              <span className="font-mono text-[11.5px]">{hostFacts.instanceType || "—"}</span>
            </Fact>
            <Fact label="Zone">
              <span className="font-mono text-[11.5px]">{hostFacts.zone || "—"}</span>
            </Fact>
            <Fact label="Account">
              <span className="font-mono text-[11.5px]">{hostFacts.account || "—"}</span>
            </Fact>
            {hostFacts.imageID && (
              <Fact label="AMI">
                <span className="font-mono text-[11.5px]">{hostFacts.imageID}</span>
              </Fact>
            )}
            {hostFacts.arch && (
              <Fact label="Architecture">
                <span className="font-mono text-[11.5px]">{hostFacts.arch}</span>
              </Fact>
            )}
          </div>
        )}
      </div>

      <div className="flex items-end justify-between gap-4">
        <HostTabs
          tab={tab} setTab={setTab}
          tabs={[
            { id: "metrics", label: "Metrics", icon: Gauge },
            // Present only when there are containers. A permanently empty tab
            // on every bare-metal host is a worse answer than no tab: it
            // invites a click that explains nothing.
            ...(d.containers.length
              ? [{ id: "containers", label: "Containers", icon: Box, count: d.containers.length }]
              : []),
            { id: "logs", label: "Logs", icon: ScrollText, count: d.logs.length },
            { id: "traces", label: "Traces", icon: Waypoints, count: d.traces.length },
          ]}
        />
        <div className="flex items-center gap-3 text-[10px] font-mono text-[var(--ink-4)] pb-2 flex-shrink-0">
          <span>{n.role}</span>
          {retainMin && <span>last {retainMin} min · live</span>}
        </div>
      </div>

      {tab === "metrics" && (
        <>
          <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
            {panels.map((p) => <MetricPanel key={p.id} panel={p} />)}
          </div>
          <div className="text-[11px] text-[var(--ink-4)] leading-relaxed px-0.5">
            One host, because this dashboard talks to one agent — the agent's view is
            deliberately per-host. A fleet view needs a backend that aggregates many
            agents. Panels with more than {MAX_SERIES_PER_PANEL} series fold the smallest
            into <span className="text-[var(--ink-3)]">other</span>, ranked by peak, rather
            than inventing colours nobody checked for contrast.
          </div>
        </>
      )}

      {/* The same components the sidebar renders, not copies. With one agent
          per dashboard, "this host's logs" and "all logs" are the same set, so
          these tabs are a pivot rather than a filter — they become a real
          narrowing only once a backend puts several hosts behind one view. */}
      {tab === "containers" && (
        <ContainersView
          containers={d.containers}
          logs={d.logs}
          // Clicking a container's log count crosses to the log tab already
          // narrowed to it. Passing the name rather than filtering in place is
          // what keeps one log view in the app: the alternative is a second,
          // slightly different log list that drifts from the first.
          onShowLogs={(name) => {
            setLogFilter(name);
            setTab("logs");
          }}
        />
      )}
      {tab === "logs" && (
        <LogsView
          logs={logFilter ? d.logs.filter((l) => l.labels?.["container.name"] === logFilter) : d.logs}
          scopeLabel={logFilter}
          onClearScope={() => setLogFilter(null)}
        />
      )}
      {tab === "traces" && <TracesView traces={d.traces} />}
    </div>
  );
}

function Fact({ label, children }) {
  return (
    <div className="flex flex-col gap-1">
      <span className="text-[10px] tracking-widest uppercase font-mono text-[var(--ink-4)]">{label}</span>
      <span className="font-mono text-[12px]">{children}</span>
    </div>
  );
}

// ---------------------------------------------------------------------------

// A function rather than a constant because the host list is now editable
// while the UI is running: adding a second server has to make the Fleet tab
// appear without a reload.
// showFleet is separate from hostCount because the two no longer mean the same
// thing. hostCount is how many agents this browser has an address for; the
// backend knows about hosts it does not. A fleet of ten reporting through the
// backend with none configured locally is the normal case now, and gating the
// tab on hostCount hid the tab from exactly that setup.
const navGroups = (hostCount, fleetCount = 0) => [
  { label: "Monitor", items: [
    // Still hidden for a single host with no backend — a "Fleet" of one is a
    // worse Overview.
    ...(hostCount > 1 || fleetCount > 1 ? [{ id: "fleet", label: "Fleet", icon: Server }] : []),
    { id: "overview", label: "Overview", icon: LayoutDashboard },
    { id: "topology", label: "Service Topology", icon: Network },
  ]},
  { label: "Explore", items: [
    { id: "traces", label: "Traces", icon: Waypoints },
    { id: "logs", label: "Logs", icon: ScrollText },
    { id: "metrics", label: "Metrics", icon: Gauge },
    { id: "exceptions", label: "Exceptions", icon: XCircle },
  ]},
  { label: "Manage", items: [
    { id: "infra", label: "Infrastructure", icon: Server },
    { id: "problems", label: "Problems", icon: AlertTriangle },
    { id: "monitors", label: "Monitors", icon: Bell },
  ]},
];

function Sidebar({ view, setView, hostCount, fleetCount }) {
  const groups = navGroups(hostCount, fleetCount);
  return (
    <div className="w-[200px] flex-shrink-0 border-r border-[var(--n2)] flex flex-col py-4 overflow-y-auto">
      {groups.map((group) => (
        <nav key={group.label} className="flex flex-col gap-0.5 px-2 mb-3">
          <div className="px-3 pb-1 text-[9.5px] font-mono uppercase tracking-widest text-[var(--ink-5)]">{group.label}</div>
          {group.items.map((item) => {
            const Icon = item.icon;
            const active = view === item.id;
            return (
              <button key={item.id} onClick={() => setView(item.id)}
                className="flex items-center gap-2.5 px-3 py-2 rounded text-[12.5px] font-mono transition-colors"
                style={{
                  color: active ? "var(--accent)" : "var(--ink-3)",
                  background: active ? "color-mix(in srgb, var(--accent) 8%, transparent)" : "transparent",
                  borderLeft: active ? "2px solid var(--accent)" : "2px solid transparent",
                }}>
                <Icon size={14} />
                <span className="flex-1 text-left">{item.label}</span>
              </button>
            );
          })}
        </nav>
      ))}
    </div>
  );
}

// UsageBar is the list's colour-coded progress bar.
//
// Colour is a redundant encoding, never the only one: the number is always
// printed beside it, so the column still reads correctly for anyone who cannot
// distinguish the fill colours.
function UsageBar({ value }) {
  const v = Number.isFinite(value) ? Math.max(0, Math.min(100, value)) : 0;
  const tone = v >= 90 ? "var(--crit)" : v >= 75 ? "var(--warn)" : "var(--good)";
  return (
    <div className="flex items-center gap-2 min-w-0">
      <div className="h-1.5 rounded-full bg-[var(--n3)] flex-1 min-w-[3rem] overflow-hidden">
        <div className="h-full rounded-full" style={{ width: `${v}%`, background: tone }} />
      </div>
      <span className="text-[11px] font-mono tabular-nums w-[3.2rem] text-right shrink-0">
        {Number.isFinite(value) ? `${v.toFixed(0)}%` : "—"}
      </span>
    </div>
  );
}

// The host list's columns. `get` pulls the sort key so sorting never has to
// know how a cell is rendered.
const HOST_COLUMNS = [
  { id: "host", label: "Hostname", get: (r) => r.host, align: "left" },
  // Its own column rather than a sub-line under Instance. The id is the only
  // identifier on the row that is guaranteed unique and cannot be renamed, so
  // it is what you match against an alert or a console tab — that is a lookup,
  // and a lookup wants a column it can be sorted and scanned down.
  { id: "instanceId", label: "Host ID", get: (r) => r.instanceID || "", align: "left" },
  // statusRank, not r.active: the column has three states and sorting on two
  // of them interleaved the hosts that had sent nothing with the healthy ones.
  { id: "status", label: "Status", get: (r) => statusRank(r), align: "left" },
  // How long since the host last reported.
  //
  // Without this, INACTIVE is a verdict with no evidence: a host that stopped
  // ten minutes ago and one that stopped last week look identical, and an
  // empty metrics row reads as a broken dashboard rather than as a quiet
  // machine. It is the first thing you want when a row is not green, and the
  // difference between "the agent just restarted" and "nobody has touched
  // this box in a month".
  //
  // Sorted by age rather than by the rendered string, so "2m" and "3h" order
  // correctly instead of alphabetically.
  { id: "seen", label: "Last Seen", get: (r) => (Number.isFinite(r.ageSec) ? r.ageSec : Infinity), align: "right" },
  // Sortable rather than tucked under the hostname, because at fleet sizes the
  // useful questions are "which instance type is this" and "is one AZ having a
  // bad day" — both of which mean grouping rows, not reading one.
  { id: "instance", label: "Instance", get: (r) => r.instanceType || "", align: "left" },
  { id: "zone", label: "Zone", get: (r) => r.zone || "", align: "left" },
  // The account is the coarsest grouping there is and the one that decides who
  // can even reach a box. It was being derived and then discarded — the column
  // never existed — which left a fleet spanning several accounts looking like
  // one flat list. Sortable, because the question is "show me everything in
  // this account", which means grouping rows rather than reading one.
  { id: "account", label: "Account", get: (r) => r.account || "", align: "left" },
  // Distro and version, read from the host. Sortable for the same reason: the
  // question is "which boxes are still on the old image", not "what is this
  // one running".
  { id: "os", label: "OS", get: (r) => r.os || "", align: "left" },
  { id: "cpu", label: "CPU Usage", get: (r) => r.cpu, align: "left", bar: true },
  { id: "mem", label: "Memory Usage", get: (r) => r.mem, align: "left", bar: true },
  { id: "iowait", label: "IOWait", get: (r) => r.iowait, align: "right" },
  { id: "disk", label: "Disk Usage", get: (r) => r.disk, align: "left", bar: true },
  { id: "load15", label: "Load Avg", get: (r) => r.load15, align: "right" },
];

// The two mappings onto the fleet row shape.
//
// fleetFromAgents is the original path: poll every configured agent and derive
// a row from its snapshot. It can only ever show hosts this browser can reach.
//
// fleetFromBackend reads the backend's inventory, which lists every host that
// has reported regardless of whether there is a route to it from here. Where a
// backend host matches a configured agent, the two are paired so the row stays
// clickable — matching on instance id first, because that is the identifier
// that cannot be renamed, and on the configured name second.
function fleetFromAgents(results) {
  return results.map(({ host, snapshot: snap, error }) => {
    const row = snap ? hostRow(snap) : null;
    return {
      key: host.url,
      host,
      error,
      // A host that never answered still gets a row: its configured name is
      // all we know about it, and omitting it would hide the outage.
      row: row || {
        host: host.name || host.url.replace(/^https?:\/\//, ""),
        active: false, os: "", osDescription: "", arch: "", version: "",
        // Strings, not undefined: these columns sort with localeCompare and an
        // absent value would take the numeric path instead.
        instanceID: "", instanceType: "", zone: "", account: "",
        cpu: NaN, mem: NaN, iowait: NaN, disk: NaN, load15: NaN,
        // Never heard from, as distinct from heard from a long time ago.
        ageSec: Infinity,
      },
    };
  });
}

function fleetFromBackend(rows, hosts, agentIDs) {
  return rows.map((row) => {
    const match =
      hosts.find((h) => agentIDs[h.url] && agentIDs[h.url] === row.instanceID) ||
      hosts.find((h) => h.name && h.name === row.host) ||
      null;
    // Keyed on the host id, which the backend guarantees is unique per row and
    // which survives a rename — unlike the display name, where two machines
    // called "web" would collapse into one row.
    return { key: row.instanceID || row.host, host: match, error: null, row };
  });
}

// FleetView renders an already-derived set of rows.
//
// It takes rows rather than snapshots because there are now two sources for
// them — the backend's host inventory, and a direct poll of each agent — and
// the sorting, filtering and formatting here is identical for both. Deriving
// inside this component would have meant it could only ever render one of
// them. See fleetFromBackend and fleetFromAgents below for the two mappings.
//
// Each entry is { key, host, row, error }. `host` is the configured agent this
// row can be opened against, and is null for a host the backend knows about
// but which this browser has no route to — which is most of them, and the
// reason the backend exists.
function FleetView({ entries, loading, source, sourceError, onOpen }) {
  const [query, setQuery] = useState("");
  const [sort, setSort] = useState({ col: "host", dir: "asc" });

  const rows = entries;

  const visible = useMemo(() => {
    const q = query.trim().toLowerCase();
    // Matches identity, not just the name. Searching an instance id is how you
    // get from an alert or a console tab to the right row, and searching a zone
    // or an instance type is how you check whether a problem is confined to
    // one of them.
    const filtered = q
      ? rows.filter(({ row }) =>
          [row.host, row.instanceID, row.instanceType, row.zone, row.account, row.os]
            .some((field) => (field || "").toLowerCase().includes(q))
        )
      : rows;
    const col = HOST_COLUMNS.find((c) => c.id === sort.col) || HOST_COLUMNS[0];
    const sign = sort.dir === "asc" ? 1 : -1;
    return [...filtered].sort((a, b) => {
      const x = col.get(a.row);
      const y = col.get(b.row);
      if (typeof x === "string") return sign * x.localeCompare(y);
      // Unreachable hosts have NaN metrics; they sort last either way rather
      // than landing at the top of an ascending numeric sort.
      const nx = Number.isFinite(x) ? x : -Infinity;
      const ny = Number.isFinite(y) ? y : -Infinity;
      return sign * (nx - ny);
    });
  }, [rows, query, sort]);

  if (loading) {
    return (
      <p className="text-[12px] font-mono text-[var(--ink-3)]">
        {source === "backend" ? "loading the fleet from the backend…" : `polling ${rows.length} hosts…`}
      </p>
    );
  }

  const toggle = (id) =>
    setSort((s) => ({ col: id, dir: s.col === id && s.dir === "asc" ? "desc" : "asc" }));
  // Counted the same way the cell labels it. Counting r.active alone put
  // hosts the table was calling NO DATA into the "N active" total in the line
  // directly above them.
  const active = rows.filter((r) => hostStatus(r.row) === "active").length;

  return (
    <div className="flex flex-col gap-3">
      <div className="flex items-center justify-between gap-3 flex-wrap">
        <div className="relative">
          <Search size={12} className="absolute left-2.5 top-1/2 -translate-y-1/2 text-[var(--ink-5)]" />
          <input
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="filter by name, instance, type or zone"
            aria-label="Filter hosts by name, instance id, instance type or zone"
            className="text-[11.5px] font-mono bg-[var(--surface)] border border-[var(--n4)] rounded pl-7 pr-2.5 py-1.5 text-[var(--ink)] w-[16rem]"
          />
        </div>
        <span className="text-[11px] font-mono text-[var(--ink-4)]">
          Showing {visible.length} of {rows.length} hosts · {active} active ·{" "}
          {/* Which source produced this table is not a detail. It decides what
              an empty row means: from the backend, a host that stopped
              reporting; from the agents, possibly just a tunnel this laptop
              cannot open. */}
          <span style={{ color: source === "backend" ? "var(--accent)" : "var(--ink-5)" }}>
            {source === "backend" ? "via backend" : "polling agents"}
          </span>
        </span>
      </div>

      {sourceError && (
        <div className="border border-[var(--warn)] rounded px-3 py-2">
          <p className="text-[11.5px] font-mono text-[var(--warn)]">{sourceError.message}</p>
          {sourceError.detail && (
            <p className="text-[10.5px] font-mono text-[var(--ink-4)] mt-0.5">{sourceError.detail}</p>
          )}
        </div>
      )}

      <div className="border border-[var(--n3)] rounded overflow-x-auto">
        <table className="w-full min-w-[80rem] border-collapse">
          <thead>
            <tr className="bg-[var(--surface)]">
              {HOST_COLUMNS.map((c) => (
                <th
                  key={c.id}
                  onClick={() => toggle(c.id)}
                  className="text-left px-3 py-2 border-b border-[var(--n3)] cursor-pointer select-none"
                  aria-sort={sort.col === c.id ? (sort.dir === "asc" ? "ascending" : "descending") : "none"}
                >
                  <span className="inline-flex items-center gap-1 text-[9.5px] font-mono uppercase tracking-widest text-[var(--ink-5)]">
                    {c.label}
                    {sort.col === c.id &&
                      (sort.dir === "asc" ? <ChevronUp size={10} /> : <ChevronDown size={10} />)}
                  </span>
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {visible.map(({ key, host, row, error }) => (
              <tr
                key={key}
                // Every row opens now. Where there is a configured agent the
                // detail comes from it live; where there is not, it comes from
                // the backend's stored copy — same payload shape either way,
                // so the views downstream do not know which they are reading.
                onClick={() => onOpen(host, row)}
                title={host ? "" : "Read from the backend — this browser has no route to the machine."}
                className="border-b border-[var(--n2)] last:border-b-0 cursor-pointer hover:bg-[var(--surface)]"
              >
                <td className="px-3 py-2.5">
                  <span className="font-mono text-[12.5px] text-[var(--accent)]">{row.host}</span>
                  {row.version && (
                    <span className="block text-[10px] font-mono text-[var(--ink-5)]">{row.version}</span>
                  )}
                </td>
                <td className="px-3 py-2.5 font-mono text-[11.5px] text-[var(--ink-3)] whitespace-nowrap">
                  {row.instanceID || "—"}
                </td>
                {/* Three states, not two. A host that is listed but has never
                    sent a data point is not the same as one that reported and
                    went quiet: the first usually means the agent is exporting
                    to somewhere else or its export carries identity and no
                    metrics, and the second means the machine or its agent is
                    down. Collapsing both into INACTIVE sends you looking at
                    the wrong end of the pipeline. Only rows sourced from the
                    backend carry hasMetrics; an agent-sourced row leaves it
                    undefined and keeps the original two states. */}
                <td className="px-3 py-2.5">
                  {(() => {
                    // One derivation, used for the dot, the word and the
                    // tooltip. Deriving the colour and the label separately is
                    // what produced a green dot labelled NO DATA.
                    const status = hostStatus(row);
                    const tone =
                      status === "no-metrics" ? "var(--warn)" : status === "active" ? "var(--good)" : "var(--crit)";
                    const label =
                      status === "no-metrics" ? "NO METRICS" : status === "active" ? "ACTIVE" : "INACTIVE";
                    return (
                      <span
                        className="inline-flex items-center gap-1.5 text-[10.5px] font-mono"
                        title={
                          status === "no-metrics"
                            ? "No host metrics from this host in the selected window. It may still be " +
                              "sending logs or traces — the fleet query does not report those, so open it " +
                              "to see. If it is empty too, the agent's exporter is probably pointing " +
                              "elsewhere, or its ingest key is being refused."
                            : undefined
                        }
                      >
                        <span className="w-1.5 h-1.5 rounded-full" style={{ background: tone }} />
                        <span style={{ color: status === "active" ? "var(--ink-3)" : tone }}>
                          {label}
                        </span>
                      </span>
                    );
                  })()}
                  {error && (
                    <span className="block text-[10px] font-mono text-[var(--ink-5)] mt-0.5">
                      {error.message}
                    </span>
                  )}
                </td>
                <td className="px-3 py-2.5 text-right font-mono text-[11.5px] tabular-nums whitespace-nowrap"
                    style={{ color: row.active ? "var(--ink-3)" : "var(--warn)" }}>
                  {fmtAge(row.ageSec)}
                </td>
                <td className="px-3 py-2.5 font-mono text-[11.5px] text-[var(--ink-3)]">
                  {row.instanceType || "—"}
                </td>
                <td className="px-3 py-2.5 font-mono text-[11.5px] text-[var(--ink-3)]">
                  {row.zone || "—"}
                </td>
                <td className="px-3 py-2.5 font-mono text-[11.5px] text-[var(--ink-3)] whitespace-nowrap">
                  {row.account || "—"}
                </td>
                <td className="px-3 py-2.5 font-mono text-[11.5px] text-[var(--ink-3)] whitespace-nowrap">
                  {/* The full description, including the kernel, is on hover:
                      it is what you want once you have found the odd row out,
                      and too long to give a column to. */}
                  <span title={row.osDescription || ""}>{row.os || "—"}</span>
                  {row.arch && (
                    <span className="block text-[10px] font-mono text-[var(--ink-5)]">{row.arch}</span>
                  )}
                </td>
                <td className="px-3 py-2.5 w-[14%]"><UsageBar value={row.cpu} /></td>
                <td className="px-3 py-2.5 w-[14%]"><UsageBar value={row.mem} /></td>
                <td className="px-3 py-2.5 text-right font-mono text-[11.5px] tabular-nums">
                  {Number.isFinite(row.iowait) ? `${row.iowait.toFixed(2)}%` : "—"}
                </td>
                <td className="px-3 py-2.5 w-[14%]"><UsageBar value={row.disk} /></td>
                <td className="px-3 py-2.5 text-right font-mono text-[11.5px] tabular-nums">
                  {Number.isFinite(row.load15) ? row.load15.toFixed(2) : "—"}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      <p className="text-[10.5px] font-mono text-[var(--ink-5)]">
        {source === "backend" ? (
          <>
            Read from the backend, so a host appears because it reported — from any
            account or region, with no route from this browser to the machine itself.
            Open any row for its metrics, logs and traces; where no direct agent is
            configured those come from the backend's stored copy rather than live
            from the machine. IOWait is not computed here.
          </>
        ) : (
          <>
            Polled from each configured agent directly, which is why every host needs a
            reachable address. Start the backend and these come from one place instead.
          </>
        )}{" "}
        Status is ACTIVE when a metric arrived in the last 10 minutes. Instance, Zone and
        Account come from the host's own cloud metadata and are empty off EC2; OS is read
        from the host itself, and is empty on agents older than the release that started
        reporting it.
      </p>
    </div>
  );
}

// HostPicker switches which agent the dashboard is reading, and is the way in
// to editing the list.
//
// A native <select> rather than a custom dropdown: it is keyboard accessible
// and screen-reader correct for free, and closes on outside click without any
// of the listener bookkeeping a div-based menu needs.
//
// The dot is the host's own reachability, not the selected host's. Its whole
// purpose is to tell you a host is unreachable BEFORE you switch to it and
// wonder why the dashboard went blank.
// Values are prefixed because the two kinds of host are addressed differently
// — an agent by URL, a backend host by id — and a bare value could not say
// which. A host that is both appears once, under the agent, because a live
// agent gives full resolution and the backend gives a stored copy of it.
const AGENT_OPT = "a:";
const BACKEND_OPT = "b:";

function HostPicker({
  hosts, selectedURL, setSelectedURL, health, agentID, onManage,
  backendRows = [], backendHostID = "", setBackendHostID = () => {},
}) {
  const dot = (state) =>
    state === "up" ? "var(--good)" : state === "down" ? "var(--crit)" : "var(--ink-3)";

  // Hosts the backend knows about that are not already in the configured list,
  // matched on the id the agent reports rather than on its display name, which
  // is editable and duplicable.
  const configuredIDs = new Set(Object.values({ [selectedURL]: agentID }).filter(Boolean));
  const backendOnly = backendRows.filter((r) => r.instanceID && !configuredIDs.has(r.instanceID));

  const value = backendHostID ? BACKEND_OPT + backendHostID : AGENT_OPT + selectedURL;
  const onChange = (e) => {
    const v = e.target.value;
    if (v.startsWith(BACKEND_OPT)) {
      setBackendHostID(v.slice(BACKEND_OPT.length));
    } else {
      setBackendHostID("");
      setSelectedURL(v.slice(AGENT_OPT.length));
    }
  };

  return (
    <div className="flex items-center gap-2 pl-3 ml-1 border-l border-[var(--n2)]">
      <span
        className="w-2 h-2 rounded-full shrink-0"
        // A backend host has no reachability to report — that is the point of
        // it — so it takes the neutral dot rather than a red one that would
        // read as an outage.
        style={{ background: backendHostID ? "var(--accent)" : dot(health[selectedURL]) }}
        aria-hidden="true"
      />
      <select
        aria-label="Select host"
        value={value}
        onChange={onChange}
        className="text-[11px] font-mono bg-[var(--surface)] border border-[var(--n4)] rounded px-2 py-1 text-[var(--ink)] cursor-pointer max-w-[16rem]"
      >
        {/* A value matching no option makes a browser display the first one,
            so an empty host list showed the name of a machine nothing had
            selected — the picker asserting a selection that did not exist.
            An explicit placeholder keeps the displayed value honest. */}
        {!hosts.length && !backendHostID && <option value="">no host selected</option>}
        {hosts.length > 0 && (
        <optgroup label="Direct agents">
          {hosts.map((host) => (
            <option key={host.url} value={AGENT_OPT + host.url}>
              {health[host.url] === "down" ? "○ " : "● "}
              {hostLabel(host, !backendHostID && host.url === selectedURL ? agentID : "")}
            </option>
          ))}
        </optgroup>
        )}
        {backendOnly.length > 0 && (
          <optgroup label="Via backend">
            {backendOnly.map((r) => (
              <option key={r.instanceID} value={BACKEND_OPT + r.instanceID}>
                {r.active ? "● " : "○ "}
                {r.host}
              </option>
            ))}
          </optgroup>
        )}
      </select>
      <button
        onClick={onManage}
        title="Add or remove hosts"
        aria-label="Manage hosts"
        className="flex items-center gap-1 text-[11px] font-mono px-2 py-1 rounded bg-[var(--surface)] border border-[var(--n4)] text-[var(--ink-3)] hover:text-[var(--ink)]"
      >
        <Settings size={12} />
        {hosts.length}
      </button>
    </div>
  );
}

// HostManager edits the list of agents.
//
// This exists because the list used to be an environment variable Vite froze
// into the bundle, so adding a server meant stopping the dev server, editing a
// shell command and starting it again — for a list that changes every time a
// tunnel comes up. Servers in different accounts get added one at a time, and
// the place to do that is the UI already showing you the others.
//
// The bulk box is the primary input, not a convenience. Ten forwarded ports
// arrive as a block of text out of a terminal or a runbook; typing them into
// ten separate fields is the same information entered ten times more slowly.
function HostManager({ hosts, setHosts, health, onClose }) {
  const [text, setText] = useState(() => toHostSpec(hosts));
  const [error, setError] = useState("");

  // An emptied box removes every host; only text that was meant to be an
  // address and is not gets refused. This textarea is the only control that
  // removes a host, so refusing an empty result meant the last one could not
  // be removed at all — and the message said "No usable addresses", which
  // describes a typo rather than the deliberate clear it usually was.
  // readHostSpec makes that call, where it can be tested.
  const apply = () => {
    const { hosts: parsed, error: reason } = readHostSpec(text);
    if (!parsed) {
      setError(reason);
      return;
    }
    setHosts(parsed);
    onClose();
  };

  const restore = () => {
    const seeded = configuredHosts();
    setText(toHostSpec(seeded.length > 0 ? seeded : hosts));
    setError(seeded.length > 0 ? "" : "AGENT_I_HOSTS was not set when the dev server started.");
  };

  return (
    <div
      className="fixed inset-0 z-50 flex items-start justify-center pt-[8vh] px-4 bg-[color-mix(in_srgb,var(--bg)_75%,transparent)]"
      onClick={onClose}
    >
      <div
        className="w-full max-w-[560px] rounded border border-[var(--n4)] bg-[var(--surface)] shadow-xl flex flex-col"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center justify-between px-4 py-3 border-b border-[var(--n2)]">
          <h2 className="font-mono text-[13px]">Hosts</h2>
          <button onClick={onClose} aria-label="Close" className="text-[var(--ink-3)] hover:text-[var(--ink)]">
            <XCircle size={16} />
          </button>
        </div>

        <div className="px-4 py-3 flex flex-col gap-3">
          <p className="text-[11px] text-[var(--ink-4)] leading-relaxed">
            One per line, as <span className="font-mono text-[var(--ink-3)]">name=url</span> or a bare
            address. Each needs to be reachable from this machine — for agents on other servers that
            means an SSH forward per host, since the dashboard port binds loopback and has no
            authentication. See <span className="font-mono text-[var(--ink-3)]">scripts/dev-tunnels.sh</span>.
          </p>

          <textarea
            value={text}
            onChange={(e) => { setText(e.target.value); setError(""); }}
            spellCheck={false}
            rows={8}
            aria-label="Host list"
            placeholder={"prod-web-1=http://127.0.0.1:8089\nprod-web-2=http://127.0.0.1:8090\n127.0.0.1:8091"}
            className="w-full font-mono text-[12px] leading-relaxed bg-[var(--bg)] border border-[var(--n4)] rounded px-2.5 py-2 text-[var(--ink)] resize-y"
          />

          {/* Live preview of what will be saved. Normalisation is forgiving —
              a missing scheme is added, a pasted /api/snapshot is stripped —
              and showing the result is how that stays trustworthy rather than
              surprising. */}
          <div className="flex flex-col gap-1">
            {parseHostSpec(text).map((h) => (
              <div key={h.url} className="flex items-center gap-2 text-[11px] font-mono">
                <span
                  className="w-1.5 h-1.5 rounded-full shrink-0"
                  style={{
                    background:
                      health[h.url] === "up" ? "var(--good)"
                      : health[h.url] === "down" ? "var(--crit)"
                      : "var(--ink-5)",
                  }}
                />
                <span className="text-[var(--ink-3)] min-w-[8rem]">{h.name || "(unnamed)"}</span>
                <span className="text-[var(--ink-5)]">{h.url}</span>
              </div>
            ))}
          </div>

          {error && <p className="text-[11px] font-mono text-[var(--crit)]">{error}</p>}
        </div>

        <div className="flex items-center justify-between px-4 py-3 border-t border-[var(--n2)]">
          <button
            onClick={restore}
            className="text-[11px] font-mono text-[var(--ink-4)] hover:text-[var(--ink-3)]"
          >
            Restore from AGENT_I_HOSTS
          </button>
          <div className="flex items-center gap-2">
            <button
              onClick={onClose}
              className="text-[11px] font-mono px-3 py-1.5 rounded border border-[var(--n4)] text-[var(--ink-3)]"
            >
              Cancel
            </button>
            <button
              onClick={apply}
              className="text-[11px] font-mono px-3 py-1.5 rounded bg-[color-mix(in_srgb,var(--accent)_15%,transparent)] border border-[color-mix(in_srgb,var(--accent)_40%,transparent)] text-[var(--accent)]"
            >
              Save
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}

export default function ObservabilityDashboard() {
  const [view, setView] = useState("overview");
  const [selected, setSelected] = useState(null);
  const [now, setNow] = useState(new Date());

  // The host list is runtime state, seeded from AGENT_I_HOSTS on first run and
  // persisted in the browser after that. See hosts.js.
  const [hosts, setHostsState] = useState(loadHosts);
  const [managing, setManaging] = useState(false);
  // Removing the last host is allowed.
  //
  // This used to substitute the current list for an empty one, which made
  // deleting the only entry a no-op: the row stayed, the removal never reached
  // storage, and there was no message saying why. Together with the seed
  // fallback in loadHosts, a host pointing at a dead tunnel could not be got
  // rid of at all.
  //
  // An empty list is a supported state — the dashboard renders it, and the
  // backend still supplies the fleet with no agent reachable — so there is
  // nothing here to protect the user from.
  const setHosts = (next) => {
    setHostsState(next);
    saveHosts(next);
  };

  // Selection is by URL, not by index. The list is editable while the UI is
  // running, and an index would silently point at a different machine the
  // moment a host above it was removed.
  const [selectedURL, setSelectedURL] = useState(() => loadHosts()[0]?.url || "");
  const selectedHost =
    hosts.find((h) => h.url === selectedURL) || hosts[0] || null;

  // A removed host must not leave the dashboard polling an address that is no
  // longer in the list.
  useEffect(() => {
    if (hosts.length > 0 && !hosts.some((h) => h.url === selectedURL)) {
      setSelectedURL(hosts[0].url);
    }
  }, [hosts, selectedURL]);

  // A host selected from the fleet that this browser has no route to. When
  // set, every view reads the backend instead of an agent — the payload shape
  // is identical, so nothing downstream of here knows the difference.
  const [backendHostID, setBackendHostID] = useState("");

  const agentPoll = useSnapshot(5000, backendHostID ? null : selectedHost);
  const backendPoll = useBackendSnapshot(backendHostID, 10000);

  // One of the two is live at a time. Reading from a database is slower to
  // change than reading from an agent's memory, hence the slower interval
  // above, but neither is a stream and the views cannot tell them apart.
  const readingBackend = !!backendHostID;
  const snapshot = readingBackend ? backendPoll.snapshot : agentPoll.snapshot;
  const error = readingBackend ? backendPoll.error : agentPoll.error;
  const loading = readingBackend ? backendPoll.loading : agentPoll.loading;
  const paused = readingBackend ? false : agentPoll.paused;
  const setPaused = agentPoll.setPaused;

  const hostHealth = useHostHealth(hosts);

  // The backend poll runs whether or not the fleet view is open, because its
  // result decides whether the Fleet tab exists at all — a tab that only
  // appears once you are already on the view it leads to is no tab. It is one
  // small request either way, unlike the agent poll below, which transfers
  // every host's whole retention window and so is gated on the view.
  const {
    rows: backendRows,
    error: backendError,
    loading: backendLoading,
  } = useBackendFleet(view === "fleet" ? 10000 : 60000, true);

  // Only polls while the fleet view is on screen, and only when the backend is
  // not already answering — see useAllSnapshots for why that gate matters.
  const usingBackend = backendRows.length > 0;
  const { results: fleet, loading: fleetLoading } = useAllSnapshots(
    hosts,
    10000,
    view === "fleet" && !usingBackend
  );

  // agentIDs maps a configured agent's URL to the host id it reports, so a
  // backend row can be paired with an agent this browser can actually reach.
  // Only the selected host's id is known — that is the only snapshot being
  // polled — which is enough: pairing exists to keep a row clickable, and the
  // fallback to matching on name covers the rest.
  const agentIDs = useMemo(
    () => (!readingBackend && selectedHost && snapshot?.host?.["host.id"]
      ? { [selectedHost.url]: snapshot.host["host.id"] }
      : {}),
    [readingBackend, selectedHost, snapshot]
  );

  // What to offer when the selected agent cannot be reached.
  //
  // Matched on the configured name against the host the backend knows, since
  // an unreachable agent has told us nothing about itself — there is no
  // instance id to match on, precisely because the poll failed. An unmatched
  // offer is still worth making: any backend host is more useful than an error
  // with nothing behind it, but it is labelled differently so the offer never
  // claims to be the same machine when it has not established that.
  const backendFallback = useMemo(
    () => chooseBackendFallback({ readingBackend, error, backendRows, selectedHost }),
    [readingBackend, error, backendRows, selectedHost]
  );

  const fleetEntries = useMemo(
    () => (usingBackend ? fleetFromBackend(backendRows, hosts, agentIDs) : fleetFromAgents(fleet)),
    [usingBackend, backendRows, hosts, agentIDs, fleet]
  );

  // With no configured agent there is nothing else to show, so a backend host
  // is selected rather than leaving the dashboard empty next to a fleet table
  // full of machines. Not a silent source swap — the header says "via
  // backend" — and it only fires when the alternative is a blank page: an
  // agent that merely fails is left alone, because switching away from it
  // would hide the failure the operator needs to see.
  useEffect(() => {
    if (hosts.length === 0 && !backendHostID && backendRows.length > 0) {
      setBackendHostID(backendRows[0].instanceID);
    }
  }, [hosts.length, backendHostID, backendRows]);

  // The Fleet tab disappears when there is nothing to compare, so a view that
  // is no longer reachable in the nav must not stay on screen.
  useEffect(() => {
    if (view === "fleet" && hosts.length <= 1 && backendRows.length <= 1) setView("overview");
  }, [view, hosts.length, backendRows.length]);
  // Charts need no re-render on theme change: their colours are var()
  // references that CSS re-resolves at paint time.
  const { theme, setTheme } = useTheme();

  useEffect(() => {
    const t = setInterval(() => setNow(new Date()), 1000);
    return () => clearInterval(t);
  }, []);

  // One derivation pass per snapshot, shared by every view — recomputing this
  // per view would run the same span walk up to nine times per poll.
  const d = useMemo(() => {
    const g = globalStats(snapshot);
    return {
      ...g,
      traces: deriveTraces(snapshot),
      edges: deriveEdges(snapshot),
      logs: deriveLogs(snapshot),
      infra: deriveInfra(snapshot),
      containers: deriveContainers(snapshot),
      traffic: deriveTraffic(snapshot),
      allSeries: deriveAllSeries(snapshot),
    };
  }, [snapshot]);

  const activeLabel = navGroups(hosts.length, backendRows.length)
    .flatMap((g) => g.items)
    .find((n) => n.id === view)?.label;
  const connected = !!snapshot && !error;

  return (
    <div className="min-h-screen w-full bg-[var(--bg)] text-[var(--ink)] font-sans flex flex-col">
      <div className="flex items-center justify-between px-5 py-3.5 border-b border-[var(--n2)]">
        <div className="flex items-center gap-3">
          <div className="w-7 h-7 rounded bg-[color-mix(in_srgb,var(--accent)_10%,transparent)] border border-[color-mix(in_srgb,var(--accent)_30%,transparent)] flex items-center justify-center">
            <Activity size={14} className="text-[var(--accent)]" />
          </div>
          <div>
            <h1 className="font-mono text-sm tracking-wide">AGENT-I</h1>
            <p className="text-[10px] text-[var(--ink-3)] font-mono">
              {snapshot?.agent_id || (hosts.length > 1 ? `${hosts.length} hosts` : "—")}
              {readingBackend && (
                // Named, because "live" in the status light means something
                // different here: these numbers are as fresh as the last
                // export the host sent, not as fresh as a poll of it.
                <span className="text-[var(--accent)]"> · via backend</span>
              )}{" "}
              · {now.toLocaleTimeString()}
            </p>
          </div>
          {/* Always rendered, unlike before. With one host it was chrome
              implying a choice you did not have — but it is also the only way
              to reach the host manager, and "add a second server" is exactly
              what someone with one host wants to do. */}
          <HostPicker
            hosts={hosts}
            selectedURL={selectedHost?.url || ""}
            setSelectedURL={setSelectedURL}
            health={hostHealth}
            agentID={snapshot?.agent_id}
            onManage={() => setManaging(true)}
            backendRows={backendRows}
            backendHostID={backendHostID}
            setBackendHostID={setBackendHostID}
          />
        </div>
        <div className="flex items-center gap-3">
          <div className="flex items-center gap-2 text-[11px] font-mono">
            {/* Red means something failed, not merely that nothing is
                connected yet. Idle takes the neutral colour, or the header
                claims an outage on a dashboard that has not been pointed at
                anything. */}
            <span
              className="w-2 h-2 rounded-full"
              style={{ background: connected ? "var(--good)" : error ? "var(--crit)" : "var(--ink-4)" }}
            />
            {/* title carries the diagnosis — which layer failed and what to
                check — without spending header width on it. */}
            <span
              style={{ color: connected ? "var(--ink-3)" : error ? "var(--crit)" : "var(--ink-4)" }}
              title={error?.detail || ""}
            >
              {/* Four states, not three. "Not loading, not connected, no
                  error" is idle — nothing has been asked to connect — and it
                  used to be unreachable because the poll never stopped
                  loading when there was no host. Fixing that made this branch
                  reachable and it read error.message off null, which took the
                  whole page down rather than showing a status. */}
              {loading
                ? "connecting…"
                : connected
                  ? readingBackend ? "from backend" : "live"
                  : error
                    ? error.message
                    : "no host selected"}
            </span>
          </div>
          {/* Pausing stops the agent poll. There is no agent poll to stop
              while reading the backend, so the control is disabled rather
              than left looking operable and doing nothing. */}
          <button
            onClick={() => setPaused(!paused)}
            disabled={readingBackend}
            title={readingBackend ? "Reading stored data from the backend — nothing to pause" : ""}
            className="flex items-center gap-1.5 text-[11px] font-mono px-2.5 py-1.5 rounded bg-[var(--surface)] border border-[var(--n4)] text-[var(--ink-3)] disabled:opacity-40 disabled:cursor-not-allowed"
          >
            {paused ? <Play size={12} /> : <Pause size={12} />}{paused ? "Resume" : "Pause"}
          </button>
          <ThemeSwitch theme={theme} setTheme={setTheme} />
        </div>
      </div>

      {/* A reload that could not apply everything used to be visible only in
          the agent's log — the one place nobody looks after editing a config
          and seeing "reload" succeed. The setting is silently not in effect
          until someone restarts. */}
      {snapshot?.reload_pending_restart?.length > 0 && (
        <div
          className="flex items-start gap-2 px-5 py-2.5 text-[11px] font-mono border-b"
          style={{
            background: "color-mix(in srgb, var(--warn) 8%, var(--surface))",
            borderColor: "color-mix(in srgb, var(--warn) 25%, transparent)",
            color: "var(--ink-2)",
          }}
          role="status"
        >
          <AlertTriangle size={13} className="flex-shrink-0 mt-0.5" style={{ color: "var(--warn)" }} />
          <span>
            <span style={{ color: "var(--warn)" }}>restart required</span>
            <span className="text-[var(--ink-3)]">
              {" "}— the last reload could not apply{" "}
              {snapshot.reload_pending_restart.join(", ")}. The running agent does
              not match its config file until it is restarted.
            </span>
          </span>
        </div>
      )}

      {/* The diagnosis, not just the symptom. A connection failure here is
          almost never the agent — it is a stopped process or a closed tunnel
          one layer in front of it, and saying which saves the round trip of
          going to look at a healthy agent. */}
      {error && (
        <div
          className="flex items-start gap-2 px-5 py-2.5 text-[11px] font-mono border-b"
          style={{
            background: "color-mix(in srgb, var(--crit) 8%, var(--surface))",
            borderColor: "color-mix(in srgb, var(--crit) 25%, transparent)",
            color: "var(--ink-2)",
          }}
          role="status"
        >
          <AlertTriangle size={13} className="flex-shrink-0 mt-0.5" style={{ color: "var(--crit)" }} />
          <span>
            <span style={{ color: "var(--crit)" }}>{error.message}</span>
            {error.detail && <span className="text-[var(--ink-3)]"> — {error.detail}</span>}
            {snapshot && (
              <span className="text-[var(--ink-4)]"> Showing the last successful poll.</span>
            )}
            {/* The way out of the dead end.
                An unreachable agent used to leave nothing to do here: the
                address is a forwarded port, the tunnel is down, and the fix
                was to go edit the host list. But the backend usually holds
                this host's telemetry already — it arrives by the host pushing
                outward, which needs no route from this browser — so the
                honest response to "cannot reach the agent" is to offer the
                copy that does not need reaching. */}
            {backendFallback && (
              <>
                {/* Counting rows and calling them "reporting" was a claim the
                    inventory cannot support: a host is listed the moment
                    anything arrives carrying its identity, so a fleet can hold
                    machines that have never sent a data point. Saying they are
                    reporting and then opening an empty view reads as a broken
                    dashboard rather than an empty host, so the wording follows
                    what is actually there. */}
                <span className="text-[var(--ink-4)]">
                  {" "}
                  {backendFallback.matched
                    ? backendFallback.hasData
                      ? "This host is also reporting to the backend."
                      : "The backend knows this host but has no metrics from it."
                    : backendFallback.reporting > 0
                      ? `${backendFallback.reporting} host${backendFallback.reporting === 1 ? " is" : "s are"} reporting to the backend.`
                      : `${backendFallback.total} host${backendFallback.total === 1 ? " is" : "s are"} registered with the backend, none reporting metrics.`}
                </span>{" "}
                <button
                  onClick={() => setBackendHostID(backendFallback.hostID)}
                  className="underline underline-offset-2"
                  style={{ color: "var(--accent)" }}
                >
                  {backendFallback.matched
                    ? `Read ${backendFallback.label} from the backend`
                    : backendFallback.hasData
                      ? `Open ${backendFallback.label}`
                      : `Open ${backendFallback.label} (no metrics yet)`}
                </button>
              </>
            )}
          </span>
        </div>
      )}

      <div className="flex flex-1 min-h-0">
        {managing && (
          <HostManager
            hosts={hosts}
            setHosts={setHosts}
            health={hostHealth}
            onClose={() => setManaging(false)}
          />
        )}
        <Sidebar view={view} setView={setView} hostCount={hosts.length} fleetCount={backendRows.length} />

        <div className="flex-1 min-w-0 p-5 overflow-y-auto">
          <div className="flex items-center gap-2 text-[11px] text-[var(--ink-5)] font-mono mb-3">
            <span>agent-i</span><ChevronRight size={11} /><span className="text-[var(--ink-3)]">{activeLabel}</span>
          </div>

          {view === "overview" && (
            <OverviewView
              snap={snapshot}
              d={d}
              openService={(id) => { setSelected(id); setView("topology"); }}
              openHost={() => setView("infra")}
              openLogs={() => setView("logs")}
            />
          )}
          {view === "fleet" && (
            <FleetView
              entries={fleetEntries}
              loading={usingBackend ? backendLoading : fleetLoading}
              source={usingBackend ? "backend" : "agents"}
              sourceError={usingBackend ? backendError : null}
              onOpen={(host, row) => {
                if (host) {
                  setBackendHostID("");
                  setSelectedURL(host.url);
                } else {
                  // No route to the machine, so its telemetry comes from the
                  // backend. This is the case the backend exists for and the
                  // one that used to dead-end at a row you could not open.
                  setBackendHostID(row.instanceID);
                }
                // SigNoz opens the host's detail, not a generic overview.
                setView("infra");
              }}
            />
          )}
          {view === "topology" && <TopologyView d={d} selected={selected} setSelected={setSelected} />}
          {view === "logs" && <LogsView logs={d.logs} />}
          {view === "metrics" && <MetricsView d={d} />}
          {view === "traces" && <TracesView traces={d.traces} />}
          {view === "infra" && <InfrastructureView snap={snapshot} d={d} />}

          {view === "problems" && (
            <NotWired title="Problems"
              why="Auto-detected problems with a probable root cause need a correlation engine that watches signals over time, groups related anomalies, and ranks causes. The agent collects and forwards; it does not analyse. This is the largest of the unbuilt pieces."
              needs="a correlation/root-cause service" />
          )}
          {view === "exceptions" && (
            <NotWired title="Exceptions"
              why="Exception grouping needs span events (exception.type, exception.stacktrace) extracted from incoming spans and aggregated by type. The receiver currently keeps span attributes but does not read span events, which is where OTel puts exceptions."
              needs="span-event extraction in the OTLP receiver" />
          )}
          {view === "monitors" && (
            <NotWired title="Monitors"
              why="Monitors are rule definitions plus evaluation state — thresholds, for-duration, notification routing. None of that exists in the agent, and it is the kind of thing that belongs in the backend that outlives any single host anyway."
              needs="an alerting rule engine" />
          )}

          <div className="flex items-center gap-2 text-[10px] text-[var(--ink-5)] font-mono mt-5">
            <Cpu size={11} />
            {connected
              ? readingBackend
                ? `from the backend · ${snapshot.counts?.series ?? 0} series · ${snapshot.counts?.logs ?? 0} logs · ${snapshot.counts?.spans ?? 0} spans · ${snapshot.retain_sec}s window`
                : `live from agent-i · ${d.envelopes.toLocaleString()} envelopes · ${snapshot.retain_sec}s window`
              : hosts.length === 0 && backendRows.length === 0
                ? "no agents configured and nothing reporting to the backend"
                : "not connected to an agent"}
          </div>
        </div>
      </div>
    </div>
  );
}
