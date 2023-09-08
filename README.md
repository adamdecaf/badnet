## badnet

`badnet` is a lightweight TCP proxy that sits in the middle of your tests and what they're trying to connect to. badnet sits outside of your application or tests to help simulate real network failures without modifying application code.

1. Setup a proxy
1. Connect your application
1. Run typical tests

Future Ideas
- Add channel / helpers to modify proxy behavior in the middle of an integration test

Related
- https://pkg.go.dev/go4.org/net/throttle#Rate
- https://pkg.go.dev/io#LimitReader
- https://pkg.go.dev/golang.org/x/net@v0.14.0/netutil
- https://pkg.go.dev/golang.org/x/net@v0.14.0/proxy
- https://github.com/Shopify/toxiproxy
- https://pkg.go.dev/github.com/cevatbarisyilmaz/lossy#section-readme
