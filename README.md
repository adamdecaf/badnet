## badnet

`badnet` is a TCP proxy for tests. Release notes are on [GitHub Releases](https://github.com/adamdecaf/badnet/releases). It sits between your application and the service it talks to so you can simulate latency, bandwidth limits, and connection failures without changing application code.

1. Start a proxy with `badnet.ForTest`
2. Point the application at `proxy.BindAddr()`
3. Run the tests you already have

Zero-config is a transparent TCP forwarder. Impairments are optional and applied from the **connecting client's** point of view.

```go
proxy := badnet.ForTest(t, badnet.Config{
    Listen: "127.0.0.1:0",
    Target: "127.0.0.1:8080", // host:port, http://..., https://..., or [IPv6]
    Read: badnet.Direction{ // data the client receives
        Latency:      50 * time.Millisecond,
        MaxKBps:      32,
        FailureRatio: 10, // percent of connections
    },
    Write: badnet.Direction{ // data the client sends
        FailureRatio: 5,
    },
})

// App dials the proxy instead of the real target.
addr := proxy.BindAddr()
```

### Config

| Field | Meaning |
| --- | --- |
| `Listen` | Local address to accept on. `127.0.0.1:0` picks a free port. |
| `Target` | Upstream `host:port`. `http://` defaults to 80, `https://` / `wss://` to 443. IPv6 works with or without brackets. |
| `Read` | Impairments on bytes the client **reads** (server → client). |
| `Write` | Impairments on bytes the client **writes** (client → server). |
| `DialTimeout` | Timeout when the proxy dials `Target`. Default 5s. |

Each `Direction`:

| Field | Meaning |
| --- | --- |
| `Latency` | Delay applied once, on the first I/O in that direction. |
| `MaxKBps` | Throughput cap in kilobytes/second. `0` is unlimited. |
| `FailureRatio` | 0–100. Chance **this connection** fails in that direction (partial I/O, then an error). Not per `Read`/`Write` call. |

`ForTest` shuts the proxy down in `t.Cleanup`. Target dial failures are counted in stats; they do not fail the caller’s test.

### Stats

```go
proxy.Port()          // listening port, or -1
proxy.BindAddr()      // host:port to dial
proxy.FailureRatio()  // failed connections / accepted connections
```

### Notes

- This is a TCP proxy. HTTP clients must set `Host` themselves if the upstream is name-based.
- Both directions are copied to completion. A client `CloseWrite` is forwarded so protocols that half-close still get a response.
- See [`examples/`](examples/) for HTTP, latency, and concurrent TCP.

Related
- https://github.com/Shopify/toxiproxy
- https://pkg.go.dev/github.com/cevatbarisyilmaz/lossy
- https://pkg.go.dev/golang.org/x/net/proxy
