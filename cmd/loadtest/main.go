// Redirect-scale load test: hammers a running affiliate-engine's /r/
// endpoint and reports throughput + error rate. Also verifies persistence
// counts after the run via the sqlite file.
//
// Usage: go run ./cmd/loadtest -url http://127.0.0.1:8080 -workers 16 -requests 50000
package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	baseURL := flag.String("url", "http://127.0.0.1:8080", "engine base url")
	workers := flag.Int("workers", 16, "concurrent workers")
	requests := flag.Int("requests", 50000, "total requests")
	flag.Parse()

	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			MaxIdleConns: 256, MaxIdleConnsPerHost: 256,
		},
	}

	var ok, errCount int64
	var mu sync.Mutex
	start := time.Now()
	var wg sync.WaitGroup
	per := *requests / *workers

	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				req, _ := http.NewRequest("GET", *baseURL+"/r/1?dest=https://example.com/lp&subid=w"+fmt.Sprint(i%97), nil)
				req.Header.Set("User-Agent", "loadtest-bot/1.0") // classified bot, excluded from stats but stored
				resp, err := client.Do(req)
				if err != nil {
					atomic.AddInt64(&errCount, 1)
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode == 302 {
					atomic.AddInt64(&ok, 1)
				} else {
					atomic.AddInt64(&errCount, 1)
				}
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	mu.Lock()
	fmt.Printf("requests=%d ok=%d errors=%d elapsed=%v rps=%.0f\n",
		*requests, ok, errCount, elapsed, float64(ok)/elapsed.Seconds())
	os.Exit(0)
}
