# GitHub webhook payload fixtures

Recorded GitHub.com webhook payloads, copied unchanged from the payload
examples of [octokit/webhooks](https://github.com/octokit/webhooks)
(`payload-examples/api.github.com/<event>/<action>.payload.json`, commit
`aa80c24a3f55354465b8b1438d33dd5602956d0d`), MIT licensed: see
[LICENSE.octokit-webhooks](LICENSE.octokit-webhooks). File names are
`<event>.<action>[.<variant>].json`; the event is the `X-GitHub-Event` header.

They contain no secrets. Tests sign them with a test-only fixture secret
(HMAC is deterministic, so the signatures are reproducible) and point the test
instance at the payloads' installation (`1`) and organization (`38302899`,
`Octocoders`). A few examples were recorded from repository webhooks and carry
no `installation` (`issues.opened.with-organization`,
`membership.removed.with-deleted-team`); tests add `installation.id`, as GitHub
does for App deliveries, before signing. Tests that need mentions replace a
body field before signing.

Payloads from the dedicated test organization are not committed: the live
check (`TestGitHubLiveWebhookDeliveries`) reads them from the App's delivery
log instead (docs/specs/providers.md "Webhooks in live tests").
