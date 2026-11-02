# fribo-mcp

An [MCP](https://modelcontextprotocol.io) server for **Fribo** (Hyttetjenester AS,
[portal.hyttetjenester.no](https://portal.hyttetjenester.no)), the cabin road plowing service.

Register when you arrive at and leave the cabin, so the road is plowed before you come. The
server checks your contract's order deadline first and tells you whether plowing is guaranteed.

This uses the API behind Fribo's customer portal. It is not an official, documented API and may
change without notice. See [docs/api.md](docs/api.md) for what the server relies on.

## Tools

| Tool | What it does |
|---|---|
| `list_cabins` | Cabins on the account |
| `plowing_status` | Contract, season, order deadlines, earliest arrival with guaranteed plowing, last plowing and upcoming stays |
| `list_stays` | Registered stays, with whether plowing is guaranteed |
| `plowing_history` | When the road was plowed |
| `check_stay` | Check planned dates against deadlines, season and other stays, without registering |
| `register_stay` | Register a stay: arrival day and hour, departure day, optional days to skip plowing |
| `update_stay` | Change a registered stay |
| `cancel_stay` | Delete a stay that has not started |

`register_stay`, `update_stay` and `cancel_stay` are hidden when `MCP_READ_ONLY=true`.

Times are Norwegian local time. A stay starts at the arrival hour and ends at 23:59:59 on the
departure day, as in the portal.

## Configuration

| Variable | Required | Description |
|---|---|---|
| `FRIBO_EMAIL` | yes* | E-mail for portal.hyttetjenester.no |
| `FRIBO_PASSWORD` | yes* | Password |
| `FRIBO_CABIN_ID` | no | Cabin to use when a tool gets no `cabin_id`. Default: the only or default cabin |
| `MCP_AUTH_TOKEN` | for HTTP | Clients must send `Authorization: Bearer <token>` |
| `MCP_READ_ONLY` | no | `true` hides the tools that register, change and cancel stays |
| `MCP_TRANSPORT` | no | `http` (default, stateless Streamable HTTP on `/mcp`) or `stdio` |
| `PORT` | no | Default `8080` |
| `MCP_ALLOW_NO_AUTH` | no | Set to `1` to run HTTP without a token (trusted networks only) |
| `CONFIG_TOKEN` | no | Token for the settings API below. Defaults to `MCP_AUTH_TOKEN` |
| `DATA_DIR` | no | Where settings saved through the API are kept. Default `/data` |
| `FRIBO_API_URL` | no | Default `https://portal.hyttetjenester.no/__platform` |
| `LOG_LEVEL` | no | `debug`, `info` (default), `warn` or `error` |

\* Required either as environment variables or through the settings API.

### Login

The server logs in with e-mail and password and keeps the access token in memory only. Fribo's
access tokens last five minutes; the server logs in again shortly before expiry or on a 401, and
concurrent calls share one login. The portal's refresh token is not used: it rotates on every use
and lasts a day, so a fresh login is simpler and leaves nothing to go stale. Nothing about the
login is written to disk.

### Settings API and gateway

Run the server behind [mcp-gateway](https://github.com/Olsenius/mcp-gateway) for remote access.
The gateway adds OAuth, and its admin page shows these settings as a form with a **Test** button.

| Endpoint | |
|---|---|
| `GET /healthz` | Liveness |
| `GET /healthz?check=1` | Logs in and reads the cabin and plowing contract |
| `GET /config` | Settings, cabin choices and lock state. The password is never returned |
| `POST /config` | `{"values": {"FRIBO_PASSWORD": "…"}}`. Saved in `DATA_DIR` and applied without restart |

All but plain `/healthz` need `Authorization: Bearer <CONFIG_TOKEN>`. Settings given as environment
variables are locked.

### Tracing

OpenTelemetry tracing covers incoming HTTP, MCP methods and tool names, and each Fribo request.
It is off unless `OTEL_EXPORTER_OTLP_ENDPOINT` (or `..._TRACES_ENDPOINT`) is set; standard `OTEL_*`
variables apply (`http/protobuf` only). Fribo requests are recorded by route template, such as
`/plowing/customerPresences/{cabinId}`, so spans hold no ids, names, e-mail, tokens, arguments or
response bodies. Trace context is not sent to Fribo.

## Development

```bash
go test -race ./...
```

Tests use a fake Fribo API with synthetic data. A read-only check against the real API logs only
counts and statuses:

```bash
FRIBO_EMAIL=... FRIBO_PASSWORD=... go test -tags live -run Live -count=1 -v .
```
