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

// Command fakenode impersonates the /status endpoint of a Gno (tm2) node.
//
// It exists so the operator can be demonstrated and exercised without running
// a real chain: a real gnoland node needs a genesis file, keys, peers and
// minutes of startup, and -- crucially -- cannot be told to halt on command.
//
// The response deliberately reproduces amino's JSON quirk of encoding 64-bit
// integers as quoted strings, because that is the detail the client has to
// cope with.
//
// Endpoints:
//
//	GET  /status          tm2-shaped status response
//	POST /admin/freeze    stop producing blocks (simulate a halted chain)
//	POST /admin/resume    start producing blocks again
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

type chain struct {
	mu        sync.Mutex
	height    int64
	blockTime time.Time
	frozen    bool
}

func (c *chain) tick(interval time.Duration) {
	for range time.Tick(interval) {
		c.mu.Lock()
		if !c.frozen {
			c.height++
			c.blockTime = time.Now()
		}
		c.mu.Unlock()
	}
}

func (c *chain) snapshot() (int64, time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.height, c.blockTime, c.frozen
}

func (c *chain) setFrozen(frozen bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frozen = frozen
}

func main() {
	var (
		moniker    = env("MONIKER", "fake-gno-node")
		network    = env("CHAIN_ID", "test13")
		listenAddr = ":" + env("PORT", "26657")
		catchingUp = env("CATCHING_UP", "false") == "true"
		startAt    = envInt("START_HEIGHT", 1000)
		blockEvery = time.Duration(envInt("BLOCK_TIME_MS", 1000)) * time.Millisecond
		frozen     = env("FROZEN", "false") == "true"
	)

	c := &chain{height: startAt, blockTime: time.Now(), frozen: frozen}
	go c.tick(blockEvery)

	mux := http.NewServeMux()

	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		height, blockTime, isFrozen := c.snapshot()

		// Amino encodes int64 as a QUOTED string. Reproducing that here is the
		// entire reason this fake is more useful than a static JSON file.
		body := fmt.Sprintf(`{
  "jsonrpc": "2.0",
  "id": "",
  "result": {
    "node_info": {"network": %q, "moniker": %q, "version": "fake"},
    "sync_info": {
      "latest_block_height": "%d",
      "latest_block_time": %q,
      "catching_up": %t
    },
    "validator_info": {"voting_power": "0"}
  }
}`, network, moniker, height, blockTime.UTC().Format(time.RFC3339Nano), catchingUp)

		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(body)); err != nil {
			log.Printf("writing status response: %v", err)
		}
		if isFrozen {
			log.Printf("served /status: height=%d FROZEN", height)
		}
	})

	for path, freeze := range map[string]bool{"/admin/freeze": true, "/admin/resume": false} {
		mux.HandleFunc("POST "+path, func(w http.ResponseWriter, _ *http.Request) {
			c.setFrozen(freeze)
			height, _, _ := c.snapshot()
			log.Printf("chain frozen=%t at height %d", freeze, height)
			if err := json.NewEncoder(w).Encode(map[string]any{
				"frozen": freeze, "height": height,
			}); err != nil {
				log.Printf("writing admin response: %v", err)
			}
		})
	}

	log.Printf("fakenode %q on %s: chain %s, height %d, block every %s, frozen=%t",
		moniker, listenAddr, network, startAt, blockEvery, frozen)

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int64) int64 {
	v, err := strconv.ParseInt(os.Getenv(key), 10, 64)
	if err != nil {
		return fallback
	}
	return v
}
