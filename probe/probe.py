#!/usr/bin/env python3
"""Phase 0: record real Hermes API event streams for the relay's test fixtures.

Runs on optiplex against the local API server (127.0.0.1:8642). Reads API_SERVER_KEY from ~/.hermes/.env and never
prints it. Each scenario starts one run in its own throwaway API session and records every SSE event, plus the run
status, to probe/raw/<scenario>.jsonl (gitignored). `sanitize.py` turns those into committed fixtures.

    python3 -I probe/probe.py list-sessions
    python3 -I probe/probe.py run plain
    python3 -I probe/probe.py run tool
    python3 -I probe/probe.py run approval [--choice deny]
    python3 -I probe/probe.py run stop
    python3 -I probe/probe.py run subagent
    python3 -I probe/probe.py messages <session_id>
"""
import argparse
import json
import os
import sys
import threading
import time
import urllib.error
import urllib.request
import uuid

BASE = os.environ.get("HERMES_URL", "http://127.0.0.1:8642")
RAW = os.path.join(os.path.dirname(os.path.abspath(__file__)), "raw")

SCENARIOS = {
    "plain": "Reply with one short sentence: what is 2 + 2? Do not use any tools.",
    "tool": "Use the terminal tool to run `date +%Y` and tell me the year it prints. Keep the answer to one line.",
    # A command Hermes' approval system treats as dangerous, on a scratch path, so the run pauses for approval.
    "approval": (
        "Use the terminal tool to run exactly this command and nothing else: "
        "`rm -rf /home/muramasa/.hermes/cache/scratch/trismegistus-probe-target` "
        "(it is a scratch directory created for this test). Then say whether it ran."
    ),
    "stop": "Count slowly from 1 to 200, one number per line, with a short comment after each number.",
    "subagent": (
        "Use delegate_task to spawn exactly one subagent with the goal: 'Reply with the single word ok.' "
        "Then tell me what it returned, in one line."
    ),
}


def api_key() -> str:
    for line in open(os.path.expanduser("~/.hermes/.env"), encoding="utf-8"):
        if line.startswith("API_SERVER_KEY="):
            return line.split("=", 1)[1].strip().strip('"').strip("'")
    sys.exit("API_SERVER_KEY not found in ~/.hermes/.env")


KEY = api_key()


def req(method: str, path: str, body=None, headers=None, timeout=30):
    h = {"Authorization": f"Bearer {KEY}", "Content-Type": "application/json", **(headers or {})}
    r = urllib.request.Request(BASE + path, method=method, headers=h,
                               data=None if body is None else json.dumps(body).encode())
    try:
        with urllib.request.urlopen(r, timeout=timeout) as resp:
            raw = resp.read().decode()
            return resp.status, (json.loads(raw) if raw else None), dict(resp.headers)
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        try:
            return e.code, json.loads(raw), dict(e.headers)
        except ValueError:
            return e.code, raw[:500], dict(e.headers)


def sse(path: str, on_event, stop_flag: threading.Event, timeout=600):
    """Read an SSE stream, calling on_event(event_name, data_dict_or_str, raw_lines) for each event."""
    r = urllib.request.Request(BASE + path, headers={"Authorization": f"Bearer {KEY}", "Accept": "text/event-stream"})
    with urllib.request.urlopen(r, timeout=timeout) as resp:
        name, data, raw = None, [], []
        for line in resp:
            if stop_flag.is_set():
                return
            line = line.decode("utf-8").rstrip("\n").rstrip("\r")
            raw.append(line)
            if line == "":
                if data or name:
                    text = "\n".join(data)
                    try:
                        parsed = json.loads(text)
                    except ValueError:
                        parsed = text
                    if on_event(name, parsed, raw) is False:
                        return
                name, data, raw = None, [], []
            elif line.startswith(":"):
                on_event(":comment", line[1:].strip(), [line])
                raw = []
            elif line.startswith("event:"):
                name = line[6:].strip()
            elif line.startswith("data:"):
                data.append(line[5:].lstrip())


def run_scenario(name: str, choice: str):
    os.makedirs(RAW, exist_ok=True)
    out_path = os.path.join(RAW, f"{name}.jsonl")
    out = open(out_path, "w", encoding="utf-8")
    t0 = time.monotonic()

    def log(kind, payload):
        out.write(json.dumps({"t": round(time.monotonic() - t0, 3), "kind": kind, **payload}, ensure_ascii=False) + "\n")
        out.flush()

    if name == "approval":
        os.makedirs(os.path.expanduser("~/.hermes/cache/scratch/trismegistus-probe-target/x"), exist_ok=True)

    status, session, _ = req("POST", "/api/sessions", {"title": f"trismegistus probe: {name} {uuid.uuid4().hex[:6]}"})
    log("session.create", {"status": status, "body": session})
    if status not in (200, 201):
        sys.exit(f"session create failed: {status} {session}")
    sid = (session.get("session") or session).get("id") or session.get("session_id")
    log("session.id", {"session_id": sid})

    idem = f"probe-{name}-{uuid.uuid4().hex[:12]}"
    body = {"input": SCENARIOS[name], "session_id": sid}
    # Probe on a cheap model: the event format doesn't depend on which model answers.
    if os.environ.get("PROBE_MODEL", "claude-haiku-5-5"):
        body.update(model=os.environ.get("PROBE_MODEL", "claude-haiku-5-5"),
                    provider=os.environ.get("PROBE_PROVIDER", "claude-subscription-directsdk-experimental"))
    status, run, headers = req("POST", "/v1/runs", body, headers={"Idempotency-Key": idem})
    log("run.create", {"status": status, "body": run, "headers": {k: v for k, v in headers.items() if k.lower().startswith(("idempotency", "x-hermes"))}})
    if status not in (200, 201, 202):
        sys.exit(f"run create failed: {status} {run}")
    run_id = run["run_id"]

    # Retry with the same key: must return the same run, not start a second one.
    status, again, headers = req("POST", "/v1/runs", body, headers={"Idempotency-Key": idem})
    log("run.create.retry", {"status": status, "body": again, "headers": {k: v for k, v in headers.items() if k.lower().startswith(("idempotency", "x-hermes"))}})

    stop_flag = threading.Event()
    counts = {}
    acted = {"stop": False, "approval": False}

    def on_event(ev, data, raw):
        etype = ev if ev and ev != "message" else (data.get("type") or data.get("event") if isinstance(data, dict) else None)
        counts[str(etype)] = counts.get(str(etype), 0) + 1
        log("sse", {"event": ev, "data": data})
        pass
        if name == "approval" and not acted["approval"] and etype and "approval" in str(etype):
            acted["approval"] = True
            s, status_body, _ = req("GET", f"/v1/runs/{run_id}")
            log("run.status.pending", {"status": s, "body": status_body})
            pc = (status_body or {}).get("pending_controls") or []
            ctl = pc[0] if isinstance(pc, list) and pc else (pc if isinstance(pc, dict) else {})
            rid = ctl.get("prompt_id") or ctl.get("request_id") or (data.get("request_id") if isinstance(data, dict) else None)
            gen = ctl.get("execution_generation", (data.get("execution_generation") if isinstance(data, dict) else None))
            body = {"request_id": rid, "execution_generation": gen, "choice": choice}
            s, b, _ = req("POST", f"/v1/runs/{run_id}/approval", body)
            log("run.approval", {"status": s, "request": body, "body": b})
        if isinstance(etype, str) and etype in ("run.completed", "run.failed", "run.cancelled"):
            return False
        return True

    if name == "stop":
        def stopper():
            time.sleep(float(os.environ.get("PROBE_STOP_AFTER", "4")))
            s, b, _ = req("GET", f"/v1/runs/{run_id}")
            log("run.status.before_stop", {"status": s, "body": b})
            s, b, _ = req("POST", f"/v1/runs/{run_id}/stop", {})
            log("run.stop", {"status": s, "body": b})
        threading.Thread(target=stopper, daemon=True).start()
    try:
        sse(f"/v1/runs/{run_id}/events", on_event, stop_flag)
    except Exception as e:  # stream closed or timed out
        log("sse.error", {"error": f"{type(e).__name__}: {e}"[:300]})

    status, final, _ = req("GET", f"/v1/runs/{run_id}")
    log("run.status.final", {"status": status, "body": final})
    status, msgs, _ = req("GET", f"/api/sessions/{sid}/messages")
    log("session.messages", {"status": status, "body": msgs})
    out.close()
    print(json.dumps({"scenario": name, "session_id": sid, "run_id": run_id, "events": counts,
                      "final_status": (final or {}).get("status"), "seconds": round(time.monotonic() - t0, 1),
                      "file": out_path}, indent=1))


def chat_stream(text: str):
    """POST /api/sessions/{id}/chat/stream and record its SSE frames (does it stream text deltas?)."""
    os.makedirs(RAW, exist_ok=True)
    out = open(os.path.join(RAW, "chat_stream.jsonl"), "w", encoding="utf-8")
    t0 = time.monotonic()
    s, session, _ = req("POST", "/api/sessions", {"title": f"trismegistus probe: chat_stream {uuid.uuid4().hex[:6]}"})
    sid = session["session"]["id"]
    body = {"message": text, "model": os.environ.get("PROBE_MODEL", "claude-haiku-5-5"),
            "provider": os.environ.get("PROBE_PROVIDER", "claude-subscription-directsdk-experimental")}
    r = urllib.request.Request(BASE + f"/api/sessions/{sid}/chat/stream", method="POST", data=json.dumps(body).encode(),
                               headers={"Authorization": f"Bearer {KEY}", "Content-Type": "application/json",
                                        "Accept": "text/event-stream"})
    counts = {}
    try:
        with urllib.request.urlopen(r, timeout=300) as resp:
            name, data = None, []
            for line in resp:
                line = line.decode("utf-8").rstrip("\r\n")
                if line == "":
                    if name or data:
                        txt = "\n".join(data)
                        try:
                            d = json.loads(txt)
                        except ValueError:
                            d = txt
                        ev = name or (d.get("event") or d.get("type") if isinstance(d, dict) else None)
                        counts[str(ev)] = counts.get(str(ev), 0) + 1
                        out.write(json.dumps({"t": round(time.monotonic() - t0, 3), "event": name, "data": d}, ensure_ascii=False) + "\n")
                    name, data = None, []
                elif line.startswith(":"):
                    counts[":comment"] = counts.get(":comment", 0) + 1
                elif line.startswith("event:"):
                    name = line[6:].strip()
                elif line.startswith("data:"):
                    data.append(line[5:].lstrip())
    except urllib.error.HTTPError as e:
        print("HTTP", e.code, e.read().decode()[:400])
    out.close()
    print(json.dumps({"scenario": "chat_stream", "session_id": sid, "events": counts, "seconds": round(time.monotonic() - t0, 1)}))


def main():
    ap = argparse.ArgumentParser()
    sub = ap.add_subparsers(dest="cmd", required=True)
    sub.add_parser("list-sessions")
    r = sub.add_parser("run")
    r.add_argument("scenario", choices=sorted(SCENARIOS))
    r.add_argument("--choice", default="once")
    cs = sub.add_parser("chat-stream")
    cs.add_argument("--text", default="Write four short sentences about the ocean, one per line. No tools.")
    m = sub.add_parser("messages")
    m.add_argument("session_id")
    a = ap.parse_args()
    if a.cmd == "list-sessions":
        s, b, _ = req("GET", "/api/sessions?limit=50")
        os.makedirs(RAW, exist_ok=True)
        json.dump(b, open(os.path.join(RAW, "sessions.json"), "w"), indent=1, ensure_ascii=False)
        rows = (b.get("data") or b.get("sessions") or []) if isinstance(b, dict) else b
        print("status", s, "count", len(rows) if isinstance(rows, list) else "?")
        for x in (rows or [])[:50]:
            print(" ", x.get("id"), "|", x.get("source"), "|", (x.get("title") or "")[:50])
    elif a.cmd == "chat-stream":
        chat_stream(a.text)
    elif a.cmd == "run":
        run_scenario(a.scenario, a.choice)
    else:
        s, b, _ = req("GET", f"/api/sessions/{a.session_id}/messages")
        print("status", s)
        print(json.dumps(b, indent=1, ensure_ascii=False)[:3000])


if __name__ == "__main__":
    main()
