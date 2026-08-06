// Package icloud is a client for the private web API behind icloud.com.
//
// Apple publishes no iCloud API for Linux: CloudKit Web Services only reaches
// an app's own container, not the user's Drive or photo library. So we speak
// the same endpoints the website does — SRP sign-in on idmsa.apple.com, then
// the drivews/docws/ckdatabasews services handed out by setup.icloud.com.
// These endpoints are undocumented and can change without notice; everything
// version-specific is kept in this file so it is cheap to patch.
package icloud

import (
	"bytes"
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// Client identity of the icloud.com web app.
	widgetKey = "d39ba9916b7251055b22c7f910e2ea796ee65e98b2ddecea8f5dde8d9d1a815d"
	userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 " +
		"(KHTML, like Gecko) Version/17.4 Safari/605.1.15"

	authRoot  = "https://idmsa.apple.com/appleauth/auth"
	setupRoot = "https://setup.icloud.com/setup/ws/1"

	clientBuildNumber     = "2523Project35"
	clientMasteringNumber = "2523B41"
)

// ErrLoginRequired means the stored session is gone or rejected and a full
// interactive login (password + 2FA) is needed.
var ErrLoginRequired = errors.New("icloud: interactive login required")

// Err2FARequired is returned by Login when a two-factor code must be supplied.
var Err2FARequired = errors.New("icloud: two-factor code required")

// APIError describes a non-2xx response from any iCloud endpoint.
type APIError struct {
	Status int
	URL    string
	Body   string
}

func (e *APIError) Error() string {
	body := e.Body
	if len(body) > 400 {
		body = body[:400] + "…"
	}
	return fmt.Sprintf("icloud: %s returned %d: %s", e.URL, e.Status, body)
}

// Retryable reports whether the request may succeed if repeated later.
func (e *APIError) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// Session is the persisted authentication state.
type Session struct {
	AppleID      string            `json:"apple_id"`
	ClientID     string            `json:"client_id"`
	SessionToken string            `json:"session_token"`
	TrustToken   string            `json:"trust_token"`
	TrustedAt    time.Time         `json:"trusted_at,omitzero"`
	Country      string            `json:"account_country"`
	DSID         string            `json:"dsid"`
	Webservices  map[string]string `json:"webservices"`
	Cookies      []storedCookie    `json:"cookies"`

	// Transient, not persisted: only valid during one sign-in exchange.
	sessionID string
	scnt      string
}

// Client talks to iCloud on behalf of one Apple ID.
type Client struct {
	http *http.Client
	jar  *Jar

	mu   sync.Mutex
	sess *Session

	sessionPath string
	logf        func(format string, args ...any)
}

// Option configures a Client.
type Option func(*Client)

// WithLogger installs a log function (defaults to no-op).
func WithLogger(f func(format string, args ...any)) Option {
	return func(c *Client) { c.logf = f }
}

// New creates a client, loading a stored session from sessionPath if present.
func New(sessionPath string, opts ...Option) (*Client, error) {
	jar := NewJar()
	c := &Client{
		jar:         jar,
		sessionPath: sessionPath,
		logf:        func(string, ...any) {},
		sess:        &Session{Webservices: map[string]string{}},
	}
	c.http = &http.Client{
		Jar:     jar,
		Timeout: 0, // per-request contexts / long downloads
		Transport: &http.Transport{
			MaxIdleConnsPerHost:   8,
			ResponseHeaderTimeout: 90 * time.Second,
			ForceAttemptHTTP2:     true,
		},
		// iCloud replies to signin/complete with a redirect we must not follow
		// blindly; everything else is explicit.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
	for _, o := range opts {
		o(c)
	}
	if err := c.loadSession(); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return c, nil
}

// AppleID returns the Apple ID of the current session.
func (c *Client) AppleID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sess.AppleID
}

// SessionInfo returns a copy of the session for display purposes.
func (c *Client) SessionInfo() Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := *c.sess
	s.Cookies = nil
	return s
}

func (c *Client) loadSession() error {
	raw, err := os.ReadFile(c.sessionPath)
	if err != nil {
		return err
	}
	var s Session
	if err := json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("%s: %w", c.sessionPath, err)
	}
	if s.Webservices == nil {
		s.Webservices = map[string]string{}
	}
	c.jar.importCookies(s.Cookies)
	c.sess = &s
	return nil
}

// SaveSession writes the session (0600) so later runs skip the password.
func (c *Client) SaveSession() error {
	c.mu.Lock()
	s := *c.sess
	c.mu.Unlock()

	s.Cookies = c.jar.export()
	raw, err := json.MarshalIndent(&s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.sessionPath), 0o700); err != nil {
		return err
	}
	tmp := c.sessionPath + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.sessionPath)
}

// ReloadSession re-reads the session file, picking up a login performed by
// another process (typically the user running `icloud-synk login` while the
// daemon waits for a new session).
func (c *Client) ReloadSession() error {
	raw, err := os.ReadFile(c.sessionPath)
	if err != nil {
		return err
	}
	var s Session
	if err := json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("%s: %w", c.sessionPath, err)
	}
	if s.Webservices == nil {
		s.Webservices = map[string]string{}
	}
	c.jar.replaceAll(s.Cookies)
	c.mu.Lock()
	c.sess = &s
	c.mu.Unlock()
	return nil
}

// Logout deletes the stored session.
func (c *Client) Logout() error {
	c.mu.Lock()
	c.sess = &Session{Webservices: map[string]string{}}
	c.mu.Unlock()
	c.jar.replaceAll(nil)
	err := os.Remove(c.sessionPath)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// ---------------------------------------------------------------- HTTP helpers

func (c *Client) newRequest(ctx context.Context, method, rawURL string, body []byte) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Origin", "https://www.icloud.com")
	req.Header.Set("Referer", "https://www.icloud.com/")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// authHeaders adds the OAuth/widget headers idmsa.apple.com requires.
func (c *Client) authHeaders(req *http.Request) {
	s := c.sess
	h := req.Header
	h.Set("X-Apple-Widget-Key", widgetKey)
	h.Set("X-Apple-OAuth-Client-Id", widgetKey)
	h.Set("X-Apple-OAuth-Client-Type", "firstPartyAuth")
	h.Set("X-Apple-OAuth-Redirect-URI", "https://www.icloud.com")
	h.Set("X-Apple-OAuth-Require-Grant-Code", "true")
	h.Set("X-Apple-OAuth-Response-Mode", "web_message")
	h.Set("X-Apple-OAuth-Response-Type", "code")
	h.Set("X-Apple-OAuth-State", s.ClientID)
	h.Set("X-Apple-Domain-Id", "1")
	if s.sessionID != "" {
		h.Set("X-Apple-ID-Session-Id", s.sessionID)
	}
	if s.scnt != "" {
		h.Set("scnt", s.scnt)
	}
}

// captureAuthHeaders picks up the rolling session identifiers Apple returns.
func (c *Client) captureAuthHeaders(resp *http.Response) {
	if v := resp.Header.Get("X-Apple-ID-Session-Id"); v != "" {
		c.sess.sessionID = v
	}
	if v := resp.Header.Get("scnt"); v != "" {
		c.sess.scnt = v
	}
	if v := resp.Header.Get("X-Apple-Session-Token"); v != "" {
		c.sess.SessionToken = v
	}
	if v := resp.Header.Get("X-Apple-ID-Account-Country"); v != "" {
		c.sess.Country = v
	}
	if v := resp.Header.Get("X-Apple-Twosv-Trust-Token"); v != "" {
		c.sess.TrustToken = v
		c.sess.TrustedAt = time.Now()
	}
}

// do performs a request, returning the body. Non-2xx becomes an *APIError.
func (c *Client) do(req *http.Request) ([]byte, *http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if readErr != nil {
		return nil, resp, readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return body, resp, &APIError{Status: resp.StatusCode, URL: req.URL.String(), Body: string(body)}
	}
	return body, resp, nil
}

// postJSON issues an authenticated POST against a service endpoint (drivews,
// docws, ckdatabasews) with the dsid and client identifiers Apple expects.
func (c *Client) postJSON(ctx context.Context, rawURL string, params url.Values, payload, out any) error {
	return c.serviceJSON(ctx, http.MethodPost, rawURL, params, payload, out)
}

func (c *Client) serviceJSON(ctx context.Context, method, rawURL string, params url.Values, payload, out any) error {
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return err
		}
	}
	full, err := c.serviceURL(rawURL, params)
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, method, full, body)
	if err != nil {
		return err
	}
	respBody, _, err := c.do(req)
	if err != nil {
		if ae, ok := err.(*APIError); ok && (ae.Status == 401 || ae.Status == 421) {
			return fmt.Errorf("%w (%v)", ErrLoginRequired, ae.Status)
		}
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("%s: decoding response: %w", rawURL, err)
	}
	return nil
}

// serviceURL appends the standard query parameters to a service URL.
func (c *Client) serviceURL(rawURL string, params url.Values) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	for k, vs := range params {
		for _, v := range vs {
			q.Set(k, v)
		}
	}
	c.mu.Lock()
	dsid, clientID := c.sess.DSID, c.sess.ClientID
	c.mu.Unlock()
	if dsid != "" {
		q.Set("dsid", dsid)
	}
	q.Set("clientBuildNumber", clientBuildNumber)
	q.Set("clientMasteringNumber", clientMasteringNumber)
	q.Set("clientId", strings.TrimPrefix(clientID, "auth-"))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// service returns the root URL of a named web service.
func (c *Client) service(name string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	u := c.sess.Webservices[name]
	if u == "" {
		return "", fmt.Errorf("icloud: service %q is not available for this account", name)
	}
	return u, nil
}

// ------------------------------------------------------------------- Login

// PhoneNumber is a trusted phone number that can receive a code by SMS.
type PhoneNumber struct {
	ID     int    `json:"id"`
	Masked string `json:"numberWithDialCode"`
	Mode   string `json:"pushMode"`
}

// Challenge describes a pending two-factor request: how many trusted devices
// were pushed to, and which phone numbers can be used instead. Apple only
// shows the code on a trusted device after the user taps "Allow", so knowing
// what is available is the difference between waiting and being stuck.
type Challenge struct {
	TrustedDeviceCount int           `json:"trustedDeviceCount"`
	PhoneNumbers       []PhoneNumber `json:"trustedPhoneNumbers"`
	CodeLength         int           `json:"-"`

	SecurityCode struct {
		Length           int  `json:"length"`
		TooManyCodesSent bool `json:"tooManyCodesSent"`
		TooManyCodesLock bool `json:"tooManyCodesLock"`
	} `json:"securityCode"`

	// set when the code was requested over SMS, so verification uses the
	// phone endpoint rather than the trusted-device one.
	smsID int
	cl    *Client
	ctx   context.Context
}

// SendSMS asks Apple to text the code to one of the trusted numbers. Pass the
// ID from PhoneNumbers (the first one if unsure).
func (ch *Challenge) SendSMS(id int) error {
	if ch == nil || ch.cl == nil {
		return errors.New("icloud: no pending two-factor challenge")
	}
	payload := map[string]any{
		"phoneNumber": map[string]any{"id": id},
		"mode":        "sms",
	}
	if err := ch.cl.authJSON(ch.ctx, http.MethodPut, authRoot+"/verify/phone", nil, payload, nil); err != nil {
		return fmt.Errorf("requesting an SMS code: %w", err)
	}
	ch.smsID = id
	return nil
}

// ResendPush re-pushes the prompt to the trusted devices.
func (ch *Challenge) ResendPush() error {
	if ch == nil || ch.cl == nil {
		return errors.New("icloud: no pending two-factor challenge")
	}
	ch.smsID = 0
	if err := ch.cl.authJSON(ch.ctx, http.MethodGet, authRoot+"/verify/trusteddevice", nil, nil, nil); err != nil {
		return fmt.Errorf("re-sending the code to trusted devices: %w", err)
	}
	return nil
}

// fetchChallenge asks Apple what the pending two-factor request looks like.
func (c *Client) fetchChallenge(ctx context.Context) *Challenge {
	ch := &Challenge{cl: c, ctx: ctx}
	if err := c.authJSON(ctx, http.MethodGet, authRoot, nil, nil, ch); err != nil {
		c.logf("could not read the two-factor challenge details: %v", err)
	}
	ch.CodeLength = ch.SecurityCode.Length
	if ch.CodeLength == 0 {
		ch.CodeLength = 6
	}
	return ch
}

// Login performs the full SRP sign-in. If Apple asks for a two-factor code,
// codeFn is called with the pending challenge to obtain it; passing a nil
// codeFn yields Err2FARequired.
func (c *Client) Login(ctx context.Context, appleID, password string, codeFn func(*Challenge) (string, error)) error {
	c.mu.Lock()
	if c.sess.ClientID == "" || !strings.EqualFold(c.sess.AppleID, appleID) {
		c.sess = &Session{Webservices: map[string]string{}}
		c.sess.ClientID = "auth-" + newUUID()
	}
	c.sess.AppleID = appleID
	c.mu.Unlock()

	srp, err := newSRPClient(appleID)
	if err != nil {
		return err
	}

	// Step 1: ask for the salt and the server's public value.
	initReq := map[string]any{
		"a":           base64.StdEncoding.EncodeToString(srp.A.Bytes()),
		"accountName": appleID,
		"protocols":   []string{"s2k", "s2k_fo"},
	}
	var initResp struct {
		Iteration int    `json:"iteration"`
		Salt      string `json:"salt"`
		Protocol  string `json:"protocol"`
		B         string `json:"b"`
		C         string `json:"c"`
	}
	if err := c.authJSON(ctx, http.MethodPost, authRoot+"/signin/init", nil, initReq, &initResp); err != nil {
		return fmt.Errorf("signin/init: %w", err)
	}

	salt, err := base64.StdEncoding.DecodeString(initResp.Salt)
	if err != nil {
		return fmt.Errorf("signin/init: bad salt: %w", err)
	}
	bBytes, err := base64.StdEncoding.DecodeString(initResp.B)
	if err != nil {
		return fmt.Errorf("signin/init: bad b: %w", err)
	}

	passKey := derivePasswordKey(password, salt, initResp.Iteration, initResp.Protocol)
	m1, m2, err := srp.proof(salt, bBytes, passKey)
	if err != nil {
		return err
	}

	// Step 2: prove knowledge of the password.
	completeReq := map[string]any{
		"accountName": appleID,
		"c":           initResp.C,
		"m1":          base64.StdEncoding.EncodeToString(m1),
		"m2":          base64.StdEncoding.EncodeToString(m2),
		"rememberMe":  true,
	}
	c.mu.Lock()
	if c.sess.TrustToken != "" {
		completeReq["trustTokens"] = []string{c.sess.TrustToken}
	} else {
		completeReq["trustTokens"] = []string{}
	}
	c.mu.Unlock()

	params := url.Values{"isRememberMeEnabled": {"true"}}
	err = c.authJSON(ctx, http.MethodPost, authRoot+"/signin/complete", params, completeReq, nil)
	needs2FA := false
	if err != nil {
		ae, ok := err.(*APIError)
		switch {
		case ok && ae.Status == http.StatusConflict: // 409 — two-factor challenge
			needs2FA = true
		case ok && (ae.Status == 401 || ae.Status == 403):
			return fmt.Errorf("wrong Apple ID or password (%d)", ae.Status)
		default:
			return fmt.Errorf("signin/complete: %w", err)
		}
	}

	if needs2FA {
		if codeFn == nil {
			return Err2FARequired
		}
		ch := c.fetchChallenge(ctx)
		code, err := codeFn(ch)
		if err != nil {
			return err
		}
		if err := c.submit2FA(ctx, code, ch); err != nil {
			return err
		}
		if err := c.trustSession(ctx); err != nil {
			return err
		}
	}

	if err := c.accountLogin(ctx); err != nil {
		return err
	}
	return c.SaveSession()
}

// derivePasswordKey turns the password into the SRP "password" per Apple's
// s2k / s2k_fo schemes.
func derivePasswordKey(password string, salt []byte, iterations int, protocol string) []byte {
	sum := sha256.Sum256([]byte(password))
	pw := sum[:]
	if protocol == "s2k_fo" {
		pw = []byte(hex.EncodeToString(sum[:]))
	}
	if iterations <= 0 {
		iterations = 20 // never happens in practice; avoid a zero-round PBKDF2
	}
	key, err := pbkdf2.Key(sha256.New, string(pw), salt, iterations, 32)
	if err != nil {
		// pbkdf2.Key only fails on absurd parameters.
		panic(fmt.Sprintf("pbkdf2: %v", err))
	}
	return key
}

// authJSON performs a request against idmsa.apple.com.
func (c *Client) authJSON(ctx context.Context, method, rawURL string, params url.Values, payload, out any) error {
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return err
		}
	}
	if len(params) > 0 {
		rawURL += "?" + params.Encode()
	}
	req, err := c.newRequest(ctx, method, rawURL, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	c.mu.Lock()
	c.authHeaders(req)
	c.mu.Unlock()

	respBody, resp, err := c.do(req)
	if resp != nil {
		c.mu.Lock()
		c.captureAuthHeaders(resp)
		c.mu.Unlock()
	}
	if err != nil {
		return err
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("%s: decoding response: %w", rawURL, err)
		}
	}
	return nil
}

func (c *Client) submit2FA(ctx context.Context, code string, ch *Challenge) error {
	code = strings.TrimSpace(code)
	if code == "" {
		return errors.New("empty two-factor code")
	}

	// A code requested over SMS must be verified through the phone endpoint;
	// the trusted-device endpoint rejects it.
	if ch != nil && ch.smsID != 0 {
		payload := map[string]any{
			"securityCode": map[string]string{"code": code},
			"phoneNumber":  map[string]any{"id": ch.smsID},
			"mode":         "sms",
		}
		err := c.authJSON(ctx, http.MethodPost, authRoot+"/verify/phone/securitycode", nil, payload, nil)
		if err == nil {
			return nil
		}
		if ae, ok := err.(*APIError); ok && ae.Status == 400 {
			return errors.New("iCloud rejected the code — it may have expired, request a new one")
		}
		return fmt.Errorf("verifying the SMS code: %w", err)
	}

	payload := map[string]any{
		"securityCode": map[string]string{"code": code},
	}
	err := c.authJSON(ctx, http.MethodPost, authRoot+"/verify/trusteddevice/securitycode", nil, payload, nil)
	if err == nil {
		return nil
	}
	if ae, ok := err.(*APIError); ok && ae.Status == 400 {
		return errors.New("iCloud rejected the code — it may have expired, request a new one")
	}
	return fmt.Errorf("verifying the two-factor code: %w", err)
}

// trustSession exchanges the verified session for a long-lived trust token so
// subsequent runs need neither password nor code.
func (c *Client) trustSession(ctx context.Context) error {
	if err := c.authJSON(ctx, http.MethodGet, authRoot+"/2sv/trust", nil, nil, nil); err != nil {
		return fmt.Errorf("trusting session: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sess.TrustToken == "" {
		return errors.New("iCloud did not return a trust token")
	}
	return nil
}

// accountLogin trades the session token for the dsid and service URLs.
func (c *Client) accountLogin(ctx context.Context) error {
	c.mu.Lock()
	payload := map[string]any{
		"accountCountryCode": c.sess.Country,
		"dsWebAuthToken":     c.sess.SessionToken,
		"extended_login":     true,
	}
	if c.sess.TrustToken != "" {
		payload["trustToken"] = c.sess.TrustToken
	}
	c.mu.Unlock()

	var resp struct {
		DSInfo struct {
			DSID     string `json:"dsid"`
			FullName string `json:"fullName"`
		} `json:"dsInfo"`
		Webservices map[string]struct {
			URL    string `json:"url"`
			Status string `json:"status"`
		} `json:"webservices"`
		HSAChallengeRequired bool `json:"hsaChallengeRequired"`
		HSATrustedBrowser    bool `json:"hsaTrustedBrowser"`
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	u, err := c.serviceURL(setupRoot+"/accountLogin", nil)
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, http.MethodPost, u, body)
	if err != nil {
		return err
	}
	raw, _, err := c.do(req)
	if err != nil {
		if ae, ok := err.(*APIError); ok && (ae.Status == 401 || ae.Status == 421 || ae.Status == 450) {
			return fmt.Errorf("%w (accountLogin %d)", ErrLoginRequired, ae.Status)
		}
		return fmt.Errorf("accountLogin: %w", err)
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("accountLogin: decoding response: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.sess.DSID = resp.DSInfo.DSID
	if c.sess.Webservices == nil {
		c.sess.Webservices = map[string]string{}
	}
	for name, svc := range resp.Webservices {
		if svc.URL != "" && (svc.Status == "" || svc.Status == "active") {
			c.sess.Webservices[name] = strings.TrimSuffix(svc.URL, "/")
		}
	}
	if c.sess.DSID == "" {
		return errors.New("accountLogin: no dsid in response")
	}
	c.logf("signed in as %s (dsid %s), %d services available",
		c.sess.AppleID, c.sess.DSID, len(c.sess.Webservices))
	if resp.HSAChallengeRequired && !resp.HSATrustedBrowser {
		return fmt.Errorf("%w: iCloud wants a new two-factor confirmation", ErrLoginRequired)
	}
	return nil
}

// Authenticate makes sure the client has a usable session, reusing the stored
// one when possible. It returns ErrLoginRequired if a password is needed.
func (c *Client) Authenticate(ctx context.Context) error {
	c.mu.Lock()
	hasSession := c.sess.SessionToken != "" || c.jar.value("X-APPLE-WEBAUTH-TOKEN") != ""
	hasServices := len(c.sess.Webservices) > 0 && c.sess.DSID != ""
	c.mu.Unlock()

	if !hasSession {
		return ErrLoginRequired
	}
	if hasServices && c.validate(ctx) == nil {
		return nil
	}
	// Cookies alone were not enough — replay accountLogin with the trust token.
	if err := c.accountLogin(ctx); err != nil {
		return err
	}
	return c.SaveSession()
}

// validate is a cheap check that the current cookies still work.
func (c *Client) validate(ctx context.Context) error {
	u, err := c.serviceURL(setupRoot+"/validate", nil)
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	_, _, err = c.do(req)
	return err
}

// TrustExpiry estimates when the trust token stops working (Apple grants
// roughly 30 days).
func (c *Client) TrustExpiry() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sess.TrustToken == "" || c.sess.TrustedAt.IsZero() {
		return time.Time{}, false
	}
	return c.sess.TrustedAt.Add(30 * 24 * time.Hour), true
}

// newUUID returns a random RFC 4122 v4 UUID in upper case, as icloud.com does.
func newUUID() string {
	var b [16]byte
	rand.Read(b[:]) // crypto/rand.Read never fails on Linux
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return strings.ToUpper(fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]))
}
