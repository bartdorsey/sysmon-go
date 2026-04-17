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

## Architecture

**Single-binary HTTP service** — [main.go](main.go) (~1200 lines) embeds the entire frontend via Go's `//go:embed static` directive and serves it alongside JSON API endpoints.

### API Endpoints

- `GET /api/stats` — dynamic metrics: CPU (aggregate + per-core), memory/swap, disk filesystems, top-5 processes by CPU and by memory, network I/O rates
- `GET /api/hardware` — static hardware info (CPU model, RAM type/speed, hostname, OS, kernel, arch); cached for 1 hour via `sync.Once`
- `GET /api/services` — systemd service list via D-Bus (`org.freedesktop.systemd1`); cached for 5 seconds
- `GET /api/logs?unit=<name>` — last 20 journal entries for a service, fetched via `journalctl --root=/host`; unit name is validated against `[a-zA-Z0-9@._-]+`

### Metric Sources

All metrics are read directly from the Linux `/proc` and `/sys` filesystems. The binary detects containerized execution by checking for `/host/proc` and transparently switches paths. The Docker volume mount is `/:/host:ro`.

| Subsystem | Source |
|-----------|--------|
| CPU | `/proc/stat` |
| Memory/Swap | `/proc/meminfo` |
| Disk | `/proc/mounts` + `syscall.Statfs()` |
| Processes | `/proc/[pid]/stat`, `/proc/[pid]/status` |
| Network I/O | `/proc/net/dev`, `/sys/class/net/[iface]/` |
| Hardware | `/proc/cpuinfo`, `/sys/firmware/dmi/tables/DMITable` (SMBIOS Type 17) |
| Services | D-Bus system socket (`/run/dbus/system_bus_socket` or `/host/run/dbus/`) |

### Delta Sampling

CPU, process, and network metrics work by **diffing consecutive snapshots**. On startup, `main()` primes all snapshot state so the first API call returns meaningful values rather than zeros.

### External Dependencies

- `github.com/godbus/dbus/v5` — used only for the `/api/services` endpoint to query systemd over D-Bus

### Frontend

`static/` contains a zero-dependency vanilla JS app (no build step) with two pages:
- `index.html` + `app.js` — dashboard polling `/api/stats` at a configurable interval with live charts/tables
- `services.html` + `services.js` — services list with inline log viewer using `/api/services` and `/api/logs`

Assets are embedded into the binary at compile time.
