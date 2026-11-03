"""Writes the conformance suite configuration for the Orglane under test.

The clients are registered with `orglane cli oidc-client add`; their IDs and
secrets come from the environment. The browser section drives Orglane's
fallback sign-in form (/oidc/login) the way a person would.
"""

import json
import os

OP = "https://op.orglane.test"
EMAIL = os.environ["OWNER_EMAIL"]
PASSWORD = os.environ["OWNER_PASSWORD"]

login = {
    "task": "Login",
    "optional": True,
    "match": OP + "/oidc/login*",
    "commands": [
        ["text", "id", "email", EMAIL],
        ["text", "id", "password", PASSWORD],
        ["click", "id", "submit"],
    ],
}
verify = {
    "task": "Verify Complete",
    "match": "*/test/*/callback*",
    "commands": [["wait", "id", "submission_complete", 10]],
}


def browser(login_task):
    return [{"match": OP + "/oidc/authorize*", "tasks": [login_task, verify]}]


# The second sign-in of prompt=login and max_age tests: the suite keeps a
# screenshot of the login page as evidence that the person was asked again.
relogin = dict(login)
relogin["commands"] = [["wait", "xpath", "//*", 10, "Sign in", "update-image-placeholder-optional"]] + login["commands"]

# Requests with a redirect URI the client did not register must end on an
# error page at the OP, never at the redirect URI.
error_page = [{
    "match": OP + "/oidc/authorize*",
    "tasks": [{
        "task": "Expect the invalid redirect URI error page",
        "match": OP + "/oidc/authorize*",
        "commands": [["wait", "xpath", "//*", 10, "invalid sign-in request", "update-image-placeholder"]],
    }],
}]

config = {
    "alias": "orglane",
    "description": "Orglane embedded OpenID Provider",
    "server": {"discoveryUrl": OP + "/.well-known/openid-configuration"},
    "client": {"client_id": os.environ["CLIENT1_ID"], "client_secret": os.environ["CLIENT1_SECRET"]},
    "client2": {"client_id": os.environ["CLIENT2_ID"], "client_secret": os.environ["CLIENT2_SECRET"]},
    "client_secret_post": {"client_id": os.environ["CLIENT3_ID"], "client_secret": os.environ["CLIENT3_SECRET"]},
    "browser": browser(login),
    "override": {
        "oidcc-prompt-login": {"browser": browser(relogin)},
        "oidcc-max-age-1": {"browser": browser(relogin)},
        "oidcc-ensure-registered-redirect-uri": {"browser": error_page},
        "oidcc-ensure-redirect-uri-in-authorization-request": {"browser": error_page},
        "oidcc-ensure-request-object-with-redirect-uri": {"browser": error_page},
    },
}
with open("/work/config.json", "w") as f:
    json.dump(config, f, indent=2)
print("wrote /work/config.json")
