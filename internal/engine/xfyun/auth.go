// Package xfyun implements iFlytek (讯飞) speech engines: real-time
// transcription (rtasr) and online speech synthesis.
//
// Both services use WebSocket with URL based signatures:
//   - ASR: wss://rtasr.xfyun.cn/v1/ws?appid=..&ts=..&signa=.., where
//     signa = Base64(HMAC-SHA1(MD5(appid+ts), apiKey)).
//   - TTS: wss://tts-api.xfyun.cn/v2/tts?authorization=..&date=..&host=..,
//     where the request is signed with HMAC-SHA256 over
//     "host: ..\ndate: ..\nGET /v2/tts HTTP/1.1".
package xfyun

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/qtstack/mrcpgo/internal/config"
)

// asrSigna computes the rtasr handshake signature:
// Base64(HMAC-SHA1(MD5(appID + ts), apiKey)).
func asrSigna(appID, ts, apiKey string) string {
	digest := md5.Sum([]byte(appID + ts))
	mac := hmac.New(sha1.New, []byte(apiKey))
	mac.Write([]byte(hex.EncodeToString(digest[:])))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// asrURL builds the transcription handshake URL.
func asrURL(cfg config.Xfyun) string {
	endpoint := cfg.ASREndpoint
	if endpoint == "" {
		endpoint = "wss://rtasr.xfyun.cn/v1/ws"
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	params := url.Values{}
	params.Set("appid", cfg.AppID)
	params.Set("ts", ts)
	params.Set("signa", asrSigna(cfg.AppID, ts, cfg.APIKey))
	if cfg.ASR.Lang != "" {
		params.Set("lang", cfg.ASR.Lang)
	}
	for key, value := range cfg.ASR.Params {
		params.Set(key, value)
	}
	return endpoint + "?" + params.Encode()
}

// ttsSignature signs the TTS handshake string with HMAC-SHA256.
func ttsSignature(host, path, date, apiSecret string) string {
	origin := "host: " + host + "\ndate: " + date + "\nGET " + path + " HTTP/1.1"
	mac := hmac.New(sha256.New, []byte(apiSecret))
	mac.Write([]byte(origin))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// buildTTSURL assembles the authorised synthesis handshake URL. now is a
// parameter so the signature can be tested deterministically.
func buildTTSURL(endpoint, apiKey, apiSecret string, now time.Time) (string, error) {
	if endpoint == "" {
		endpoint = "wss://tts-api.xfyun.cn/v2/tts"
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("xfyun: parse endpoint: %w", err)
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return "", fmt.Errorf("xfyun: endpoint %q must use ws or wss", endpoint)
	}

	date := now.UTC().Format(http.TimeFormat)
	signature := ttsSignature(u.Host, u.Path, date, apiSecret)
	authorizationOrigin := fmt.Sprintf(
		`api_key="%s",algorithm="hmac-sha256",headers="host date request-line",signature="%s"`,
		apiKey, signature)
	authorization := base64.StdEncoding.EncodeToString([]byte(authorizationOrigin))

	params := url.Values{}
	params.Set("authorization", authorization)
	params.Set("date", date)
	params.Set("host", u.Host)
	return endpoint + "?" + params.Encode(), nil
}
