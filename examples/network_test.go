package tests

import (
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/adamdecaf/badnet"

	"github.com/stretchr/testify/require"
)

func TestHealthyNetwork(t *testing.T) {
	t.Run("HTTP GET", func(t *testing.T) {
		proxy := badnet.ForTest(t, badnet.Config{
			Listen: "127.0.0.1:0",
			Target: "example.com:80",
		})
		t.Logf("badnet proxy address: %v", proxy.BindAddr())

		req, err := http.NewRequest("GET", "http://"+proxy.BindAddr(), nil)
		require.NoError(t, err)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		t.Cleanup(func() { resp.Body.Close() })

		// Loading example.com by its IP gives a 404
		bs, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Contains(t, string(bs), "<h1>404 - Not Found</h1>")

		// Make multiple requests with one proxy
		for i := 0; i < 5; i++ {
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			t.Cleanup(func() { resp.Body.Close() })

			// Loading example.com by its IP gives a 404
			bs, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Contains(t, string(bs), "<h1>404 - Not Found</h1>")
		}
	})

	t.Run("throttled", func(t *testing.T) {
		proxy := badnet.ForTest(t, badnet.Config{
			Listen: "127.0.0.1:0",
			Target: "example.com:80",

			Read: badnet.Direction{
				MaxKBps: 1,
				Latency: 1 * time.Second,
			},
			Write: badnet.Direction{
				MaxKBps: 1,
				Latency: 1 * time.Second,
			},
		})
		t.Logf("badnet proxy address: %v", proxy.BindAddr())

		req, err := http.NewRequest("GET", "http://"+proxy.BindAddr(), nil)
		require.NoError(t, err)

		start := time.Now()
		resp, err := http.DefaultClient.Do(req)
		end := time.Since(start)

		require.NoError(t, err)
		t.Cleanup(func() { resp.Body.Close() })

		// Verify at least one second passes while the HTTP request completes
		require.Greater(t, end.Milliseconds(), (1 * time.Second).Milliseconds())

		// Loading example.com by its IP gives a 404
		bs, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Contains(t, string(bs), "<h1>404 - Not Found</h1>")
	})
}
