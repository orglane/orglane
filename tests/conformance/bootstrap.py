"""Creates the first owner of the Orglane under test (first-run setup).

Usage: python3 bootstrap.py <setup-token> <email> <password>
"""

import sys

import httpx

token, email, password = sys.argv[1:4]
res = httpx.post(
    "https://op.orglane.test/api/v1/auth/setup",
    json={
        "setupToken": token,
        "workEmail": email,
        "username": "conformance",
        "displayName": "Conformance Tester",
        "password": password,
    },
    verify="/work/certs/ca.crt",
    timeout=30,
)
if res.status_code != 201:
    sys.exit(f"setup failed: {res.status_code} {res.text}")
print(f"created owner {email}")
