package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// razorpay3DSResult holds the result of the browser-based 3DS handling.
type razorpay3DSResult struct {
	PageText        string
	PageClosedEarly bool
	Charged         bool
}

// solverRequest is the JSON body sent to the solver service.
type solverRequest struct {
	URL   string `json:"url"`
	Proxy string `json:"proxy"`
}

// solverResponse is what the solver service returns.
type solverResponse struct {
	Charged     bool   `json:"charged"`
	PageText    string `json:"page_text"`
	ClosedEarly bool   `json:"closed_early"`
	Error       string `json:"error,omitempty"`
}

var solverRoundRobin uint64

// getSolverURLs returns the list of solver service URLs from env vars.
// Supports RAZORPAY_SOLVER_URLS (comma-separated) and RAZORPAY_SOLVER_URL (single).
func getSolverURLs() []string {
	var urls []string

	if multi := os.Getenv("RAZORPAY_SOLVER_URLS"); multi != "" {
		for _, u := range strings.Split(multi, ",") {
			u = strings.TrimSpace(u)
			if u != "" {
				urls = append(urls, u)
			}
		}
	}

	if single := os.Getenv("RAZORPAY_SOLVER_URL"); single != "" {
		single = strings.TrimSpace(single)
		if single != "" {
			urls = append(urls, single)
		}
	}

	return urls
}

// handle3DSRedirect opens the 3DS redirect URL in a headless browser,
// waits for the bank page to load/process, reads the final page text,
// and determines if the payment was charged (frictionless 3DS).
//
// If RAZORPAY_SOLVER_URLS or RAZORPAY_SOLVER_URL is set, it calls the
// external solver service(s) with round-robin load balancing and failover.
// Otherwise, it runs a local headless Chrome instance.
func handle3DSRedirect(redirectURL, proxyURL string) *razorpay3DSResult {
	solverURLs := getSolverURLs()

	if len(solverURLs) > 0 {
		startIdx := int(atomic.AddUint64(&solverRoundRobin, 1)-1) % len(solverURLs)
		allBusy := true

		for i := 0; i < len(solverURLs); i++ {
			idx := (startIdx + i) % len(solverURLs)
			result, busy := solveViaHTTP(solverURLs[idx], redirectURL, proxyURL)
			if result != nil {
				// Don't retry on empty results - if the first solver couldn't
				// load the bank page through the proxy, the others won't either
				// (same proxy, same ERR_TUNNEL_CONNECTION_FAILED). Return the
				// result and let the bot's status check handle classification.
				if strings.Contains(strings.ToLower(result.PageText), "403 forbidden") {
					fmt.Printf("[RAZ] solver %s got 403, trying next\n", solverURLs[idx])
					continue
				}
				return result
			}
			if !busy {
				allBusy = false
			}
		}

		if allBusy {
			fmt.Printf("[RAZ] all %d solver(s) busy, skipping 3DS\n", len(solverURLs))
			return &razorpay3DSResult{}
		}

		fmt.Printf("[RAZ] all %d solver(s) failed, falling back to local browser\n", len(solverURLs))
	}

	return solveLocal(redirectURL, proxyURL)
}

// solveViaHTTP calls the external solver service via HTTP.
// Returns (result, busy). If busy=true, the solver returned 503 (all slots taken).
// Returns (nil, false) if the solver had a real error.
func solveViaHTTP(solverURL, redirectURL, proxyURL string) (*razorpay3DSResult, bool) {
	reqBody := solverRequest{URL: redirectURL, Proxy: proxyURL}
	jsonBody, _ := json.Marshal(reqBody)

	solveEndpoint := strings.TrimRight(solverURL, "/") + "/solve"

	client := &http.Client{Timeout: 45 * time.Second}
	req, err := http.NewRequest("POST", solveEndpoint, bytes.NewReader(jsonBody))
	if err != nil {
		fmt.Printf("[RAZ] solver %s req error: %v\n", solverURL, err)
		return nil, false
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("[RAZ] solver %s HTTP error: %v\n", solverURL, err)
		return nil, false
	}
	defer resp.Body.Close()

	if resp.StatusCode == 503 {
		return nil, true
	}

	if resp.StatusCode != 200 {
		fmt.Printf("[RAZ] solver %s HTTP status: %d\n", solverURL, resp.StatusCode)
		return nil, false
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Printf("[RAZ] solver %s read body error: %v\n", solverURL, err)
		return nil, false
	}

	var sr solverResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		fmt.Printf("[RAZ] solver %s parse error: %v body=%s\n", solverURL, err, string(body))
		return nil, false
	}

	result := &razorpay3DSResult{
		PageText:        sr.PageText,
		PageClosedEarly: sr.ClosedEarly,
		Charged:         sr.Charged,
	}

	fmt.Printf("[RAZ] 3ds solver=%s charged=%v closed_early=%v page_text_len=%d\n",
		solverURL, result.Charged, result.PageClosedEarly, len(result.PageText))
	if result.PageText != "" {
		fmt.Printf("[RAZ] 3ds solver page_text=%s\n", result.PageText)
	}
	if sr.Error != "" {
		fmt.Printf("[RAZ] 3ds solver error=%s\n", sr.Error)
	}

	return result, false
}

// solveLocal runs a local headless Chrome instance to handle the 3DS redirect.
func solveLocal(redirectURL, proxyURL string) *razorpay3DSResult {
	result := &razorpay3DSResult{}

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-web-security", true),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("ignore-certificate-errors", true),
		chromedp.WindowSize(1366, 768),
		chromedp.UserAgent(razorpayUserAgent),
	)

	// Use explicit Chrome/Chromium path if found (needed for Docker/Alpine)
	if chromePath := findChromePath(); chromePath != "" {
		opts = append(opts, chromedp.ExecPath(chromePath))
	}

	if proxyURL != "" {
		opts = append(opts, chromedp.ProxyServer(proxyURL))
	}

	allocCtx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()

	// Set a hard timeout for the whole browser session
	browserCtx, cancelBrowser := context.WithTimeout(allocCtx, 40*time.Second)
	defer cancelBrowser()

	taskCtx, cancelTask := chromedp.NewContext(browserCtx)
	defer cancelTask()

	// Navigate to the 3DS redirect URL
	// The page auto-submits a form via JavaScript to the bank's 3DS page.
	// We need to wait for the bank to process and potentially redirect back.
	navigateErr := chromedp.Run(taskCtx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			// Enable network to track redirects
			if err := network.Enable().Do(ctx); err != nil {
				return nil // non-fatal
			}
			return nil
		}),
		// Navigate with DOM content loaded (don't wait for full page load)
		chromedp.Navigate(redirectURL),
	)

	if navigateErr != nil {
		errStr := navigateErr.Error()
		if strings.Contains(errStr, "Target closed") || strings.Contains(errStr, "browser has been closed") {
			result.PageClosedEarly = true
			return result
		}
		// Try again with a different wait strategy
		_ = chromedp.Run(taskCtx,
			chromedp.Navigate(redirectURL),
		)
	}

	// Wait for the bank page to process 3DS.
	// Frictionless 3DS will auto-redirect back to Razorpay with a signature.
	// Challenge 3DS will show an OTP form (we can't complete it).
	waitCtx, cancelWait := context.WithTimeout(taskCtx, 37*time.Second)
	defer cancelWait()

	_ = chromedp.Run(waitCtx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			deadline := time.Now().Add(36 * time.Second)
			leftPgRouter := false
			leftPgRouterTime := time.Time{}

			for time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
				}

				var currentURL string
				_ = chromedp.Run(ctx, chromedp.Location(&currentURL))

				var pageText string
				_ = chromedp.Run(ctx, chromedp.Text("body", &pageText, chromedp.ByQuery))
				pageText = strings.TrimSpace(pageText)
				lowerText := strings.ToLower(pageText)

				if pageText != "" {
					if strings.Contains(lowerText, "razorpay_signature") ||
						strings.Contains(lowerText, "payment successful") ||
						strings.Contains(lowerText, "payment_success") ||
						strings.Contains(lowerText, "payment succeeded") ||
						strings.Contains(lowerText, "payment_done") {
						result.Charged = true
						result.PageText = pageText
						if len(result.PageText) > 300 {
							result.PageText = result.PageText[:300]
						}
						return nil
					}
					if strings.Contains(lowerText, "payment") && strings.Contains(lowerText, "failed") {
						result.PageText = pageText
						if len(result.PageText) > 300 {
							result.PageText = result.PageText[:300]
						}
						return nil
					}
					if strings.Contains(lowerText, "transaction failed") ||
						strings.Contains(lowerText, "authentication failed") ||
						strings.Contains(lowerText, "access denied") {
						result.PageText = pageText
						if len(result.PageText) > 300 {
							result.PageText = result.PageText[:300]
						}
						return nil
					}
				}

				onPgRouter := strings.Contains(currentURL, "pg_router") || strings.Contains(currentURL, "authenticate")
				if !onPgRouter && pageText != "" {
					if !leftPgRouter {
						leftPgRouter = true
						leftPgRouterTime = time.Now()
					}
					if time.Since(leftPgRouterTime) > 3*time.Second {
						result.PageText = pageText
						if len(result.PageText) > 300 {
							result.PageText = result.PageText[:300]
						}
						return nil
					}
				}

				time.Sleep(500 * time.Millisecond)
			}
			return nil
		}),
	)

	// Read final page text
	if result.PageText == "" {
		var pageText string
		err := chromedp.Run(taskCtx,
			chromedp.Text("body", &pageText, chromedp.ByQuery),
		)
		if err != nil {
			errStr := err.Error()
			if strings.Contains(errStr, "Target closed") || strings.Contains(errStr, "browser has been closed") {
				result.PageClosedEarly = true
			}
		} else {
			result.PageText = strings.TrimSpace(pageText)
			if len(result.PageText) > 300 {
				result.PageText = result.PageText[:300]
			}
			// Check for charged indicators
			lowerText := strings.ToLower(result.PageText)
			if strings.Contains(lowerText, "razorpay_signature") ||
				strings.Contains(lowerText, "payment successful") ||
				strings.Contains(lowerText, "payment_success") {
				result.Charged = true
			}
		}
	}

	if result.PageText != "" {
		fmt.Printf("[RAZ] 3ds page_text=%s\n", result.PageText)
	}
	if result.PageClosedEarly {
		fmt.Printf("[RAZ] 3ds page_closed_early=true\n")
	}

	return result
}

// findChromePath returns the path to Chrome/Chromium on the system.
func findChromePath() string {
	candidates := []string{
		os.Getenv("CHROME_BIN"),
		"/usr/bin/chromium-browser",
		"/usr/bin/chromium",
		"/usr/bin/google-chrome",
		"/usr/bin/google-chrome-stable",
		"/opt/google/chrome/chrome",
		"C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe",
		"C:\\Program Files (x86)\\Google\\Chrome\\Application\\chrome.exe",
	}
	for _, p := range candidates {
		if fileExists(p) {
			return p
		}
	}
	return ""
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}
