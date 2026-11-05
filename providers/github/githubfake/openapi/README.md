# GitHub API descriptions for the fake GitHub

The fake GitHub (`providers/github/githubfake`) validates every request it
receives and every response it sends against these descriptions, so it
cannot drift from the real API.

- `api.github.com.subset.json`: a subset of GitHub's official REST API
  description, [github/rest-api-description](https://github.com/github/rest-api-description),
  file `descriptions/api.github.com/api.github.com.2022-11-28.json` (the API
  version the adapter sends) at commit
  `78edb1e2af8b511d88a0be137b69622a13a2b137`. It keeps the operations listed
  in `extract.go` and every component they reference, unchanged. Licensed
  under the MIT License, see `LICENSE.md` (copied from that repository).
- `oauth.json`: the user authorization (web application) flow on
  `github.com`, which the REST description does not cover. Hand-written by
  Orglane from GitHub's documentation (links inside the file).

Responses with a status an operation does not document are accepted only for
the errors GitHub documents for every endpoint (401, 403 and 429 rate limits,
5xx), and only with GitHub's `basic-error` body.

To add an operation or move the pin: download the description at the new
commit, add the operation to `extract.go`, run
`go run extract.go <description.json> > api.github.com.subset.json`, update
the commit here, and run the fake's tests.
