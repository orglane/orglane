"""Exports the HTML report of every test plan on the conformance suite.

The suite's run-test-plan.py exports each plan as JSON (with signatures)
only. This adds the human-readable HTML export of the same plans next to it.
The suite's database is fresh for every `make conformance`, so every plan it
knows belongs to this run.

Usage (in the runner container, working directory /suite):
    python3 /conformance/export_html.py <output-dir>
"""

import asyncio
import os
import sys

sys.path.insert(0, "/suite/scripts")
from conformance import Conformance  # noqa: E402  (the suite's API wrapper)


async def main(out):
    os.makedirs(out, exist_ok=True)
    api = Conformance(os.environ["CONFORMANCE_SERVER"], None, verify_ssl=False)
    res = await api.httpclient.get(api.api_url_base + "api/plan?length=1000")
    res.raise_for_status()
    plans = res.json()["data"]
    if not plans:
        sys.exit("no test plans to export")
    for plan in plans:
        path = await api.exporthtml(plan["_id"], out)
        print(f"exported {plan.get('planName', plan['_id'])} to {os.path.basename(path)}")


asyncio.run(main(sys.argv[1]))
