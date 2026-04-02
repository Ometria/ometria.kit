# ometria.kit

**Language**: Go
**Purpose**: Shared Go building blocks for Ometria Go APIs. Provides standardized HTTP API infrastructure.

## Key packages
- `api` — HTTP API interface and `NewServer` factory. `API` interface + `http.Server` wrapper for consistent API setup across Go services.

## Code structure
- `api/` — HTTP API package

## Related repos
- Go API services that use this as a foundation
