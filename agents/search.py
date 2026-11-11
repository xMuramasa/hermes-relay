# Searches every Hermes conversation's messages with Hermes' own index (run by trismegistus-agents with Hermes'
# interpreter after hermes_bootstrap). argv: query, limit. Prints {"results": [...], "more": bool}, newest first.
# The index also covers tool calls and recalled memory, so each hit is re-checked against the message's own text.
import json
import os
import re
import sqlite3
import sys
from pathlib import Path

from agent.context_compressor import user_originated_turn_view
from hermes_state import SessionDB

BEFORE, AFTER = 60, 120  # snippet characters around the first match


def terms(q):
    """The query's words (\\w+; a trailing * is a prefix, which a substring match covers) and quoted phrases. FTS5's
    operators AND, OR and NOT are syntax, not words."""
    phrases = [" ".join(p.split()) for p in re.findall(r'"([^"]*)"', q)]
    words = [w for w in re.findall(r"\w+", re.sub(r'"[^"]*"', " ", q)) if w not in ("AND", "OR", "NOT")]
    return [t for t in phrases + words if t]


def spans(text, ts):
    """Every term's matches in text, merged and sorted; None unless every term occurs."""
    found = []
    for t in ts:
        m = [(x.start(), x.end()) for x in re.finditer(re.escape(t), text, re.IGNORECASE)]
        if not m:
            return None
        found += m
    merged = []
    for s, e in sorted(found):
        if merged and s <= merged[-1][1]:
            merged[-1][1] = max(merged[-1][1], e)
        else:
            merged.append([s, e])
    return merged


def u16(s):
    return len(s.encode("utf-16-le")) // 2


def snippet(text, sp):
    """About BEFORE characters before the first match and AFTER after it, cut at spaces, with its marks in UTF-16."""
    first, first_end = sp[0]
    s, e = max(0, first - BEFORE), min(len(text), first_end + AFTER)
    if s > 0 and (i := text.find(" ", s, first)) != -1:
        s = i + 1
    if e < len(text) and (i := text.rfind(" ", first_end, e)) > first_end:
        e = i
    lead = "…" if s > 0 else ""
    out = lead + text[s:e] + ("…" if e < len(text) else "")
    marks = [[u16(lead + text[s:a]), u16(text[a:b])] for a, b in sp if a >= s and b <= e]
    return out, marks


def main():
    q, limit = sys.argv[1], int(sys.argv[2])
    ts = terms(q)
    db = SessionDB(read_only=True)
    rows = db.search_messages(q, role_filter=["user", "assistant"], limit=300, sort="newest")
    if not rows or not ts:
        print(json.dumps({"results": [], "more": False}))
        return
    path = getattr(db, "db_path", None) or os.path.join(os.environ["HERMES_HOME"], "state.db")
    con = sqlite3.connect(Path(path).resolve().as_uri() + "?mode=ro", uri=True)
    ids = [r["id"] for r in rows]
    content = dict(con.execute(f"select id, content from messages where id in ({','.join('?' * len(ids))})", ids))
    sids = sorted({r["session_id"] for r in rows})
    sessions = {
        sid: (title, source)
        for sid, title, source in con.execute(
            f"select id, title, source from sessions where id in ({','.join('?' * len(sids))})", sids
        )
    }
    con.close()

    results = []
    for r in rows:
        text = content.get(r["id"])
        if not isinstance(text, str):
            continue
        if r["role"] == "user":
            text = text.split("<memory-context>", 1)[0]
            if user_originated_turn_view({"role": "user", "content": text}) is None:
                continue  # a compaction summary, not a turn
        text = " ".join(text.split())
        sp = spans(text, ts)
        if sp is None:
            continue
        snip, marks = snippet(text, sp)
        title, source = sessions.get(r["session_id"], (None, r.get("source")))
        results.append({
            "session_id": r["session_id"],
            "message_id": int(r["id"]),
            "role": r["role"],
            "timestamp": float(r.get("timestamp") or 0),
            "snippet": snip,
            "marks": marks,
            "session_title": title,
            "session_source": source,
        })
        if len(results) > limit:
            break
    print(json.dumps({"results": results[:limit], "more": len(results) > limit}))


main()
