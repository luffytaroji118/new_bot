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
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// razorpay3DSResult holds the result of the browser-based 3DS handling.
type razorpay3DSResult struct {
	PageText      string
	PageClosedEarly bool
	Charged       bool
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

// handle3DSRedirect opens the 3DS redirect URL in a headless browser,
// waits for the bank page to load/process, reads the final page text,
// and determines if the payment was charged (frictionless 3DS).
//
// If RAZORPAY_SOLVER_URL is set, it calls the external solver service.
// Otherwise, it runs a local headless Chrome instance.
func handle3DSRedirect(redirectURL, proxyURL string) *razorpay3DSResult {
	// Try external solver service first
	// Note: proxy is NOT passed to the solver — the solver is on Railway
	// and can reach Razorpay directly. The bot's proxy format (e.g. socks4)
	// is often incompatible with Chrome's --proxy-server flag.
	solverURL := os.Getenv("RAZORPAY_SOLVER_URL")
	if solverURL != "" {
		result := solveViaHTTP(solverURL, redirectURL, "")
		if result != nil {
			return result
		}
		// If solver failed, fall through to local browser
		fmt.Printf("[RAZ] solver service unavailable, falling back to local browser\n")
	}

	// Fall back to local browser
	return solveLocal(redirectURL, proxyURL)
}

// solveViaHTTP calls the external solver service via HTTP.
func solveViaHTTP(solverURL, redirectURL, proxyURL string) *razorpay3DSResult {
	reqBody := solverRequest{URL: redirectURL, Proxy: proxyURL}
	jsonBody, _ := json.Marshal(reqBody)

	// Ensure the URL has /solve endpoint
	solveEndpoint := strings.TrimRight(solverURL, "/") + "/solve"

	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("POST", solveEndpoint, bytes.NewReader(jsonBody))
	if err != nil {
		return nil
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("[RAZ] solver HTTP error: %v\n", err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		fmt.Printf("[RAZ] solver HTTP status: %d\n", resp.StatusCode)
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Printf("[RAZ] solver read body error: %v\n", err)
		return nil
	}

	var sr solverResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		fmt.Printf("[RAZ] solver parse error: %v body=%s\n", err, string(body))
		return nil
	}

	result := &razorpay3DSResult{
		PageText:      sr.PageText,
		PageClosedEarly: sr.ClosedEarly,
		Charged:       sr.Charged,
	}

	fmt.Printf("[RAZ] 3ds solver charged=%v closed_early=%v page_text_len=%d\n", result.Charged, result.PageClosedEarly, len(result.PageText))
	if result.PageText != "" {
		fmt.Printf("[RAZ] 3ds solver page_text=%s\n", result.PageText)
	}
	if sr.Error != "" {
		fmt.Printf("[RAZ] 3ds solver error=%s\n", sr.Error)
	}

	return result
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
	browserCtx, cancelBrowser := context.WithTimeout(allocCtx, 25*time.Second)
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

	// Wait for the bank page to process (TDS_WAIT_SECONDS = 10 in Python)
	// The page may auto-redirect, auto-submit, or show an OTP form.
	// Frictionless 3DS will auto-redirect back to Razorpay with a signature.
	waitCtx, cancelWait := context.WithTimeout(taskCtx, 12*time.Second)
	defer cancelWait()

	_ = chromedp.Run(waitCtx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			// Wait for either:
			// 1. The page to change URL (redirect back to Razorpay)
			// 2. A timeout (bank page is showing OTP input)
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
				}

				// Check current URL
				var currentURL string
				_ = chromedp.Run(ctx, chromedp.Location(&currentURL))

				// If redirected away from pg_router, the bank processed it
				if !strings.Contains(currentURL, "pg_router") && !strings.Contains(currentURL, "authenticate") {
					// Redirected to Razorpay callback — check page text
					time.Sleep(500 * time.Millisecond)
					var pageText string
					_ = chromedp.Run(ctx, chromedp.Text("body", &pageText, chromedp.ByQuery))
					result.PageText = strings.TrimSpace(pageText)
					if strings.Contains(strings.ToLower(pageText), "razorpay_signature") ||
						strings.Contains(strings.ToLower(pageText), "payment successful") ||
						strings.Contains(strings.ToLower(pageText), "payment_success") {
						result.Charged = true
					}
					return nil
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
