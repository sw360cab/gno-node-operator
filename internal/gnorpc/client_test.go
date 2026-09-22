/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package gnorpc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// aminoStatusBody is shaped like a real tm2 reply: note that
// latest_block_height arrives as a QUOTED string, because amino encodes every
// 64-bit integer that way. This is the payload that breaks a naive int64 field.
const aminoStatusBody = `{
  "jsonrpc": "2.0",
  "id": "",
  "result": {
    "node_info": {
      "network": "test13",
      "moniker": "gnocore-rpc-01",
      "version": "0.0.0"
    },
    "sync_info": {
      "latest_block_hash": "7Fg1x4LqB2M=",
      "latest_block_height": "48213",
      "latest_block_time": "2026-09-22T09:15:04.123456789Z",
      "catching_up": false
    },
    "validator_info": {
      "voting_power": "0"
    }
  }
}`

func serve(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL
}

func statusHandler(t *testing.T, body string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			t.Errorf("probed %q, want /status", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("used %s, want GET", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func TestStatusDecodesAminoQuotedHeight(t *testing.T) {
	endpoint := serve(t, statusHandler(t, aminoStatusBody))

	got, err := New(time.Second).Status(t.Context(), endpoint)
	if err != nil {
		t.Fatalf("Status() returned error: %v", err)
	}

	if got.Height != 48213 {
		t.Errorf("Height = %d, want 48213", got.Height)
	}
	if got.Moniker != "gnocore-rpc-01" {
		t.Errorf("Moniker = %q, want %q", got.Moniker, "gnocore-rpc-01")
	}
	if got.Network != "test13" {
		t.Errorf("Network = %q, want %q", got.Network, "test13")
	}
	if got.CatchingUp {
		t.Error("CatchingUp = true, want false")
	}
	want := time.Date(2026, 9, 22, 9, 15, 4, 123456789, time.UTC)
	if !got.BlockTime.Equal(want) {
		t.Errorf("BlockTime = %s, want %s", got.BlockTime, want)
	}
}

func TestStatusAcceptsBareNumericHeight(t *testing.T) {
	// Forward compatibility: if tm2 ever stops using amino for RPC results,
	// an unquoted height must keep working.
	body := strings.Replace(aminoStatusBody, `"latest_block_height": "48213"`,
		`"latest_block_height": 48213`, 1)
	endpoint := serve(t, statusHandler(t, body))

	got, err := New(time.Second).Status(t.Context(), endpoint)
	if err != nil {
		t.Fatalf("Status() returned error: %v", err)
	}
	if got.Height != 48213 {
		t.Errorf("Height = %d, want 48213", got.Height)
	}
}

func TestStatusReportsCatchingUp(t *testing.T) {
	body := strings.Replace(aminoStatusBody, `"catching_up": false`, `"catching_up": true`, 1)
	endpoint := serve(t, statusHandler(t, body))

	got, err := New(time.Second).Status(t.Context(), endpoint)
	if err != nil {
		t.Fatalf("Status() returned error: %v", err)
	}
	if !got.CatchingUp {
		t.Error("CatchingUp = false, want true")
	}
}

func TestStatusTrailingSlashEndpoint(t *testing.T) {
	endpoint := serve(t, statusHandler(t, aminoStatusBody))

	// A Service URL assembled by string concatenation often ends in "/".
	if _, err := New(time.Second).Status(t.Context(), endpoint+"/"); err != nil {
		t.Fatalf("Status() with trailing slash returned error: %v", err)
	}
}

func TestStatusErrors(t *testing.T) {
	tests := []struct {
		name        string
		handler     http.HandlerFunc
		wantErrPart string
	}{
		{
			name: "http error status",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			wantErrPart: "unexpected status",
		},
		{
			name: "rpc error envelope",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"","error":{"code":-32603,"message":"Internal error","data":"boom"}}`))
			},
			wantErrPart: "rpc error",
		},
		{
			name: "result missing",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":""}`))
			},
			wantErrPart: "empty result",
		},
		{
			name: "body is not json",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`<html>502 Bad Gateway</html>`))
			},
			wantErrPart: "decoding response envelope",
		},
		{
			name: "height is not a number",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"result":{"sync_info":{"latest_block_height":"soon"}}}`))
			},
			wantErrPart: "decoding status",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			endpoint := serve(t, tt.handler)

			_, err := New(time.Second).Status(t.Context(), endpoint)
			if err == nil {
				t.Fatal("Status() returned no error, want one")
			}
			if !strings.Contains(err.Error(), tt.wantErrPart) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErrPart)
			}
		})
	}
}

func TestStatusUnreachableEndpoint(t *testing.T) {
	// Port 1 on loopback refuses connections immediately.
	_, err := New(time.Second).Status(t.Context(), "http://127.0.0.1:1")
	if err == nil {
		t.Fatal("Status() returned no error for an unreachable endpoint")
	}
}

func TestStatusHonoursContextCancellation(t *testing.T) {
	endpoint := serve(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	// The client timeout is deliberately long: the context must win.
	if _, err := New(time.Minute).Status(ctx, endpoint); err == nil {
		t.Fatal("Status() returned no error, want a deadline error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Status() took %s, context deadline was ignored", elapsed)
	}
}
