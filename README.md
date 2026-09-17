# GPT OWU Bridge

A self-hosted Go bridge for importing user-provided ChatGPT share snapshots into Open WebUI.

Current status: the M2 local administrator CLI, SQLite persistence, strict target ownership and synthetic tests are implemented. MCP, OAuth, continuous synchronization and real ChatGPT end-to-end acceptance are not complete.

Container delivery: see [deployment instructions](docs/DEPLOYMENT.md). The container currently exposes loopback health endpoints only; healthy does not mean ChatGPT is connected.

```sh
go test ./...
go test -race ./...
go vet ./...
go build -trimpath -o /tmp/gpt-owu-gate ./cmd/gpt-owu-gate
python3 scripts/m2_acceptance.py /tmp/gpt-owu-gate
```

The public distribution contains only source, synthetic fixtures and general documentation. Credentials, personal research, chat samples and proxy profiles are excluded.
