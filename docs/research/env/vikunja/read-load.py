"""Bounded authenticated reads; no credentials or task contents on stdout."""

import argparse
import collections
import concurrent.futures
import json
import pathlib
import time
import urllib.error
import urllib.request

parser = argparse.ArgumentParser()
parser.add_argument("--token-file", required=True)
parser.add_argument("--url", default="http://127.0.0.1:18101/api/v1/tasks?page=1&per_page=50")
parser.add_argument("--workers", type=int, default=2)
parser.add_argument("--requests-per-worker", type=int, default=40)
parser.add_argument("--interval", type=float, default=0.5)
args = parser.parse_args()
if not 1 <= args.workers <= 2 or not 1 <= args.requests_per_worker <= 40 or args.interval < 0.5:
    parser.error("research bound: 1-2 workers, 1-40 requests each, interval >= 0.5 seconds")
token = pathlib.Path(args.token_file).read_text().strip()


def worker():
    statuses = collections.Counter()
    lengths = collections.Counter()
    for _ in range(args.requests_per_worker):
        request = urllib.request.Request(args.url, headers={"Authorization": "Bearer " + token})
        try:
            with urllib.request.urlopen(request, timeout=10) as response:
                data = json.load(response)
                statuses[str(response.status)] += 1
                lengths[str(len(data)) if isinstance(data, list) else "not-list"] += 1
        except urllib.error.HTTPError as error:
            statuses[str(error.code)] += 1
        except (OSError, ValueError):
            statuses["transport-or-json-error"] += 1
        time.sleep(args.interval)
    return statuses, lengths


started = time.monotonic()
statuses = collections.Counter()
lengths = collections.Counter()
with concurrent.futures.ThreadPoolExecutor(max_workers=args.workers) as pool:
    for worker_statuses, worker_lengths in pool.map(lambda _: worker(), range(args.workers)):
        statuses.update(worker_statuses)
        lengths.update(worker_lengths)
elapsed = time.monotonic() - started
print(json.dumps({"elapsed_seconds": round(elapsed, 3), "statuses": dict(statuses),
                  "array_lengths": dict(lengths)}, sort_keys=True))
if statuses != {"200": args.workers * args.requests_per_worker} or "not-list" in lengths:
    raise SystemExit(1)
