"""Disposable local OCS/WebDAV checks; prints statuses, never credentials."""

import argparse
import base64
import hashlib
import json
import urllib.error
import urllib.parse
import urllib.request
import xml.etree.ElementTree as ET

parser = argparse.ArgumentParser()
parser.add_argument("--base", default="http://127.0.0.1:18103")
args = parser.parse_args()
results = {}
person = "research-person"
password = "research-pass-1"


def request(method, path, user="admin", fields=None, body=None):
    headers = {
        "Authorization": "Basic " + base64.b64encode(f"{user}:{password}".encode()).decode(),
        "OCS-APIRequest": "true",
        "Accept": "application/json",
    }
    if fields is not None:
        body = urllib.parse.urlencode(fields).encode()
        headers["Content-Type"] = "application/x-www-form-urlencoded"
    req = urllib.request.Request(args.base + path, data=body, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=20) as response:
            return response.status, response.read()
    except urllib.error.HTTPError as error:
        return error.code, error.read()


def ocs(label, method, path, user="admin", fields=None):
    status, body = request(method, path + "?format=json", user, fields)
    data = json.loads(body)["ocs"]
    results[label] = {"http": status, "ocs_status": data["meta"]["statuscode"]}
    if data["meta"]["status"] != "ok" or not 200 <= status < 300:
        raise RuntimeError(f"{label} failed: {results[label]}")
    return data["data"]


cloud = "/ocs/v2.php/cloud"
ocs("create_user", "POST", cloud + "/users", fields={"userid": person, "password": password})
ocs("create_group", "POST", cloud + "/groups", fields={"groupid": "research-team"})
ocs("add_membership", "POST", cloud + f"/users/{person}/groups", fields={"groupid": "research-team"})
groups = ocs("read_membership", "GET", cloud + f"/users/{person}/groups")
assert "research-team" in groups["groups"]

payload = (b"Orglane research fixture\n" * 50000)[:1048576]
file_path = f"/remote.php/dav/files/{person}/research.bin"
status, _ = request("PUT", file_path, person, body=payload)
results["file_put"] = status
assert status == 201
status, data = request("GET", file_path, person)
results["file_get"] = {"http": status, "bytes": len(data), "sha256": hashlib.sha256(data).hexdigest()}
assert status == 200 and data == payload

share = ocs(
    "share_file", "POST", "/ocs/v2.php/apps/files_sharing/api/v1/shares", person,
    {"path": "/research.bin", "shareType": 0, "shareWith": "admin", "permissions": 1},
)
assert share["share_with"] == "admin"
ocs("read_share", "GET", f'/ocs/v2.php/apps/files_sharing/api/v1/shares/{share["id"]}', person)

try:
    ocs("disable_user", "PUT", cloud + f"/users/{person}/disable")
    status, disabled_body = request("GET", file_path, person)
    error_class = None
    try:
        root = ET.fromstring(disabled_body)
        error_class = root.findtext("{http://sabredav.org/ns}exception")
    except ET.ParseError:
        pass
    results["basic_file_get_while_disabled"] = {"http": status, "dav_error_class": error_class}
    # Nextcloud's DAV auth adapter can map a login exception to HTTP 503.
    # A status-only observation does not prove why access failed or revoke tokens.
    assert status in (401, 403, 503) and disabled_body != payload
    results["suspension_semantics_unverified"] = status == 503
finally:
    ocs("enable_user", "PUT", cloud + f"/users/{person}/enable")
results["basic_file_get_after_enable"], _ = request("GET", file_path, person)
assert results["basic_file_get_after_enable"] == 200
print(json.dumps(results, sort_keys=True))
