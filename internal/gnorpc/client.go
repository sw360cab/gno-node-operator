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

// Package gnorpc is a minimal read-only client for the Gno (tm2) RPC API.
//
// It deliberately does not import github.com/gnolang/gno. Pulling the whole
// tm2 dependency tree into an operator to read four fields is a bad trade;
// the wire contract for /status is stable and small enough to restate here.
package gnorpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// maxResponseBytes caps how much of a response body we are willing to read.
// The endpoint is remote and may be unhealthy in interesting ways.
const maxResponseBytes = 1 << 20 // 1 MiB

// Status is the subset of the /status response this controller acts on.
type Status struct {
	// Moniker is the node's self-assigned name.
	Moniker string
	// Network is the chain ID the node believes it is on.
	Network string
	// Height is the latest block the node has committed.
	Height int64
	// BlockTime is the timestamp of that block.
	BlockTime time.Time
	// CatchingUp is true while the node is still replaying history.
	CatchingUp bool
}

// Client performs RPC probes against a Gno node.
type Client struct {
	httpClient *http.Client
}

// New returns a Client whose requests are bounded by timeout.
func New(timeout time.Duration) *Client {
	return &Client{httpClient: &http.Client{Timeout: timeout}}
}

// Status fetches GET {endpoint}/status and returns the fields we care about.
//
// The context bounds the call in addition to the client timeout, so a
// reconcile that is cancelled does not leave a request in flight.
func (c *Client) Status(ctx context.Context, endpoint string) (*Status, error) {
	target, err := url.JoinPath(endpoint, "status")
	if err != nil {
		return nil, fmt.Errorf("building status URL from %q: %w", endpoint, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("probing %s: %w", target, err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only request

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("probing %s: unexpected status %s", target, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("reading response from %s: %w", target, err)
	}

	var envelope rpcResponse
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decoding response envelope from %s: %w", target, err)
	}
	if envelope.Error != nil {
		return nil, fmt.Errorf("rpc error from %s: %s", target, envelope.Error)
	}
	if len(envelope.Result) == 0 {
		return nil, fmt.Errorf("empty result in response from %s", target)
	}

	var result resultStatus
	if err := json.Unmarshal(envelope.Result, &result); err != nil {
		return nil, fmt.Errorf("decoding status from %s: %w", target, err)
	}

	return &Status{
		Moniker:    result.NodeInfo.Moniker,
		Network:    result.NodeInfo.Network,
		Height:     int64(result.SyncInfo.LatestBlockHeight),
		BlockTime:  result.SyncInfo.LatestBlockTime,
		CatchingUp: result.SyncInfo.CatchingUp,
	}, nil
}

// rpcResponse is the JSON-RPC envelope every tm2 RPC method replies with.
type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data"`
}

func (e *rpcError) String() string {
	if e.Data == "" {
		return fmt.Sprintf("%s (code %d)", e.Message, e.Code)
	}
	return fmt.Sprintf("%s: %s (code %d)", e.Message, e.Data, e.Code)
}

// resultStatus mirrors only the parts of tm2's ResultStatus that we read.
type resultStatus struct {
	NodeInfo struct {
		Moniker string `json:"moniker"`
		Network string `json:"network"`
	} `json:"node_info"`
	SyncInfo struct {
		LatestBlockHeight aminoInt64 `json:"latest_block_height"`
		LatestBlockTime   time.Time  `json:"latest_block_time"`
		CatchingUp        bool       `json:"catching_up"`
	} `json:"sync_info"`
}

// aminoInt64 decodes an int64 that amino has encoded as a quoted string.
//
// tm2 marshals RPC results with amino, which writes every 64-bit integer as
// `"48213"` rather than `48213`, on the grounds that JavaScript cannot
// represent int64 exactly. A plain int64 field fails to unmarshal against it.
// Bare numbers are accepted too, so the client keeps working if that ever
// changes.
type aminoInt64 int64

func (a *aminoInt64) UnmarshalJSON(data []byte) error {
	raw := strings.TrimSpace(string(data))
	if raw == "null" {
		return nil
	}
	raw = strings.Trim(raw, `"`)

	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fmt.Errorf("parsing %q as int64: %w", raw, err)
	}
	*a = aminoInt64(value)
	return nil
}
