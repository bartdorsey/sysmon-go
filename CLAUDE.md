# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
# Build
go build .

# Run
./sysmon          # Listens on http://localhost:8080

# Format and vet
go fmt ./...
go vet ./...

# Docker
docker-compose up --build -d
```

There is no `go.mod` in the repository — it is generated at Docker build time via `go mod init sysmon`. To build locally, create one first:

```bash
go mod init sysmon
go build .
```

## Architecture

**Single-binary HTTP service** — [main.go](main.go) (~800 lines) embeds the entire frontend via Go's `//go:embed static` directive and serves it alongside two JSON API endpoints.

### API Endpoints

- `GET /api/stats` — dynamic metrics: CPU (aggregate + per-core), memory/swap, disk filesystems, top-5 processes by CPU and by memory, network I/O rates
- `GET /api/hardware` — static hardware info (CPU model, RAM type/speed); cached for 1 hour

### Metric Sources

All metrics are read directly from the Linux `/proc` and `/sys` filesystems with no external dependencies. The binary detects containerized execution by checking for `/host/proc` and transparently switches paths.

| Subsystem | Source |
|-----------|--------|
| CPU | `/proc/stat` |
| Memory/Swap | `/proc/meminfo` |
| Disk | `/proc/mounts` + `syscall.Statfs()` |
| Processes | `/proc/[pid]/stat`, `/proc/[pid]/status` |
| Network I/O | `/proc/net/dev`, `/sys/class/net/[iface]/` |
| Hardware | `/proc/cpuinfo`, `/sys/firmware/dmi/tables/DMITable` (SMBIOS Type 17) |

### Delta Sampling

CPU, process, and network metrics work by **diffing consecutive snapshots**. On startup, `main()` primes all snapshot state so the first API call returns meaningful values rather than zeros.

### Frontend

`static/` contains a zero-dependency vanilla JS single-page app (no build step). It polls `/api/stats` at a configurable interval and renders live charts/tables. Assets are embedded into the binary at compile time.
