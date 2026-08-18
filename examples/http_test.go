package tests

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/adamdecaf/badnet"

	"github.com/stretchr/testify/require"
)

// FailureRatio is per connection. Read fails the response; Write fails the request.
func TestHTTP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("PONG", 34)))
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go server.Serve(ln)
	t.Cleanup(func() { _ = server.Close() })
	target := ln.Addr().String()

	t.Run("10% failure", func(t *testing.T) {
		proxy := badnet.ForTest(t, badnet.Config{
			Listen: "127.0.0.1:0",
			Target: target,
			Read:   badnet.Direction{FailureRatio: 10},
			Write:  badnet.Direction{FailureRatio: 10},
		})

		successful, partial, failed := makePingRequests(proxy)
		require.Equal(t, 100, successful+partial+failed)
		require.InDelta(t, 81, successful, 25)
		require.Greater(t, partial+failed, 0)
	})

	t.Run("50% failure", func(t *testing.T) {
		proxy := badnet.ForTest(t, badnet.Config{
			Listen: "127.0.0.1:0",
			Target: target,
			Read:   badnet.Direction{FailureRatio: 50},
			Write:  badnet.Direction{FailureRatio: 50},
		})

		successful, partial, failed := makePingRequests(proxy)
		require.Equal(t, 100, successful+partial+failed)
		require.InDelta(t, 25, successful, 30)
		require.Greater(t, partial+failed, 10)
	})

	t.Run("99% failure", func(t *testing.T) {
		proxy := badnet.ForTest(t, badnet.Config{
			Listen: "127.0.0.1:0",
			Target: target,
			Read:   badnet.Direction{FailureRatio: 99},
			Write:  badnet.Direction{FailureRatio: 99},
		})

		successful, partial, failed := makePingRequests(proxy)
		require.Equal(t, 100, successful+partial+failed)
		require.Less(t, successful, 20)
		require.Greater(t, partial+failed, 50)
	})
}

func makePingRequests(proxy *badnet.Proxy) (successful, partial, failed int) {
	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	for i := 0; i < 100; i++ {
		resp, err := client.Get("http://" + proxy.BindAddr() + "/ping")
		if err != nil {
			failed++
			continue
		}
		bs, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			failed++
			continue
		}
		if err != nil {
			partial++
			continue
		}
		if len(bs) == 136 {
			successful++
		} else {
			partial++
		}
	}
	return
}
