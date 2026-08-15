import React, { useState, useEffect, useMemo } from "react";
import {
  LineChart, Line, AreaChart, Area, BarChart, Bar,
  XAxis, YAxis, CartesianGrid, Tooltip, ResponsiveContainer,
} from "recharts";
import {
  Activity, AlertTriangle, CheckCircle2, XCircle, Server,
  Cpu, MemoryStick, Gauge, Clock, Search, Bell, ChevronRight,
  LayoutDashboard, ScrollText, Waypoints, HardDrive,
  Network, PlugZap, Pause, Play, Sun, Moon, Monitor,
  ChevronUp, ChevronDown, X, Braces,
} from "lucide-react";

import { useSnapshot } from "./api";
import { useTheme } from "./useTheme";
import {
  deriveServices, deriveTraces, deriveEdges, layoutTopology,
  deriveLogs, deriveInfra, deriveTraffic, deriveAllSeries, globalStats,
  fmtRps, hostMetricPanels, fmtMetric, MAX_SERIES_PER_PANEL, flattenFields,
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

function TopologyGraph({ services, edges, positions, selected, onSelect }) {
  if (!services.length) return <EmptyHint>no services — send spans to the agent's OTLP receiver</EmptyHint>;
  return (
    <svg viewBox="0 0 460 190" className="w-full h-[220px]">
      <defs>
        <marker id="arrow" markerWidth="8" markerHeight="8" refX="7" refY="4" orient="auto">
          <path d="M0,0 L8,4 L0,8 z" fill="var(--n5)" />
        </marker>
      </defs>
      {edges.map(([from, to], i) => {
        const a = positions[from], b = positions[to];
        if (!a || !b) return null;
        return <line key={i} x1={a.x} y1={a.y} x2={b.x} y2={b.y} stroke="var(--n5)" strokeWidth="1.5" markerEnd="url(#arrow)" />;
      })}
      {services.map((s) => {
        const p = positions[s.id];
        if (!p) return null;
        const isSelected = selected === s.id;
        return (
          <g key={s.id} transform={`translate(${p.x},${p.y})`} className="cursor-pointer" onClick={() => onSelect(s.id)}>
            {s.status !== "healthy" && (
              <circle r="20" fill={statusColor[s.status]} opacity="0.15">
                <animate attributeName="r" values="14;24;14" dur="2s" repeatCount="indefinite" />
                <animate attributeName="opacity" values="0.25;0;0.25" dur="2s" repeatCount="indefinite" />
              </circle>
            )}
            <circle r="13" fill="var(--n2)" stroke={isSelected ? "var(--accent)" : statusColor[s.status]} strokeWidth={isSelected ? 2.5 : 1.5} />
            <circle r="3.5" fill={statusColor[s.status]} />
            <text y="26" textAnchor="middle" className="font-mono" fontSize="9.5" fill={isSelected ? "var(--accent)" : "var(--ink-3)"}>
              {s.label}
            </text>
          </g>
        );
      })}
    </svg>
  );
}

function ServiceDetail({ svc, edges }) {
  if (!svc) return <EmptyHint>no service selected</EmptyHint>;
  const related = (edges || []).filter(([f, t]) => f === svc.id || t === svc.id);
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
            {related.map(([from, to], i) => (
              <div key={i} className="flex items-center gap-1.5 text-[11px] font-mono text-[var(--ink-2)]">
                <span className={from === svc.id ? "text-[var(--accent)]" : ""}>{from}</span>
                <ChevronRight size={11} className="text-[var(--ink-5)]" />
                <span className={to === svc.id ? "text-[var(--accent)]" : ""}>{to}</span>
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

// Overview summarises; it does not re-host other views' tables. Where a panel
// shows the same entity another view owns, clicking it navigates there rather
// than selecting in place — a selection that changes nothing on screen is a
// control that lies about what it does.
function OverviewView({ snap, d, openService, openHost, openLogs }) {
  const healthy = d.services.filter((s) => s.status === "healthy").length;
  const infra = d.infra[0];

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
        <KpiTile icon={Activity} label="Envelopes" value={d.envelopes.toLocaleString()} sub={`${d.envelopesPerSec}/s since start`} />
      </div>

      {d.seriesDropped > 0 && (
        <div className="mb-4 border border-[var(--warn)] border-l-2 rounded px-3 py-2 text-[12px] text-[var(--ink-2)] bg-[color-mix(in srgb, var(--warn) 4%, transparent)]">
          {d.seriesDropped} series refused — the agent's in-memory cap was reached, so this view is incomplete.
          Raise <span className="font-mono text-[var(--warn)]">dashboard.max_series</span> or narrow what is collected.
        </div>
      )}

      {/* RED hero row. Rate and errors on the left, duration on the right —
          the convention every platform surveyed follows, and the order an
          operator actually asks the questions in: is it serving, is it
          broken, is it slow. These are taller than everything below them so
          the layout itself says which panels matter; a uniform grid makes you
          scan all of them equally, every time. */}
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

      {/* Secondary: context for the row above, deliberately shorter. */}
      <div className="grid grid-cols-1 lg:grid-cols-3 gap-4 mt-4">
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
            <EmptyHint>
              no services yet — enable <span className="text-[var(--accent)]">traces.enabled</span> and point an app at the agent's OTLP receiver
            </EmptyHint>
          )}
        </Panel>

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
      if (e.key === "Escape") return onClose();
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

function LogsView({ logs }) {
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
  const positions = useMemo(() => layoutTopology(d.services, d.edges), [d.services, d.edges]);
  const svc = d.services.find((s) => s.id === selected) || d.services[0];

  return (
    <div className="grid grid-cols-1 lg:grid-cols-3 gap-4">
      <Panel title="Service Topology" className="lg:col-span-2">
        <TopologyGraph services={d.services} edges={d.edges} positions={positions} selected={svc?.id} onSelect={setSelected} />
        <div className="flex items-center justify-between mt-3 pt-3 border-t border-[var(--n3)]">
          <div className="flex items-center gap-4">
            {["healthy", "degraded"].map((s) => (
              <div key={s} className="flex items-center gap-1.5">
                <span className="w-2 h-2 rounded-full" style={{ background: statusColor[s] }} />
                <span className="text-[10px] text-[var(--ink-3)] font-mono uppercase">{s}</span>
              </div>
            ))}
          </div>
          <span className="text-[10px] font-mono text-[var(--ink-5)]">
            {d.edges.length} edge{d.edges.length === 1 ? "" : "s"} derived from span parent links
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
  const panels = useMemo(() => hostMetricPanels(snap), [snap]);

  if (!d.infra.length) {
    return <NotWired title="Infrastructure" why="No host metrics received. Set metrics.enabled: true in the agent config." needs="metrics.enabled" />;
  }
  const n = d.infra[0];
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
          {/* The agent reads /proc for every metric here and only builds for
              Linux, so this is a property of the binary, not a guess. */}
          <Fact label="Operating system">linux</Fact>
          <Fact label="CPU usage"><GaugeBar value={n.cpu} /></Fact>
          <Fact label="Memory usage"><GaugeBar value={n.mem} /></Fact>
        </div>
      </div>

      <div className="flex items-end justify-between gap-4">
        <HostTabs
          tab={tab} setTab={setTab}
          tabs={[
            { id: "metrics", label: "Metrics", icon: Gauge },
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
      {tab === "logs" && <LogsView logs={d.logs} />}
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

const NAV_GROUPS = [
  { label: "Monitor", items: [
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
const NAV_ITEMS = NAV_GROUPS.flatMap((g) => g.items);

function Sidebar({ view, setView, snap }) {
  return (
    <div className="w-[200px] flex-shrink-0 border-r border-[var(--n2)] flex flex-col py-4 overflow-y-auto">
      {NAV_GROUPS.map((group) => (
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
      <div className="mt-auto px-4 pt-4 border-t border-[var(--n2)] mx-2 flex flex-col gap-2">
        <div className="text-[10px] text-[var(--ink-5)] font-mono leading-relaxed">
          agent-i {snap?.version || "—"}<br />{snap?.agent_id || "not connected"}
        </div>
        {/* Where to point an instrumented app. Every question about this
            ends up being "what URL do I send to", so it belongs on screen
            rather than in a config file someone has to go and read. */}
        <div className="text-[9.5px] text-[var(--ink-5)] font-mono leading-relaxed">
          <div className="uppercase tracking-widest pb-0.5">send traces to</div>
          <div className="text-[var(--ink-4)] break-all">{window.location.origin}/v1/traces</div>
        </div>
        <a href="/agent" target="_blank" rel="noreferrer"
           className="text-[9.5px] font-mono text-[var(--ink-5)] hover:text-[var(--accent)] underline decoration-dotted">
          agent's built-in page ↗
        </a>
      </div>
    </div>
  );
}

export default function ObservabilityDashboard() {
  const [view, setView] = useState("overview");
  const [selected, setSelected] = useState(null);
  const [now, setNow] = useState(new Date());
  const { snapshot, error, loading, paused, setPaused } = useSnapshot(5000);
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
      traffic: deriveTraffic(snapshot),
      allSeries: deriveAllSeries(snapshot),
    };
  }, [snapshot]);

  const activeLabel = NAV_ITEMS.find((n) => n.id === view)?.label;
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
              {snapshot?.agent_id || "—"} · {now.toLocaleTimeString()}
            </p>
          </div>
        </div>
        <div className="flex items-center gap-3">
          <div className="flex items-center gap-2 text-[11px] font-mono">
            <span className="w-2 h-2 rounded-full" style={{ background: connected ? "var(--good)" : "var(--crit)" }} />
            {/* title carries the diagnosis — which layer failed and what to
                check — without spending header width on it. */}
            <span style={{ color: connected ? "var(--ink-3)" : "var(--crit)" }} title={error?.detail || ""}>
              {loading ? "connecting…" : connected ? "live" : error.message}
            </span>
          </div>
          <button onClick={() => setPaused(!paused)}
            className="flex items-center gap-1.5 text-[11px] font-mono px-2.5 py-1.5 rounded bg-[var(--surface)] border border-[var(--n4)] text-[var(--ink-3)]">
            {paused ? <Play size={12} /> : <Pause size={12} />}{paused ? "Resume" : "Pause"}
          </button>
          <ThemeSwitch theme={theme} setTheme={setTheme} />
        </div>
      </div>

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
          </span>
        </div>
      )}

      <div className="flex flex-1 min-h-0">
        <Sidebar view={view} setView={setView} snap={snapshot} />

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
              ? `live from agent-i · ${d.envelopes.toLocaleString()} envelopes · ${snapshot.retain_sec}s window`
              : "not connected to an agent"}
          </div>
        </div>
      </div>
    </div>
  );
}
