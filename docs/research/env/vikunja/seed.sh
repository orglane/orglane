#!/bin/sh
# Seeds the local Vikunja: one user, one project, 500 tasks. Prints the
# user's JWT on stdout (for the load test). Local research data only.
set -eu
base=${BASE:-http://127.0.0.1:18101/api/v1}
json='content-type: application/json'
curl -sf -o /dev/null -X POST "$base/register" -H "$json" \
	-d '{"username":"alice","email":"alice@example.com","password":"research-pass-1"}'
token=$(curl -sf -X POST "$base/login" -H "$json" \
	-d '{"username":"alice","password":"research-pass-1"}' | jq -r .token)
auth="authorization: Bearer $token"
project=$(curl -sf -X PUT "$base/projects" -H "$auth" -H "$json" -d '{"title":"Load"}' | jq .id)
i=1
while [ "$i" -le 500 ]; do
	curl -sf -o /dev/null -X PUT "$base/projects/$project/tasks" -H "$auth" -H "$json" \
		-d "{\"title\":\"Task $i\",\"description\":\"Seeded task number $i for the load test\"}"
	i=$((i + 1))
done
echo "$token"
