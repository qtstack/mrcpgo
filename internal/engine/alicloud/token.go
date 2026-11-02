package alicloud

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qtstack/mrcpgo/internal/config"
)

// tokenAPIVersion is the NLS meta API version used by CreateToken.
const tokenAPIVersion = "2019-02-28"

// TokenSource obtains and caches the short lived NLS token. Alibaba Cloud
// issues tokens valid for roughly 24 hours; they are refreshed ahead of expiry.
type TokenSource struct {
	cfg    config.AliCloud
	log    *slog.Logger
	client *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

// NewTokenSource creates a token source. When the configuration carries a
// static token, it is returned unchanged and no request is ever made.
func NewTokenSource(cfg config.AliCloud, log *slog.Logger) *TokenSource {
	if log == nil {
		log = slog.Default()
	}
	return &TokenSource{
		cfg: cfg,
		log: log.With("component", "alicloud-token"),
		client: &http.Client{
			Timeout: cfg.Timeout.Value(),
		},
	}
}

// Token returns a usable NLS token, fetching or refreshing it when needed.
func (t *TokenSource) Token(ctx context.Context) (string, error) {
	if t.cfg.Token != "" {
		return t.cfg.Token, nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	if t.token != "" && now.Add(t.cfg.TokenRefreshBefore.Value()).Before(t.expires) {
		return t.token, nil
	}

	token, expires, err := t.fetch(ctx)
	if err != nil {
		// Keep serving the previous token while it is still valid, so a
		// transient token service outage does not break new calls.
		if t.token != "" && now.Before(t.expires) {
			t.log.Warn("token refresh failed, using cached token", "error", err)
			return t.token, nil
		}
		return "", err
	}
	t.token = token
	t.expires = expires
	t.log.Info("NLS token obtained", "expires", expires.Format(time.RFC3339))
	return token, nil
}

// fetch calls CreateToken and returns the token and its expiry.
func (t *TokenSource) fetch(ctx context.Context) (string, time.Time, error) {
	params := map[string]string{
		"AccessKeyId":      t.cfg.AccessKeyID,
		"Action":           "CreateToken",
		"Format":           "JSON",
		"RegionId":         t.cfg.Region,
		"SignatureMethod":  "HMAC-SHA1",
		"SignatureNonce":   randomNonce(),
		"SignatureVersion": "1.0",
		"Timestamp":        time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"Version":          tokenAPIVersion,
	}
	canonical := canonicalQuery(params)
	signature := signRPC(t.cfg.AccessKeySecret, "POST", canonical)
	body := canonical + "&Signature=" + percentEncode(signature)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint(t.cfg), strings.NewReader(body))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("alicloud: create token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := t.client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("alicloud: create token: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("alicloud: read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("alicloud: token service returned %s: %s",
			resp.Status, strings.TrimSpace(string(raw)))
	}

	var out struct {
		Token struct {
			ID         string `json:"Id"`
			ExpireTime int64  `json:"ExpireTime"`
		} `json:"Token"`
		ErrMsg    string `json:"ErrMsg"`
		Code      string `json:"Code"`
		Message   string `json:"Message"`
		RequestID string `json:"RequestId"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", time.Time{}, fmt.Errorf("alicloud: decode token response: %w", err)
	}
	if out.Token.ID == "" {
		detail := out.ErrMsg
		if detail == "" {
			detail = out.Message
		}
		if detail == "" {
			detail = out.Code
		}
		return "", time.Time{}, fmt.Errorf("alicloud: no token returned (request %s): %s", out.RequestID, detail)
	}

	expires := time.Unix(out.Token.ExpireTime, 0)
	if out.Token.ExpireTime == 0 {
		expires = time.Now().Add(10 * time.Minute)
	}
	return out.Token.ID, expires, nil
}

// tokenEndpoint returns the CreateToken URL.
func tokenEndpoint(cfg config.AliCloud) string {
	if cfg.TokenEndpoint != "" {
		return cfg.TokenEndpoint
	}
	return "https://nls-meta." + cfg.Region + ".aliyuncs.com"
}

// signRPC implements the Alibaba Cloud RPC signature (HMAC-SHA1) used by the
// NLS meta API.
func signRPC(secret, method, canonicalQuery string) string {
	stringToSign := method + "&" + percentEncode("/") + "&" + percentEncode(canonicalQuery)
	mac := hmac.New(sha1.New, []byte(secret+"&"))
	mac.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// canonicalQuery percent encodes and sorts the parameters as the RPC signature
// requires.
func canonicalQuery(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for key := range params {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var b strings.Builder
	for i, key := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(percentEncode(key))
		b.WriteByte('=')
		b.WriteString(percentEncode(params[key]))
	}
	return b.String()
}

// percentEncode applies RFC 3986 encoding as required by the Alibaba Cloud
// signature algorithm.
func percentEncode(value string) string {
	encoded := url.QueryEscape(value)
	encoded = strings.ReplaceAll(encoded, "+", "%20")
	encoded = strings.ReplaceAll(encoded, "*", "%2A")
	encoded = strings.ReplaceAll(encoded, "%7E", "~")
	return encoded
}

// randomNonce returns a 32 character hexadecimal nonce.
func randomNonce() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

// newID returns a 32 character identifier accepted in message_id and task_id
// fields.
func newID() string { return randomNonce() }
