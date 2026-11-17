package main

// Entry lifecycle: bind an ephemeral port, register it with the env, then poll
// /done until the suite finishes. This is the Go replacement for main.ts.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func envURL() string {
	return strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
}

func retryDelay(attempt int) time.Duration {
	shift := attempt
	if shift > 8 {
		shift = 8
	}
	d := 250 * time.Millisecond
	for i := 0; i < shift; i++ {
		d *= 2
	}
	if d > 2*time.Second {
		return 2 * time.Second
	}
	return d
}

func register(client *http.Client, base string, port int) error {
	payload, _ := json.Marshal(map[string]int{"port": port})
	for attempt := 0; ; attempt++ {
		resp, err := client.Post(base+"/register", "application/json", bytes.NewReader(payload))
		if err == nil {
			code := resp.StatusCode
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if code == 200 || code == 409 {
				return nil
			}
			if code == 400 || code == 405 {
				return fmt.Errorf("register answered HTTP %d", code)
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

func waitForDone(client *http.Client, base string) {
	for attempt := 0; ; attempt++ {
		resp, err := client.Get(base + "/done")
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr == nil && resp.StatusCode == 200 {
				var v struct {
					Done bool `json:"done"`
				}
				if json.Unmarshal(body, &v) == nil && v.Done {
					return
				}
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

func main() {
	base := envURL()
	if base == "" {
		fmt.Fprintln(os.Stderr, "[entry] MAKEFUN_ENV_URL is not set: there is no env to register with")
		os.Exit(1)
	}
	ln, port, err := startListener()
	if err != nil {
		fmt.Fprintln(os.Stderr, "[entry] listen failed:", err)
		os.Exit(1)
	}
	fmt.Printf("[entry] listening on port %d\n", port)
	client := &http.Client{Timeout: 30 * time.Second}
	if err := register(client, base, port); err != nil {
		fmt.Fprintln(os.Stderr, "[entry]", err)
		os.Exit(1)
	}
	fmt.Println("[entry] registered; waiting for the env to say done")
	waitForDone(client, base)
	ln.Close()
	fmt.Println("[entry] done")
}
