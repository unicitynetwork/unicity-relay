# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Zooid is a multi-tenant Nostr relay built on [Khatru](https://gitworkshop.dev/fiatjaf.com/nostrlib/tree/master/khatru). A single process serves multiple "virtual" relays, each with its own TOML config file in `./config/`. This fork is customized for [Unicity Sphere](https://github.com/unicitylabs/sphere) NIP-29 group chat functionality.

## Commands

```bash
just run         # Run the relay (go run cmd/relay/main.go)
just build       # Build binary to bin/zooid (requires CGO_ENABLED=1)
just test        # Run all unit tests (go test -v ./...)
just fmt         # Format code (gofmt -w -s .)

# Run a single test
go test -v -run TestFunctionName ./zooid/

# Integration tests (require Docker, use build tag)
go test -v -tags=integration -run TestIntegration ./zooid/
```

## Architecture

### Multi-Tenancy via Host Dispatch

The entrypoint is `cmd/relay/main.go`. A single HTTP server dispatches requests to virtual relay instances based on the `Host` header via `zooid.Dispatch()`. Each config file in `./config/` creates one `Instance`.

### Instance Structure (`zooid/instance.go`)

Each `Instance` composes these stores:
- **Config** — parsed TOML config, holds relay secret key, role definitions
- **EventStore** — SQLite storage with schema-prefixed tables (e.g., `myrelay__events`), uses Squirrel query builder
- **GroupStore** — NIP-29 group management (create, delete, membership, visibility, invite codes)
- **ManagementStore** — NIP-86 relay management (ban/allow pubkeys/events, membership lists)
- **BlossomStore** — media file upload/download (stored on filesystem in `./media/`)

### Database

Single shared SQLite database at `./data/db` with WAL mode. Each virtual relay gets its own table namespace via `Schema.Prefix()` (e.g., `schemaname__events`, `schemaname__event_tags`). FTS5 is used for full-text search when available.

### Hot Reloading

`lib.go:Start()` watches the config directory with fsnotify. Adding, modifying, or removing config files will dynamically load/reload/unload relay instances without restart.

### Key NIP Implementations

- **NIP-29** (Groups): `groups.go` — group creation, metadata, membership, invite codes, visibility (private/hidden/closed). Group access checks happen in `CanRead()` and `CheckWrite()`.
- **NIP-42** (Auth): All requests require authentication. `OnConnect` calls `khatru.RequestAuth`.
- **NIP-86** (Management API): `management.go` — ban/allow pubkeys/events, membership management.
- **Relay Membership**: Separate from group membership. Tracked via custom kinds in `util.go` (RELAY_JOIN=28934, RELAY_MEMBERS=13534, etc.). Managed in `ManagementStore`.

### Group Visibility Model

Groups have three independent flags set via metadata tags: `private` (content restricted to members), `hidden` (existence hidden from non-members), `closed` (posting restricted to members). Private/hidden groups require invite codes to join.

### Environment Variables

`PORT` (default 3334), `CONFIG` (default ./config), `MEDIA` (default ./media), `DATA` (default ./data). See `env.go`.

### Config Policy Options

- `policy.open` — allows all authenticated users without relay membership
- `groups.admin_create_only` — restricts group creation to admins
- `groups.private_admin_only` — restricts private group creation to admins
- `groups.auto_join` — allows joining groups without approval
