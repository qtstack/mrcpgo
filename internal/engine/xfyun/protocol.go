package xfyun

import (
	"strings"
)

// asrResponse is a text message from the rtasr WebSocket.
type asrResponse struct {
	Action string `json:"action"`
	Code   string `json:"code"`
	Data   string `json:"data"`
	Desc   string `json:"desc"`
	Sid    string `json:"sid"`
}

// asrResult is the JSON document carried in asrResponse.Data.
type asrResult struct {
	CN *struct {
		ST *struct {
			RT []struct {
				WS []struct {
					CW []struct {
						W string `json:"w"`
					} `json:"cw"`
				} `json:"ws"`
			} `json:"rt"`
			// Type is "0" for a final result and "1" for an interim result.
			Type string `json:"type"`
		} `json:"st"`
	} `json:"cn"`
	SegID int `json:"seg_id"`
}

// text concatenates the words of the segment.
func (r *asrResult) text() string {
	if r.CN == nil || r.CN.ST == nil {
		return ""
	}
	var b strings.Builder
	for _, segment := range r.CN.ST.RT {
		for _, word := range segment.WS {
			if len(word.CW) > 0 {
				b.WriteString(word.CW[0].W)
			}
		}
	}
	return b.String()
}

// final reports whether the segment is a final result.
func (r *asrResult) final() bool {
	return r.CN != nil && r.CN.ST != nil && r.CN.ST.Type == "0"
}

// ttsResponse is a text message from the synthesis WebSocket.
type ttsResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Sid     string `json:"sid"`
	Data    struct {
		Audio  string `json:"audio"`
		Status int    `json:"status"`
		Ced    string `json:"ced"`
	} `json:"data"`
}

// ttsRequest is the single frame sent to the synthesis service.
type ttsRequest struct {
	Common   ttsCommon      `json:"common"`
	Business map[string]any `json:"business"`
	Data     ttsData        `json:"data"`
}

type ttsCommon struct {
	AppID string `json:"app_id"`
}

type ttsData struct {
	Status   int    `json:"status"`
	Text     string `json:"text"`
	Encoding string `json:"encoding"`
}
