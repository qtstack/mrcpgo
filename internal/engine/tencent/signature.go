// Package tencent implements Tencent Cloud speech engines: real-time speech
// recognition v2 and streaming speech synthesis, both over WebSocket.
//
// The wire protocol follows the Tencent Cloud documentation:
//   - ASR: wss://asr.cloud.tencent.com/asr/v2/<appid> with HMAC-SHA1 signed
//     query parameters, binary PCM audio and JSON results carrying
//     sentences.sentence_list.
//   - TTS: wss://tts.cloud.tencent.com/stream_wsv2 with the
//     TextToStreamAudioWSv2 action, ACTION_SYNTHESIS / ACTION_COMPLETE JSON
//     commands and binary PCM audio frames.
package tencent

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// cannonicalQuery renders the sorted, unencoded "key=value" list that Tencent
// signs. Values are used verbatim.
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
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(params[key])
	}
	return b.String()
}

// encodedQuery renders the sorted, percent encoded query string used on the
// wire. Tencent decodes the parameters before verifying the signature, which is
// computed over canonicalQuery.
func encodedQuery(params map[string]string) string {
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
		b.WriteString(url.QueryEscape(key))
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(params[key]))
	}
	return b.String()
}

// signQuery returns the canonical query and its Base64 HMAC-SHA1 signature.
// The signed string is "<method><host><path>?<canonical query>", where method is
// "GET" for the TTS API and empty for the ASR API.
func signQuery(method, hostPath string, params map[string]string, secretKey string) (canonical, signature string) {
	canonical = canonicalQuery(params)
	mac := hmac.New(sha1.New, []byte(secretKey))
	mac.Write([]byte(method + hostPath + "?" + canonical))
	return canonical, base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// buildWebSocketURL signs params and assembles the dial URL.
//
// asrEndpoint is the base WebSocket URL without query, for example
// "wss://asr.cloud.tencent.com/asr/v2/125922". method is "" for ASR and "GET"
// for TTS, and signatureParam is "signature" or "Signature" respectively.
func buildWebSocketURL(asrEndpoint, method, signatureParam string, params map[string]string, secretKey string) (string, error) {
	u, err := url.Parse(asrEndpoint)
	if err != nil {
		return "", fmt.Errorf("tencent: parse endpoint: %w", err)
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return "", fmt.Errorf("tencent: endpoint %q must use ws or wss", asrEndpoint)
	}
	hostPath := u.Host + u.Path
	_, signature := signQuery(method, hostPath, params, secretKey)

	return fmt.Sprintf("%s://%s%s?%s&%s=%s",
		u.Scheme, u.Host, u.Path, encodedQuery(params), signatureParam, url.QueryEscape(signature)), nil
}
