# Working in fribo-mcp

- Read the workspace AGENTS.md first when working from mcp-workspace.
- Go, single `main` package. Run `gofmt`, `go vet ./...`, `go vet -tags live ./...` and
  `go test -race ./...` before committing; CI runs the same.
- Tests use the fake API in `fake_test.go` with synthetic data. Values starting with `private-`
  must never appear in tool output, errors or spans; keep the `assertNoPrivate` checks.
- The live test (`-tags live`) is read-only and logs only counts and statuses. Never register,
  change or delete stays on a real account without the account owner's explicit request.
- Never print or log e-mail, password, tokens, names, phone numbers, addresses, coordinates or
  driver details from the real API. Tool output may include the cabin name and visitors' first
  names, nothing more about people.
- Fribo has no public API. docs/api.md records what the server relies on; update it when
  behavior is confirmed or changes. Derive new endpoints from the portal's public JavaScript.
- Spans name Fribo requests by route template. Never add ids, query strings, bodies or headers.
- Keep the server stateless over MCP. Only the access token is cached, in memory.
