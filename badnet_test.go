package badnet

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConfig(t *testing.T) {
	t.Run("targetAddress", func(t *testing.T) {
		conf := Config{
			Target: "127.0.0.1:9119",
		}
		require.Equal(t, "127.0.0.1:9119", conf.targetAddress())

		conf.Target = "http://127.0.0.1:9119"
		require.Equal(t, "127.0.0.1:9119", conf.targetAddress())

		conf.Target = "https://127.0.0.1:9119"
		require.Equal(t, "127.0.0.1:9119", conf.targetAddress())

		conf.Target = "example.com"
		require.Equal(t, "example.com:80", conf.targetAddress())

		conf.Target = "example.com:81"
		require.Equal(t, "example.com:81", conf.targetAddress())

		conf.Target = "http://example.com"
		require.Equal(t, "example.com:80", conf.targetAddress())
	})
}

func TestProxy(t *testing.T) {
	t.Run("BindAddr / Port", func(t *testing.T) {
		proxy := ForTest(t, Config{
			Listen: "127.0.0.1:0",
			Target: "www.example.com:80",
		})
		t.Logf("badnet proxy address: %v", proxy.BindAddr())

		port := proxy.Port()
		require.Greater(t, port, 0)
		require.Less(t, port, 65535)
	})

	t.Run("stats", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("PONG"))
		})
		server := &http.Server{
			Addr:    ":12345",
			Handler: handler,
		}
		go server.ListenAndServe()
		t.Cleanup(func() {
			server.Shutdown(context.Background())
		})

		proxy := ForTest(t, Config{
			Listen: "127.0.0.1:0",
			Target: "127.0.0.1:12345",
			Read:   Direction{FailureRatio: 25},
			Write:  Direction{FailureRatio: 25},
		})

		address := "http://" + proxy.BindAddr()
		t.Logf("badnet proxy address: %v", address)

		for i := 0; i < 100; i++ {
			resp, _ := http.DefaultClient.Get(address)
			if resp != nil && resp.Body != nil {
				resp.Body.Close()
			}
		}

		failureRatio := proxy.FailureRatio()
		require.InDelta(t, failureRatio, 0.5, 0.3)
	})
}
