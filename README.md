# shipfolio

The command-line client for [Shipfolio](https://shipfolio.ai): Search Console and GA4 numbers for every product you run, in your terminal, in scripts and in CSV.

```console
$ shipfolio overview --period 7d
2026-11-15 to 2026-11-21 (7 days), against 2026-11-08 to 2026-11-14

PRODUCT              CLICKS  CHANGE  IMPRESSIONS  CHANGE  CTR   POSITION  SESSIONS  CHANGE  KEY EVENTS
formcraft.example    2,700   -40.0%  56,632       -41.9%  4.8%  10.7      4,517     -40.0%  348
fontpair.example     4,566   +7.3%   75,512       +5.7%   6.0%  8.2       —         —       —
unitwise.example     19,046  +0.4%   229,949      +2.2%   8.3%  5.1       31,859    +0.4%   195
```

A dash means Google reported nothing for that number (here, FontPair has no GA4 property connected). It is never shown as 0.

## Install

With Go 1.25 or later:

```sh
go install github.com/Cognivex-Lab/shipfolio-cli/cmd/shipfolio@latest
```

Prebuilt binaries for macOS, Linux and Windows are attached to each [release](https://github.com/Cognivex-Lab/shipfolio-cli/releases).

## Sign in

Create an API key in the dashboard under **Connections & API**, then:

```sh
shipfolio login            # paste the key at the prompt; it is not echoed
shipfolio whoami
```

The key is saved in `~/.config/shipfolio/config.json`, readable only by you. In CI or scripts, set `SHIPFOLIO_API_KEY` instead. `shipfolio logout` removes the saved key; revoke it in the dashboard to stop it working.

## Commands

```sh
shipfolio overview [--period 7d|28d|90d]
shipfolio metrics PRODUCT... [--metrics clicks,ctr,sessions] [--granularity day|week|month] [--period 28d | --start 2026-10-01 --end 2026-10-31] [--csv]
shipfolio breakdown PRODUCT query|page|country|device|channel|landing_page [--metric clicks] [--limit 25] [--csv]
shipfolio alerts [--product PRODUCT]

shipfolio products                      # list, with data sources
shipfolio products add NAME DOMAIN [--tags a,b]
shipfolio products rm PRODUCT [--yes]
shipfolio sources list PRODUCT
shipfolio sources add PRODUCT --connection CONN --external-id sc-domain:example.com
shipfolio sources rm PRODUCT SOURCE [--yes]
shipfolio sync PRODUCT                  # re-import the last few days now
shipfolio google connections
shipfolio google resources CONN

shipfolio api /v1/overview?period=last_7_days     # any endpoint, raw JSON
shipfolio mcp                                     # connect an AI agent
```

`PRODUCT` is a product id, its domain (`example.com`, `https://www.example.com/` both work) or its name.

Every command takes `--json` to print the API response unchanged, which is the stable format for scripts. The table output may change between versions.

Days Google may still revise are marked with `*` in `metrics`, and carry `"is_final": false` in JSON. In `--csv`, a number Google did not report is an empty cell.

Commands that delete data ask first. In a script, pass `--yes`.

## Examples

Daily clicks for two products into a spreadsheet:

```sh
shipfolio metrics example.com other.app --metrics clicks,impressions --period 90d --csv > clicks.csv
```

Fail a nightly job when any product has a new alert:

```sh
test "$(shipfolio alerts --json | jq '.data | length')" -eq 0
```

## AI agents

Shipfolio serves the same data over MCP at `https://api.shipfolio.ai/mcp`, authenticated with the same API key. `shipfolio mcp` prints a config block for clients that read an `mcpServers` file. Give agents a key with only the `metrics:read` scope.

The REST API is described in OpenAPI 3.1 at <https://api.shipfolio.ai/openapi.json>.

## Configuration

| Variable | Meaning |
| --- | --- |
| `SHIPFOLIO_API_KEY` | API key; used instead of the saved one |
| `SHIPFOLIO_API_URL` | API base URL, default `https://api.shipfolio.ai` |
| `SHIPFOLIO_CONFIG` | Config file path |

`--api-url` on any command overrides `SHIPFOLIO_API_URL`; `shipfolio login --api-url URL` saves it.

## Limits

Each key may make 120 requests a minute, and each workspace has a monthly allowance set by its plan. The CLI waits and retries briefly when rate limited; it does not retry when the monthly allowance is used up.

## Development

```sh
go test ./...
go run ./cmd/shipfolio help
```

The CLI uses only the Go standard library. Releases are built by GoReleaser when a `v*` tag is pushed.

## License

[MIT](LICENSE)
