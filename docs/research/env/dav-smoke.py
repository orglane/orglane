"""Seed disposable DAV data; print HTTP status counts, never credentials."""

import argparse
import base64
import collections
import urllib.request
import urllib.error

parser = argparse.ArgumentParser()
parser.add_argument("base")
parser.add_argument("--user", default="alice")
parser.add_argument("--password", default="research")
parser.add_argument("--nextcloud", action="store_true")
args = parser.parse_args()
auth = base64.b64encode(f"{args.user}:{args.password}".encode()).decode()
counts = collections.Counter()


def request(method, path, body=None, content_type="application/xml", allow_existing=False):
    req = urllib.request.Request(
        args.base + path,
        data=body.encode() if body is not None else None,
        method=method,
        headers={"Authorization": "Basic " + auth, "Content-Type": content_type},
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as response:
            counts[(method, response.status)] += 1
            return response.read()
    except urllib.error.HTTPError as error:
        if allow_existing and error.code == 405:
            counts[(method, error.code)] += 1
            return b""
        raise


if args.nextcloud:
    calendar = f"/remote.php/dav/calendars/{args.user}/research/"
    contacts = f"/remote.php/dav/addressbooks/users/{args.user}/research/"
else:
    # Radicale may create the principal collection upon authentication.
    request("MKCOL", f"/{args.user}/", allow_existing=True)
    calendar = f"/{args.user}/calendar/"
    contacts = f"/{args.user}/contacts/"

request("MKCALENDAR", calendar, '<C:mkcalendar xmlns:C="urn:ietf:params:xml:ns:caldav"/>')
request(
    "MKCOL",
    contacts,
    '<D:mkcol xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">'
    "<D:set><D:prop><D:resourcetype><D:collection/><C:addressbook/>"
    "</D:resourcetype></D:prop></D:set></D:mkcol>",
)
for index in range(20):
    event = "\r\n".join(
        [
            "BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//Orglane//Research//EN",
            "BEGIN:VEVENT", f"UID:research-{index}@example.com",
            "DTSTAMP:20261103T120000Z", "DTSTART:20261104T120000Z",
            "DTEND:20261104T130000Z", f"SUMMARY:Research event {index}",
            "END:VEVENT", "END:VCALENDAR", "",
        ]
    )
    card = "\r\n".join(
        ["BEGIN:VCARD", "VERSION:3.0", f"UID:research-{index}",
         f"FN:Example {index}", "END:VCARD", ""]
    )
    request("PUT", calendar + f"{index}.ics", event, "text/calendar")
    request("PUT", contacts + f"{index}.vcf", card, "text/vcard")
assert b"BEGIN:VEVENT" in request("GET", calendar + "0.ics")
assert b"BEGIN:VCARD" in request("GET", contacts + "0.vcf")
for (method, status), count in sorted(counts.items()):
    print(f"{method} HTTP {status}: {count}")
