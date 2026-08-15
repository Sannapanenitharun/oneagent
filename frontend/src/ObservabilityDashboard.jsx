import React, { useState, useEffect, useMemo } from "react";
import {
  LineChart, Line, AreaChart, Area, BarChart, Bar,
  XAxis, YAxis, CartesianGrid, Tooltip, ResponsiveContainer,
} from "recharts";
import {
  Activity, AlertTriangle, CheckCircle2, XCircle, Server,
  Cpu, MemoryStick, Gauge, Clock, Search, Bell, ChevronRight,
  LayoutDashboard, ScrollText, Waypoints, HardDrive,
  Network, PlugZap, Pause, Play,
} from "lucide-react";

import { useSnapshot } from "./api";
import {
  deriveServices, deriveTraces, deriveEdges, layoutTopology,
  deriveLogs, deriveInfra, deriveTraffic, deriveAllSeries, globalStats,
  pick, prepare, sumBy, toChart, fmtRps,
} from "./adapters";

const statusColor = { healthy: "#4ADE80", degraded: "#FBBF24", down: "#F87171" };
const lvlColor = { ERROR: "#F87171", WARN: "#FBBF24", INFO: "#7B8496", DEBUG: "#5A6273", TRACE: "#5A6273" };

const SERVICE_PALETTE = ["#5EEBD1", "#60A5FA", "#C084FC", "#FB923C", "#F472B6", "#A3E635"];
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
    <div className={`bg-[#12161F] border border-[#232838] rounded-lg ${className}`}>
      <div className="flex items-center justify-between px-4 py-3 border-b border-[#232838]">
        <h3 className="text-[11px] tracking-widest uppercase text-[#7B8496] font-mono">{title}</h3>
        {right}
      </div>
      <div className="p-4">{children}</div>
    </div>
  );
}

function KpiTile({ icon: Icon, label, value, sub, tone = "normal" }) {
  const toneColor = tone === "bad" ? "#F87171" : tone === "warn" ? "#FBBF24" : "#E6E9F0";
  return (
    <div className="bg-[#12161F] border border-[#232838] rounded-lg px-4 py-3 flex flex-col gap-1">
      <div className="flex items-center gap-2 text-[#7B8496]">
        <Icon size={13} />
        <span className="text-[10px] tracking-widest uppercase font-mono">{label}</span>
      </div>
      <div className="font-mono text-2xl leading-none" style={{ color: toneColor }}>{value}</div>
      {sub && <div className="text-[11px] text-[#7B8496]">{sub}</div>}
    </div>
  );
}

function ChartTooltip({ active, payload, label, unit }) {
  if (!active || !payload || !payload.length) return null;
  return (
    <div className="bg-[#181C27] border border-[#2A3040] rounded px-2 py-1.5 text-[11px] font-mono">
      <div className="text-[#7B8496]">{label}</div>
      <div className="text-[#E6E9F0]">{payload[0].value}{unit}</div>
    </div>
  );
}

function GaugeBar({ value, warn = 70, bad = 90 }) {
  const color = value >= bad ? "#F87171" : value >= warn ? "#FBBF24" : "#5EEBD1";
  return (
    <div className="flex items-center gap-2">
      <div className="w-20 h-1.5 rounded-full bg-[#1B2030] overflow-hidden">
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
        <PlugZap size={20} className="text-[#3A4154]" />
        <div className="text-[13px] text-[#B4BACB]">Not available from the agent yet</div>
        <div className="text-[12px] text-[#7B8496] max-w-md leading-relaxed">{why}</div>
        <div className="text-[11px] font-mono text-[#5EEBD1] mt-1">needs: {needs}</div>
      </div>
    </Panel>
  );
}

function EmptyHint({ children }) {
  return <div className="text-[#3A4154] text-[12px] py-6 text-center font-mono">{children}</div>;
}

function TopologyGraph({ services, edges, positions, selected, onSelect }) {
  if (!services.length) return <EmptyHint>no services — send spans to the agent's OTLP receiver</EmptyHint>;
  return (
    <svg viewBox="0 0 460 190" className="w-full h-[220px]">
      <defs>
        <marker id="arrow" markerWidth="8" markerHeight="8" refX="7" refY="4" orient="auto">
          <path d="M0,0 L8,4 L0,8 z" fill="#2A3040" />
        </marker>
      </defs>
      {edges.map(([from, to], i) => {
        const a = positions[from], b = positions[to];
        if (!a || !b) return null;
        return <line key={i} x1={a.x} y1={a.y} x2={b.x} y2={b.y} stroke="#2A3040" strokeWidth="1.5" markerEnd="url(#arrow)" />;
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
            <circle r="13" fill="#181C27" stroke={isSelected ? "#5EEBD1" : statusColor[s.status]} strokeWidth={isSelected ? 2.5 : 1.5} />
            <circle r="3.5" fill={statusColor[s.status]} />
            <text y="26" textAnchor="middle" className="font-mono" fontSize="9.5" fill={isSelected ? "#5EEBD1" : "#7B8496"}>
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
        <span className="text-[10px] font-mono uppercase px-2 py-0.5 rounded" style={{ color: statusColor[svc.status], background: `${statusColor[svc.status]}1A` }}>
          {svc.status}
        </span>
      </div>
      <div className="grid grid-cols-2 gap-3 text-sm mb-4">
        <div><div className="text-[10px] text-[#7B8496] font-mono uppercase">p50</div><div className="font-mono">{svc.p50}ms</div></div>
        <div><div className="text-[10px] text-[#7B8496] font-mono uppercase">p99</div><div className="font-mono" style={{ color: svc.p99 > 300 ? "#F87171" : "#E6E9F0" }}>{svc.p99}ms</div></div>
        <div><div className="text-[10px] text-[#7B8496] font-mono uppercase">req/s</div><div className="font-mono">{fmtRps(svc.rps)}</div></div>
        <div><div className="text-[10px] text-[#7B8496] font-mono uppercase">error rate</div><div className="font-mono" style={{ color: svc.err > 1 ? "#F87171" : "#E6E9F0" }}>{svc.err}%</div></div>
      </div>
      {related.length > 0 && (
        <>
          <div className="text-[10px] text-[#7B8496] font-mono uppercase mb-1.5">Upstream / Downstream</div>
          <div className="flex flex-col gap-1">
            {related.map(([from, to], i) => (
              <div key={i} className="flex items-center gap-1.5 text-[11px] font-mono text-[#B4BACB]">
                <span className={from === svc.id ? "text-[#5EEBD1]" : ""}>{from}</span>
                <ChevronRight size={11} className="text-[#3A4154]" />
                <span className={to === svc.id ? "text-[#5EEBD1]" : ""}>{to}</span>
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
          <tr className="text-[10px] text-[#7B8496] uppercase tracking-wide text-left border-b border-[#232838]">
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
              className="border-b border-[#161A24] last:border-0 cursor-pointer hover:bg-[#161A24]"
              style={{ background: selected === s.id ? "#5EEBD10D" : "transparent" }}
            >
              <td className="py-2 pr-4" style={{ color: selected === s.id ? "#5EEBD1" : "#E6E9F0" }}>{s.label}</td>
              <td className="py-2 pr-4"><StatusDot status={s.status} />{s.status}</td>
              <td className="py-2 pr-4">{s.p50}ms</td>
              <td className="py-2 pr-4" style={{ color: s.p99 > 300 ? "#F87171" : "#E6E9F0" }}>{s.p99}ms</td>
              <td className="py-2 pr-4">{fmtRps(s.rps)}</td>
              <td className="py-2 pr-4" style={{ color: s.err > 1 ? "#F87171" : "#E6E9F0" }}>{s.err}%</td>
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

function OverviewView({ snap, d, selected, setSelected }) {
  const healthy = d.services.filter((s) => s.status === "healthy").length;
  const svc = d.services.find((s) => s.id === selected) || d.services[0];
  const infra = d.infra[0];

  return (
    <>
      <div className="grid grid-cols-2 md:grid-cols-4 gap-3 mb-4">
        <KpiTile icon={Server} label="Services" value={d.services.length ? `${healthy}/${d.services.length}` : "—"}
          tone={d.services.length && healthy < d.services.length ? "warn" : "normal"} sub="healthy / seen" />
        <KpiTile icon={Gauge} label="Requests / sec" value={d.services.length ? fmtRps(d.totalRps) : "—"} sub="from received spans" />
        <KpiTile icon={Clock} label="p99 Latency" value={Number.isFinite(d.p99) ? `${d.p99}ms` : "—"} sub="worst service" />
        <KpiTile icon={Activity} label="Envelopes" value={d.envelopes.toLocaleString()} sub={`${d.envelopesPerSec}/s since start`} />
      </div>

      {d.seriesDropped > 0 && (
        <div className="mb-4 border border-[#FBBF24] border-l-2 rounded px-3 py-2 text-[12px] text-[#B4BACB] bg-[#FBBF240A]">
          {d.seriesDropped} series refused — the agent's in-memory cap was reached, so this view is incomplete.
          Raise <span className="font-mono text-[#FBBF24]">dashboard.max_series</span> or narrow what is collected.
        </div>
      )}

      <div className="grid grid-cols-1 lg:grid-cols-3 gap-4">
        <Panel title="Service Health" className="lg:col-span-2">
          {d.services.length ? (
            <div className="flex flex-col gap-1.5">
              {d.services.map((s) => (
                <button key={s.id} onClick={() => setSelected(s.id)}
                  className="flex items-center justify-between px-2.5 py-2 rounded border text-left"
                  style={{ borderColor: selected === s.id ? "#5EEBD1" : "#1B2030", background: selected === s.id ? "#5EEBD10D" : "transparent" }}>
                  <span className="flex items-center font-mono text-[12.5px]"><StatusDot status={s.status} />{s.label}</span>
                  <span className="flex items-center gap-3 text-[11px] font-mono text-[#7B8496]">
                    <span>{fmtRps(s.rps)} rps</span>
                    <span style={{ color: s.p99 > 300 ? "#F87171" : "#7B8496" }}>{s.p99}ms p99</span>
                  </span>
                </button>
              ))}
            </div>
          ) : (
            <EmptyHint>
              no services yet — enable <span className="text-[#5EEBD1]">traces.enabled</span> and point an app at the agent's OTLP receiver
            </EmptyHint>
          )}
        </Panel>

        <Panel title="This Host" right={infra && <StatusDot status={infra.status} />}>
          {infra ? (
            <>
              <div className="flex items-center justify-between mb-3">
                <span className="font-mono text-base">{infra.host}</span>
                <span className="text-[10px] font-mono text-[#7B8496]">{infra.role}</span>
              </div>
              <div className="flex flex-col gap-2.5">
                <div className="flex items-center justify-between">
                  <span className="flex items-center gap-1.5 text-[11px] text-[#7B8496] font-mono"><Cpu size={12} /> CPU</span>
                  <GaugeBar value={infra.cpu} />
                </div>
                <div className="flex items-center justify-between">
                  <span className="flex items-center gap-1.5 text-[11px] text-[#7B8496] font-mono"><MemoryStick size={12} /> Memory</span>
                  <GaugeBar value={infra.mem} />
                </div>
                <div className="flex items-center justify-between">
                  <span className="flex items-center gap-1.5 text-[11px] text-[#7B8496] font-mono"><HardDrive size={12} /> Disk (worst)</span>
                  <GaugeBar value={infra.disk} />
                </div>
                <div className="flex items-center justify-between text-[11px] font-mono">
                  <span className="text-[#7B8496]">load 1m</span><span>{infra.load1}</span>
                </div>
              </div>
            </>
          ) : <EmptyHint>no host metrics — enable metrics.enabled</EmptyHint>}
        </Panel>
      </div>

      <div className="grid grid-cols-1 lg:grid-cols-3 gap-4 mt-4">
        <Panel title="Throughput (req/s)">
          {d.traffic.rps.length > 1 ? (
            <ResponsiveContainer width="100%" height={140}>
              <AreaChart data={d.traffic.rps}>
                <defs>
                  <linearGradient id="rpsFill" x1="0" y1="0" x2="0" y2="1">
                    <stop offset="0%" stopColor="#5EEBD1" stopOpacity={0.35} />
                    <stop offset="100%" stopColor="#5EEBD1" stopOpacity={0} />
                  </linearGradient>
                </defs>
                <CartesianGrid stroke="#1B2030" vertical={false} />
                <XAxis dataKey="time" hide /><YAxis hide />
                <Tooltip content={<ChartTooltip unit=" req/s" />} />
                <Area type="monotone" dataKey="value" stroke="#5EEBD1" strokeWidth={1.5} fill="url(#rpsFill)" />
              </AreaChart>
            </ResponsiveContainer>
          ) : <EmptyHint>needs spans</EmptyHint>}
        </Panel>

        <Panel title="Latency p99 (ms)">
          {d.traffic.latency.length > 1 ? (
            <ResponsiveContainer width="100%" height={140}>
              <LineChart data={d.traffic.latency}>
                <CartesianGrid stroke="#1B2030" vertical={false} />
                <XAxis dataKey="time" hide /><YAxis hide />
                <Tooltip content={<ChartTooltip unit="ms" />} />
                <Line type="monotone" dataKey="value" stroke="#FBBF24" strokeWidth={1.5} dot={false} />
              </LineChart>
            </ResponsiveContainer>
          ) : <EmptyHint>needs spans</EmptyHint>}
        </Panel>

        <Panel title="Errors / min">
          {d.traffic.errors.length > 1 ? (
            <ResponsiveContainer width="100%" height={140}>
              <BarChart data={d.traffic.errors}>
                <CartesianGrid stroke="#1B2030" vertical={false} />
                <XAxis dataKey="time" hide /><YAxis hide />
                <Tooltip content={<ChartTooltip unit=" errs" />} />
                <Bar dataKey="value" fill="#F87171" radius={[2, 2, 0, 0]} />
              </BarChart>
            </ResponsiveContainer>
          ) : <EmptyHint>needs spans</EmptyHint>}
        </Panel>
      </div>

      <div className="mt-4">
        <Panel title="Live Log Stream">
          {d.logs.length ? (
            <div className="flex flex-col gap-1 max-h-[220px] overflow-y-auto font-mono text-[11.5px]">
              {d.logs.slice(0, 10).map((l, i) => (
                <div key={i} className="flex gap-3 py-1 border-b border-[#161A24] last:border-0">
                  <span className="text-[#3A4154] flex-shrink-0">{l.t}</span>
                  <span className="w-12 flex-shrink-0 font-semibold" style={{ color: lvlColor[l.lvl] }}>{l.lvl}</span>
                  <span className="text-[#5EEBD1] flex-shrink-0 w-28 truncate">{l.svc}</span>
                  <span className="text-[#B4BACB] truncate">{l.msg}</span>
                </div>
              ))}
            </div>
          ) : (
            <EmptyHint>
              no log lines — enable <span className="text-[#5EEBD1]">logs.enabled</span> with a matching path in logs.paths
            </EmptyHint>
          )}
        </Panel>
      </div>
    </>
  );
}

function LogsView({ logs }) {
  const [filter, setFilter] = useState("ALL");
  const [q, setQ] = useState("");

  const filtered = logs.filter((l) =>
    (filter === "ALL" || l.lvl === filter) &&
    (q === "" || l.msg.toLowerCase().includes(q.toLowerCase()) || l.svc.toLowerCase().includes(q.toLowerCase()))
  );

  return (
    <Panel
      title="Log Explorer"
      right={
        <div className="flex items-center gap-2">
          {["ALL", "ERROR", "WARN", "INFO"].map((l) => (
            <button key={l} onClick={() => setFilter(l)}
              className="text-[10px] font-mono uppercase px-2 py-0.5 rounded border"
              style={{
                color: filter === l ? "#0B0E14" : lvlColor[l] || "#7B8496",
                background: filter === l ? (lvlColor[l] || "#7B8496") : "transparent",
                borderColor: lvlColor[l] || "#3A4154",
              }}>
              {l}
            </button>
          ))}
        </div>
      }
    >
      <div className="flex items-center gap-2 bg-[#0E1119] border border-[#232838] rounded px-3 py-1.5 mb-3">
        <Search size={13} className="text-[#7B8496]" />
        <input value={q} onChange={(e) => setQ(e.target.value)}
          placeholder="filter by message or source…"
          className="bg-transparent outline-none text-[12px] font-mono flex-1 text-[#E6E9F0] placeholder:text-[#3A4154]" />
      </div>
      <div className="text-[10px] font-mono text-[#3A4154] mb-2">
        severity is classified from the line text — the agent forwards log lines verbatim and does not parse levels
      </div>
      <div className="flex flex-col gap-1 font-mono text-[12px] max-h-[520px] overflow-y-auto">
        {filtered.map((l, i) => (
          <div key={i} className="flex gap-3 py-1.5 border-b border-[#161A24] last:border-0 items-center">
            <span className="text-[#3A4154] flex-shrink-0 w-16">{l.t}</span>
            <span className="w-12 flex-shrink-0 font-semibold" style={{ color: lvlColor[l.lvl] }}>{l.lvl}</span>
            <span className="text-[#5EEBD1] flex-shrink-0 w-32 truncate" title={l.svc}>{l.svc}</span>
            <span className="text-[#B4BACB] truncate flex-1" title={l.msg}>{l.msg}</span>
          </div>
        ))}
        {!filtered.length && <EmptyHint>no logs match this filter</EmptyHint>}
      </div>
    </Panel>
  );
}

function MetricsView({ snap, d }) {
  const cpu = useMemo(() => {
    const series = pick(snap, "system.cpu.time").map((s) => ({ state: s.labels?.state, points: prepare(s) }));
    if (!series.length) return [];
    const totals = {};
    series.forEach((s) => s.points.forEach((p) => { totals[p.t] = (totals[p.t] || 0) + p.v; }));
    const busy = series.filter((s) => s.state !== "idle");
    const merged = {};
    busy.forEach((s) => s.points.forEach((p) => { merged[p.t] = (merged[p.t] || 0) + p.v; }));
    return Object.keys(merged).map(Number).sort((a, b) => a - b).map((t) => ({
      time: new Date(t).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }),
      value: totals[t] > 0 ? Math.round((merged[t] / totals[t]) * 1000) / 10 : 0,
    }));
  }, [snap]);

  const net = useMemo(() => {
    const groups = sumBy(snap, "system.network.io", "direction");
    const rx = groups.find((g) => g.key === "receive");
    return rx ? toChart(rx.points.map((p) => ({ t: p.t, v: p.v / 1024 }))) : [];
  }, [snap]);

  return (
    <div className="flex flex-col gap-4">
      <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
        <Panel title="CPU busy (%)">
          {cpu.length > 1 ? (
            <ResponsiveContainer width="100%" height={200}>
              <AreaChart data={cpu}>
                <defs>
                  <linearGradient id="cpuFill" x1="0" y1="0" x2="0" y2="1">
                    <stop offset="0%" stopColor="#5EEBD1" stopOpacity={0.35} />
                    <stop offset="100%" stopColor="#5EEBD1" stopOpacity={0} />
                  </linearGradient>
                </defs>
                <CartesianGrid stroke="#1B2030" vertical={false} />
                <XAxis dataKey="time" tick={{ fill: "#3A4154", fontSize: 10 }} axisLine={{ stroke: "#232838" }} tickLine={false} />
                <YAxis hide domain={[0, 100]} />
                <Tooltip content={<ChartTooltip unit="%" />} />
                <Area type="monotone" dataKey="value" stroke="#5EEBD1" strokeWidth={1.5} fill="url(#cpuFill)" />
              </AreaChart>
            </ResponsiveContainer>
          ) : <EmptyHint>needs system.cpu.time</EmptyHint>}
        </Panel>

        <Panel title="Network received (KiB/s)">
          {net.length > 1 ? (
            <ResponsiveContainer width="100%" height={200}>
              <LineChart data={net}>
                <CartesianGrid stroke="#1B2030" vertical={false} />
                <XAxis dataKey="time" tick={{ fill: "#3A4154", fontSize: 10 }} axisLine={{ stroke: "#232838" }} tickLine={false} />
                <YAxis hide />
                <Tooltip content={<ChartTooltip unit=" KiB/s" />} />
                <Line type="monotone" dataKey="value" stroke="#60A5FA" strokeWidth={1.5} dot={false} />
              </LineChart>
            </ResponsiveContainer>
          ) : <EmptyHint>needs system.network.io</EmptyHint>}
        </Panel>
      </div>

      <Panel title="Per-Service Metrics"><ServiceTable services={d.services} /></Panel>

      <Panel title={`All Series (${d.allSeries.length})`}>
        <div className="overflow-auto max-h-[420px]">
          <table className="w-full text-[12px] font-mono">
            <thead>
              <tr className="text-[10px] text-[#7B8496] uppercase tracking-wide text-left border-b border-[#232838] sticky top-0 bg-[#12161F]">
                <th className="py-2 pr-4 font-normal">Metric</th>
                <th className="py-2 pr-4 font-normal">Labels</th>
                <th className="py-2 pr-4 font-normal text-right">Latest</th>
                <th className="py-2 pr-4 font-normal text-right">Points</th>
              </tr>
            </thead>
            <tbody>
              {d.allSeries.map((s, i) => (
                <tr key={i} className="border-b border-[#161A24] last:border-0">
                  <td className="py-1.5 pr-4">
                    {s.name}
                    {s.cumulative && <span className="ml-2 text-[9px] px-1 py-0.5 rounded bg-[#1B2030] text-[#7B8496]">RATE</span>}
                  </td>
                  <td className="py-1.5 pr-4 text-[10.5px] text-[#7B8496] max-w-[340px] truncate" title={s.labels}>{s.labels || "—"}</td>
                  <td className="py-1.5 pr-4 text-right">{s.latest.toFixed(2)}</td>
                  <td className="py-1.5 pr-4 text-right text-[#7B8496]">{s.points}</td>
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
          style={{ background: selectedIdx === i ? "#5EEBD10D" : "transparent" }}>
          <div className="flex justify-between text-[11px] font-mono mb-1" style={{ paddingLeft: s.depth * 14 }}>
            <span className="flex items-center gap-1.5 truncate">
              {s.error && <AlertTriangle size={11} className="text-[#F87171] flex-shrink-0" />}
              <span style={{ color: serviceColor(s.svc) }}>{s.svc}</span>
              <span className="text-[#3A4154] truncate">· {s.op}</span>
            </span>
            <span className="text-[#7B8496] flex-shrink-0 pl-2">{s.dur}ms</span>
          </div>
          <div className="w-full h-2 bg-[#161A24] rounded-sm relative">
            <div className="absolute h-2 rounded-sm"
              style={{
                left: `${(s.start / maxDur) * 100}%`,
                width: `${Math.max((s.dur / maxDur) * 100, 1)}%`,
                background: s.error ? "#F87171" : serviceColor(s.svc),
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
                background: s.error ? "#F8717133" : `${serviceColor(s.svc)}33`,
                borderColor: selectedIdx === s.i ? "#5EEBD1" : s.error ? "#F8717166" : `${serviceColor(s.svc)}66`,
              }}>
              <span className="text-[10px] font-mono truncate" style={{ color: s.error ? "#F87171" : serviceColor(s.svc) }}>{s.svc}</span>
            </div>
          ))}
        </div>
      ))}
      <div className="flex items-center gap-4 mt-2 pt-2 border-t border-[#1B2030] flex-wrap">
        {[...new Set(trace.spans.map((s) => s.svc))].map((svc) => (
          <div key={svc} className="flex items-center gap-1.5">
            <span className="w-2 h-2 rounded-sm" style={{ background: serviceColor(svc) }} />
            <span className="text-[10px] text-[#7B8496] font-mono">{svc}</span>
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
              style={{ borderColor: trace.id === t.id ? "#5EEBD1" : "#1B2030", background: trace.id === t.id ? "#5EEBD10D" : "transparent" }}>
              <div className="flex items-center justify-between">
                <span className="font-mono text-[11px] text-[#5EEBD1] truncate">{t.id.slice(0, 16)}</span>
                <span className="font-mono text-[11px]" style={{ color: t.status === "error" ? "#F87171" : "#4ADE80" }}>{t.duration}ms</span>
              </div>
              <div className="text-[12px] mt-0.5 text-[#B4BACB] truncate">{t.op}</div>
              <div className="flex items-center justify-between">
                <span className="text-[10px] text-[#7B8496] font-mono">{t.root} · {t.spans.length} spans</span>
                {t.status === "error" && <AlertTriangle size={11} className="text-[#F87171]" />}
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
                  style={{ color: mode === m ? "#0B0E14" : "#7B8496", background: mode === m ? "#5EEBD1" : "transparent" }}>
                  {m === "waterfall" ? "Waterfall" : "Flame Graph"}
                </button>
              ))}
            </div>
          }>
          {mode === "waterfall"
            ? <SpanWaterfall trace={trace} onSelectSpan={setSelectedSpanIdx} selectedIdx={selectedSpanIdx} />
            : <FlameGraph trace={trace} onSelectSpan={setSelectedSpanIdx} selectedIdx={selectedSpanIdx} />}
        </Panel>

        <Panel title="Span Detail" right={span?.error && <span className="text-[10px] font-mono text-[#F87171]">ERROR</span>}>
          {span && (
            <>
              <div className="flex items-center justify-between mb-2">
                <span className="font-mono text-[13px]" style={{ color: serviceColor(span.svc) }}>{span.svc}</span>
                <span className="text-[11px] font-mono text-[#7B8496]">{span.dur}ms</span>
              </div>
              <div className="text-[12px] text-[#B4BACB] mb-3">{span.op}</div>
              <div className="text-[10px] font-mono text-[#3A4154]">
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
        <div className="flex items-center justify-between mt-3 pt-3 border-t border-[#1B2030]">
          <div className="flex items-center gap-4">
            {["healthy", "degraded"].map((s) => (
              <div key={s} className="flex items-center gap-1.5">
                <span className="w-2 h-2 rounded-full" style={{ background: statusColor[s] }} />
                <span className="text-[10px] text-[#7B8496] font-mono uppercase">{s}</span>
              </div>
            ))}
          </div>
          <span className="text-[10px] font-mono text-[#3A4154]">
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

function InfrastructureView({ d }) {
  if (!d.infra.length) return <NotWired title="Infrastructure" why="No host metrics received. Set metrics.enabled: true in the agent config." needs="metrics.enabled" />;
  return (
    <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
      {d.infra.map((n) => (
        <Panel key={n.host} title={n.host} right={<StatusDot status={n.status} />}>
          <div className="text-[11px] text-[#7B8496] font-mono mb-3">{n.role}</div>
          <div className="flex flex-col gap-2.5">
            <div className="flex items-center justify-between">
              <span className="flex items-center gap-1.5 text-[11px] text-[#7B8496] font-mono"><Cpu size={12} /> CPU</span>
              <GaugeBar value={n.cpu} />
            </div>
            <div className="flex items-center justify-between">
              <span className="flex items-center gap-1.5 text-[11px] text-[#7B8496] font-mono"><MemoryStick size={12} /> Memory</span>
              <GaugeBar value={n.mem} />
            </div>
            {n.mounts.map((m) => (
              <div key={m.mount} className="flex items-center justify-between">
                <span className="flex items-center gap-1.5 text-[11px] text-[#7B8496] font-mono truncate"><HardDrive size={12} /> {m.mount}</span>
                <GaugeBar value={m.pct} />
              </div>
            ))}
            <div className="flex items-center justify-between text-[11px] font-mono pt-1 border-t border-[#1B2030]">
              <span className="text-[#7B8496]">load 1m</span><span>{n.load1}</span>
            </div>
          </div>
        </Panel>
      ))}
      <Panel title="Note" className="lg:col-span-2">
        <div className="text-[12px] text-[#7B8496] leading-relaxed">
          One host, because this dashboard talks to one agent. A fleet view needs a backend that aggregates
          many agents — the agent's own dashboard is deliberately per-host, and SigNoz is where the fleet view lives.
        </div>
      </Panel>
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
    <div className="w-[200px] flex-shrink-0 border-r border-[#181C27] flex flex-col py-4 overflow-y-auto">
      {NAV_GROUPS.map((group) => (
        <nav key={group.label} className="flex flex-col gap-0.5 px-2 mb-3">
          <div className="px-3 pb-1 text-[9.5px] font-mono uppercase tracking-widest text-[#3A4154]">{group.label}</div>
          {group.items.map((item) => {
            const Icon = item.icon;
            const active = view === item.id;
            return (
              <button key={item.id} onClick={() => setView(item.id)}
                className="flex items-center gap-2.5 px-3 py-2 rounded text-[12.5px] font-mono transition-colors"
                style={{
                  color: active ? "#5EEBD1" : "#7B8496",
                  background: active ? "#5EEBD114" : "transparent",
                  borderLeft: active ? "2px solid #5EEBD1" : "2px solid transparent",
                }}>
                <Icon size={14} />
                <span className="flex-1 text-left">{item.label}</span>
              </button>
            );
          })}
        </nav>
      ))}
      <div className="mt-auto px-4 pt-4 border-t border-[#181C27] mx-2">
        <div className="text-[10px] text-[#3A4154] font-mono leading-relaxed">
          agent-i {snap?.version || "—"}<br />{snap?.agent_id || "not connected"}
        </div>
      </div>
    </div>
  );
}

export default function ObservabilityDashboard() {
  const [view, setView] = useState("overview");
  const [selected, setSelected] = useState(null);
  const [now, setNow] = useState(new Date());
  const { snapshot, error, loading, paused, setPaused } = useSnapshot(5000);

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
    <div className="min-h-screen w-full bg-[#0B0E14] text-[#E6E9F0] font-sans flex flex-col">
      <div className="flex items-center justify-between px-5 py-3.5 border-b border-[#181C27]">
        <div className="flex items-center gap-3">
          <div className="w-7 h-7 rounded bg-[#5EEBD1]/10 border border-[#5EEBD1]/30 flex items-center justify-center">
            <Activity size={14} className="text-[#5EEBD1]" />
          </div>
          <div>
            <h1 className="font-mono text-sm tracking-wide">AGENT-I</h1>
            <p className="text-[10px] text-[#7B8496] font-mono">
              {snapshot?.agent_id || "—"} · {now.toLocaleTimeString()}
            </p>
          </div>
        </div>
        <div className="flex items-center gap-3">
          <div className="flex items-center gap-2 text-[11px] font-mono">
            <span className="w-2 h-2 rounded-full" style={{ background: connected ? "#4ADE80" : "#F87171" }} />
            <span style={{ color: connected ? "#7B8496" : "#F87171" }}>
              {loading ? "connecting…" : connected ? "live" : `agent unreachable — ${error}`}
            </span>
          </div>
          <button onClick={() => setPaused(!paused)}
            className="flex items-center gap-1.5 text-[11px] font-mono px-2.5 py-1.5 rounded bg-[#12161F] border border-[#232838] text-[#7B8496]">
            {paused ? <Play size={12} /> : <Pause size={12} />}{paused ? "Resume" : "Pause"}
          </button>
        </div>
      </div>

      <div className="flex flex-1 min-h-0">
        <Sidebar view={view} setView={setView} snap={snapshot} />

        <div className="flex-1 min-w-0 p-5 overflow-y-auto">
          <div className="flex items-center gap-2 text-[11px] text-[#3A4154] font-mono mb-3">
            <span>agent-i</span><ChevronRight size={11} /><span className="text-[#7B8496]">{activeLabel}</span>
          </div>

          {view === "overview" && <OverviewView snap={snapshot} d={d} selected={selected} setSelected={setSelected} />}
          {view === "topology" && <TopologyView d={d} selected={selected} setSelected={setSelected} />}
          {view === "logs" && <LogsView logs={d.logs} />}
          {view === "metrics" && <MetricsView snap={snapshot} d={d} />}
          {view === "traces" && <TracesView traces={d.traces} />}
          {view === "infra" && <InfrastructureView d={d} />}

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

          <div className="flex items-center gap-2 text-[10px] text-[#3A4154] font-mono mt-5">
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
