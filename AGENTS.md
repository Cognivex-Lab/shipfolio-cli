# Working on shipfolio-cli

This is a public repository. It holds only the client: never add server code, internal hostnames, keys or customer data.

- **Standard library only.** No third-party modules.
- **The API is the contract.** Endpoints and fields come from <https://api.shipfolio.ai/openapi.json> (source: the private `shipfolio-api` repo). When the API changes, update `internal/client` first.
- **Unknown is never zero.** Metric values are `*float64`. Tables print `—` for nil, CSV prints an empty cell, `--json` passes `null` through.
- **`--json` is stable; tables are not.** `--json` prints the response body unchanged. Scripts and docs that parse output should use it.
- **Keys stay out of history and logs.** `login` reads the key from a prompt or stdin, never an argument. The config file is written `0600`. Error messages never include the key.
- **Deleting asks first.** Commands that delete data confirm on a terminal and require `--yes` otherwise.
- **Exit codes:** 0 success, 1 the request failed, 2 the command line was wrong.
- Tests run against an `httptest` fake API: `go test ./...`.
