## v0.5.0 (Unreleased)

BREAKING

- `Read` and `Write` are from the connecting client's point of view
- `FailureRatio` is per connection, not per Read/Write syscall
- `Latency` is applied once on the first I/O in that direction
- Removed `go4.org/net/throttle`; zero-config connections are raw TCP

IMPROVEMENTS

- `https://` and `wss://` targets default to port 443
- IPv6 targets no longer produce `[[::1]]:80`
- Both directions are copied to completion; TCP half-close is forwarded
- Target dial failures increment stats instead of failing the caller test
- `Config.DialTimeout` (default 5s) when connecting to the target
- Cleanup waits for in-flight connections

## v0.4.0 (Released 2025-04-17)

IMPROVEMENTS

- chore: refactor and fix issues
- examples: add concurrent TCP test

BUILD

- chore(deps): update actions/setup-go action to v5
- fix(deps): update module github.com/stretchr/testify to v1.9.0

## v0.3.0 (Released 2024-02-15)

IMPROVEMENTS

- feat: add FailureRatio() to Proxy for quick stats

## v0.2.0 (Released 2024-02-01)

IMPROVEMENTS

- feat: add .Port()

## v0.1.0 (Released 2023-09-11)

IMPROVEMENTS

- feat: replace 'Host' header in proxied HTTP requests
- test: add basic checks for various HTTP failure rates
