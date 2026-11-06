#!/usr/bin/env python3
"""Deterministic test data for the victoria_logs_s3_backup end-to-end experiment.

  vldata.py ingest --addr URL --days D1,D2 --per-day N --expected FILE
      Generates N logs per UTC day (deterministic: the random seed is the day),
      ingests them via /insert/jsonline and stores the ground truth in FILE.

  vldata.py verify --addr URL --day D --expected FILE [--expect-empty]
      Exports every log of the day from VictoriaLogs, recomputes the same
      statistics and the content digest, runs a few server-side LogsQL queries
      and compares everything with the ground truth. Exit code 1 on mismatch.

The content digest is sha256 over the sorted canonical lines
"<_time ns>\\t<service>\\t<host>\\t<level>\\t<trace_id>\\t<seq>\\t<latency_ms>\\t<_msg>",
so two instances with the same digest hold exactly the same logs.
"""

import argparse
import calendar
import datetime as dt
import hashlib
import json
import os
import random
import sys
import time
import urllib.parse
import urllib.request

SERVICES = ["api-gateway", "payment", "order", "user", "inventory", "search"]
HOSTS = ["host-%02d" % i for i in range(1, 9)]
LEVELS = ["info"] * 70 + ["warn"] * 15 + ["error"] * 10 + ["debug"] * 5
METHODS = ["GET", "GET", "GET", "POST", "PUT", "DELETE"]
PATHS = ["/api/v1/orders", "/api/v1/users", "/api/v1/pay", "/api/v1/items", "/api/v1/search", "/healthz"]
ERRORS = ["upstream timeout", "connection reset by peer", "deadlock detected", "invalid token", "quota exceeded"]
FIELDS = ["_time", "service", "host", "level", "trace_id", "seq", "latency_ms", "_msg"]


def day_bounds(day):
    start = dt.datetime.strptime(day, "%Y%m%d").replace(tzinfo=dt.timezone.utc)
    return start, start + dt.timedelta(days=1)


def to_ns(t):
    return calendar.timegm(t.utctimetuple()) * 10**9


def format_ns(ns):
    sec, frac = divmod(ns, 10**9)
    return time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(sec)) + ".%09dZ" % frac


def parse_vl_time(s):
    # VictoriaLogs returns RFC3339 with an optional, possibly shortened fractional part.
    base, _, frac = s.rstrip("Z").partition(".")
    t = dt.datetime.strptime(base, "%Y-%m-%dT%H:%M:%S").replace(tzinfo=dt.timezone.utc)
    return to_ns(t) + int((frac + "000000000")[:9])


def canonical(rec):
    return "\t".join(str(rec[f]) for f in FIELDS)


class Stats:
    def __init__(self):
        self.count = 0
        self.by_level = {}
        self.sum_latency = 0
        self.trace_ids = set()
        self.payment_errors = 0
        self.lines = []

    def add(self, rec):
        self.count += 1
        self.by_level[rec["level"]] = self.by_level.get(rec["level"], 0) + 1
        self.sum_latency += int(rec["latency_ms"])
        self.trace_ids.add(rec["trace_id"])
        if rec["service"] == "payment" and rec["level"] == "error":
            self.payment_errors += 1
        self.lines.append(canonical(rec))

    def summary(self):
        self.lines.sort()
        h = hashlib.sha256()
        for line in self.lines:
            h.update(line.encode())
            h.update(b"\n")
        return {
            "count": self.count,
            "by_level": dict(sorted(self.by_level.items())),
            "sum_latency_ms": self.sum_latency,
            "uniq_trace_id": len(self.trace_ids),
            "payment_errors": self.payment_errors,
            "sha256": h.hexdigest(),
        }


def generate(day, n, now_ns):
    rng = random.Random(int(day))
    start, end = day_bounds(day)
    lo = to_ns(start)
    hi = min(to_ns(end), now_ns - 120 * 10**9)  # never write logs from the future
    if hi <= lo:
        raise SystemExit("day %s has not started yet" % day)
    offsets = sorted(rng.randrange(hi - lo) for _ in range(n))
    for seq, off in enumerate(offsets):
        level = rng.choice(LEVELS)
        status = 500 if level == "error" else rng.choice([200, 200, 200, 201, 204, 400, 404])
        latency = rng.randint(1, 2000)
        user = "u%05d" % rng.randrange(20000)
        msg = "%s %s status=%d latency_ms=%d user=%s" % (rng.choice(METHODS), rng.choice(PATHS), status, latency, user)
        if level == "error":
            msg += " error=\"%s\"" % rng.choice(ERRORS)
        yield {
            "_time": lo + off,
            "service": rng.choice(SERVICES),
            "host": rng.choice(HOSTS),
            "level": level,
            "trace_id": "%032x" % rng.getrandbits(128),
            "seq": seq,
            "latency_ms": latency,
            "user_id": user,
            "_msg": msg,
        }


def post(url, body, content_type):
    req = urllib.request.Request(url, data=body, method="POST", headers={"Content-Type": content_type})
    with urllib.request.urlopen(req, timeout=600) as resp:
        return resp.read()


def ingest(args):
    url = args.addr.rstrip("/") + "/insert/jsonline?" + urllib.parse.urlencode(
        {"_stream_fields": "service,host", "_time_field": "_time", "_msg_field": "_msg"})
    expected = {}
    if os.path.exists(args.expected):
        with open(args.expected) as f:
            expected = json.load(f)
    now_ns = time.time_ns()
    for day in args.days.split(","):
        st = Stats()
        batch = []
        t0 = time.time()
        for rec in generate(day, args.per_day, now_ns):
            st.add(rec)
            out = dict(rec, _time=format_ns(rec["_time"]))
            batch.append(json.dumps(out, separators=(",", ":")))
            if len(batch) >= 20000:
                post(url, ("\n".join(batch) + "\n").encode(), "application/stream+json")
                batch = []
        if batch:
            post(url, ("\n".join(batch) + "\n").encode(), "application/stream+json")
        expected[day] = st.summary()
        print(json.dumps({"day": day, "ingested": st.count, "seconds": round(time.time() - t0, 2),
                          "sha256": expected[day]["sha256"]}), flush=True)
    with open(args.expected, "w") as f:
        json.dump(expected, f, indent=2, sort_keys=True)


def query_iter(addr, q):
    """Streams the JSON lines returned by /select/logsql/query."""
    body = urllib.parse.urlencode({"query": q}).encode()
    req = urllib.request.Request(addr.rstrip("/") + "/select/logsql/query", data=body, method="POST",
                                 headers={"Content-Type": "application/x-www-form-urlencoded"})
    with urllib.request.urlopen(req, timeout=600) as resp:
        for line in resp:
            if line.strip():
                yield json.loads(line)


def query(addr, q):
    return list(query_iter(addr, q))


def num(v):
    """Converts a stats result to int; empty results (e.g. NaN from sum() over no rows) become 0."""
    try:
        f = float(v)
    except (TypeError, ValueError):
        return 0
    return 0 if f != f else int(f)


def verify(args):
    start, end = day_bounds(args.day)
    tf = "_time:[%s, %s)" % (start.strftime("%Y-%m-%dT%H:%M:%SZ"), end.strftime("%Y-%m-%dT%H:%M:%SZ"))

    st = Stats()
    for rec in query_iter(args.addr, tf + " | fields " + ", ".join(FIELDS)):
        rec["_time"] = parse_vl_time(rec["_time"])
        st.add(rec)
    got = st.summary()

    # Server-side queries exercise the restored indexdb (stream filters) and column data.
    agg = query(args.addr, tf + " | stats count() as n, sum(latency_ms) as lat, count_uniq(trace_id) as u")
    agg = agg[0] if agg else {}
    pay = query(args.addr, tf + ' {service="payment"} level:error | stats count() as n')
    server = {
        "count": num(agg.get("n")),
        "sum_latency_ms": num(agg.get("lat")),
        "uniq_trace_id": num(agg.get("u")),
        "payment_errors": num(pay[0]["n"]) if pay else 0,
    }

    if args.expect_empty:
        ok = got["count"] == 0 and server["count"] == 0
        result = {"addr": args.addr, "day": args.day, "expect_empty": True, "count": got["count"],
                  "server_count": server["count"], "ok": ok}
    else:
        with open(args.expected) as f:
            want = json.load(f)[args.day]
        checks = {k: got[k] == want[k] for k in want}
        checks.update({"server_" + k: server[k] == want[k] for k in server})
        ok = all(checks.values())
        result = {"addr": args.addr, "day": args.day, "ok": ok, "checks": checks, "got": got, "server": server,
                  "expected_sha256": want["sha256"]}
    print(json.dumps(result, indent=2))
    return 0 if ok else 1


def main():
    p = argparse.ArgumentParser()
    sub = p.add_subparsers(dest="cmd", required=True)
    pi = sub.add_parser("ingest")
    pi.add_argument("--addr", required=True)
    pi.add_argument("--days", required=True)
    pi.add_argument("--per-day", type=int, default=300000)
    pi.add_argument("--expected", required=True)
    pv = sub.add_parser("verify")
    pv.add_argument("--addr", required=True)
    pv.add_argument("--day", required=True)
    pv.add_argument("--expected", required=True)
    pv.add_argument("--expect-empty", action="store_true")
    args = p.parse_args()
    if args.cmd == "ingest":
        ingest(args)
        return 0
    return verify(args)


if __name__ == "__main__":
    sys.exit(main())
