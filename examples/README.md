## examples

Integration-style tests that use `badnet` as a test would.

```
cd examples && go test -count=1 ./...
```

| File | What it shows |
| --- | --- |
| `healthy_test.go` | Transparent proxy and read/write latency + bandwidth |
| `http_test.go` | Per-connection `FailureRatio` on HTTP GET |
| `tcp_test.go` | Concurrent echo, half-close, and mixed impairments |

Point the client at `proxy.BindAddr()`, not the real server. `Read` / `Write` are from that client's point of view.
