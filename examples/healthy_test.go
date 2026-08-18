package tests

import (
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/adamdecaf/badnet"

	"github.com/stretchr/testify/require"
)

func TestHealthyNetwork(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("NeverSSL"))
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go server.Serve(ln)
	t.Cleanup(func() { _ = server.Close() })
	target := ln.Addr().String()

	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}

	t.Run("HTTP GET", func(t *testing.T) {
		proxy := badnet.ForTest(t, badnet.Config{
			Listen: "127.0.0.1:0",
			Target: target,
		})
		t.Logf("badnet proxy address: %v", proxy.BindAddr())

		for i := 0; i < 4; i++ {
			resp, err := client.Get("http://" + proxy.BindAddr())
			require.NoError(t, err)
			bs, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Contains(t, string(bs), "NeverSSL")
		}
	})

	t.Run("throttled", func(t *testing.T) {
		proxy := badnet.ForTest(t, badnet.Config{
			Listen: "127.0.0.1:0",
			Target: target,
			Read: badnet.Direction{
				MaxKBps: 10,
				Latency: 200 * time.Millisecond,
			},
			Write: badnet.Direction{
				MaxKBps: 10,
				Latency: 200 * time.Millisecond,
			},
		})
		t.Logf("badnet proxy address: %v", proxy.BindAddr())

		start := time.Now()
		resp, err := client.Get("http://" + proxy.BindAddr())
		elapsed := time.Since(start)
		require.NoError(t, err)
		t.Cleanup(func() { resp.Body.Close() })

		require.GreaterOrEqual(t, elapsed, 200*time.Millisecond)

		bs, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Contains(t, string(bs), "NeverSSL")
	})
}
