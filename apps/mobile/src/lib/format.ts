import type { Session } from "./api";

const SOURCES: Record<string, string> = {
  api_server: "app",
  telegram: "Telegram",
  cli: "CLI",
  tui: "TUI",
  cron: "cron",
  discord: "Discord",
  slack: "Slack",
};

export const sourceLabel = (source: string) => SOURCES[source] ?? source.replace(/_/g, " ");

/** Only sessions the app created can take prompts (the relay refuses the others with read_only). */
export const isWritable = (s: Pick<Session, "source">) => s.source === "api_server";

export function sessionTitle(s: Pick<Session, "title" | "preview">): string {
  const t = s.title?.trim();
  if (t) return t;
  const words = s.preview?.trim().split(/\s+/).slice(0, 8).join(" ");
  return words || "New chat";
}

export function relativeTime(unixSeconds: number | null, nowMs = Date.now()): string {
  if (unixSeconds === null) return "";
  const s = Math.max(0, Math.round(nowMs / 1000 - unixSeconds));
  if (s < 60) return "just now";
  if (s < 3600) return `${Math.floor(s / 60)} min ago`;
  if (s < 86400) return `${Math.floor(s / 3600)} h ago`;
  if (s < 7 * 86400) return `${Math.floor(s / 86400)} d ago`;
  return new Date(unixSeconds * 1000).toLocaleDateString();
}

export const formatDuration = (sec: number | undefined) =>
  sec === undefined ? "" : sec < 10 ? `${sec.toFixed(1)} s` : sec < 90 ? `${Math.round(sec)} s` : `${Math.round(sec / 60)} min`;
