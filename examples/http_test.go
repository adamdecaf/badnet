package tests

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/adamdecaf/badnet"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"
)

func TestHTTP(t *testing.T) {
	router := mux.NewRouter()
	router.Methods("GET").Path("/ping").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("PONG", 34)))
	})

	server := &http.Server{
		Addr:    ":9119",
		Handler: router,
	}
	go server.ListenAndServe()
	t.Cleanup(func() {
		server.Shutdown(context.Background())
	})

	t.Run("10% failure", func(t *testing.T) {
		proxy := badnet.ForTest(t, badnet.Config{
			Listen: "127.0.0.1:0",
			Target: "127.0.0.1:9119",

			Read:  badnet.Direction{FailureRatio: 10},
			Write: badnet.Direction{FailureRatio: 10},
		})

		// Make a bunch of requests
		successful, partial, failed := makePingRequests(proxy)
		require.Equal(t, 100, successful+partial+failed)

		// Check some basic facts
		require.InDelta(t, 90, successful, 20)
		require.Greater(t, partial+failed, 0)
	})

	t.Run("50% failure", func(t *testing.T) {
		proxy := badnet.ForTest(t, badnet.Config{
			Listen: "127.0.0.1:0",
			Target: "127.0.0.1:9119",

			Read:  badnet.Direction{FailureRatio: 50},
			Write: badnet.Direction{FailureRatio: 50},
		})

		// Make a bunch of requests
		successful, partial, failed := makePingRequests(proxy)
		require.Equal(t, 100, successful+partial+failed)

		// Check some basic facts
		require.InDelta(t, 50, successful, 40)
		require.Greater(t, partial+failed, 10)
	})

	t.Run("99% failure", func(t *testing.T) {
		proxy := badnet.ForTest(t, badnet.Config{
			Listen: "127.0.0.1:0",
			Target: "127.0.0.1:9119",

			Read:  badnet.Direction{FailureRatio: 99},
			Write: badnet.Direction{FailureRatio: 99},
		})

		// Make a bunch of requests
		successful, partial, failed := makePingRequests(proxy)
		require.Equal(t, 100, successful+partial+failed)

		// Check some basic facts
		require.InDelta(t, 10, successful, 10)
		require.Greater(t, partial+failed, 50)
	})
}

func makePingRequests(proxy *badnet.Proxy) (successful, partial, failed int) {
	for i := 0; i < 100; i++ {
		resp, err := http.DefaultClient.Get("http://" + proxy.BindAddr() + "/ping")
		if err != nil {
			failed += 1
			continue
		}
		// Can we read the entire response
		if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
			bs, err := io.ReadAll(resp.Body)
			if err != nil {
				partial += 1
			} else {
				if len(bs) == 136 { // "PONG" * 34
					successful += 1
				} else {
					partial += 1
				}
			}
			resp.Body.Close()
		}
	}
	return
}
