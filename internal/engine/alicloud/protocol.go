// Package alicloud implements the Alibaba Cloud Intelligent Speech Interaction
// (NLS) engine: real-time speech recognition and speech synthesis over the NLS
// WebSocket protocol.
//
// The wire format follows the protocol used by the official
// github.com/aliyun/alibabacloud-nls-go-sdk: JSON control frames with a
// header/payload/context envelope, and raw binary PCM frames for audio.
package alicloud

import (
	"encoding/json"
	"fmt"
	"math"
)

// Protocol namespaces and message names.
const (
	nsASR = "SpeechRecognizer"
	nsTTS = "SpeechSynthesizer"
	// nsTTSLong is used for long text synthesis.
	nsTTSLong = "SpeechLongSynthesizer"

	nameStartRecognition = "StartRecognition"
	nameStopRecognition  = "StopRecognition"

	nameRecognitionStarted       = "RecognitionStarted"
	nameRecognitionResultChanged = "RecognitionResultChanged"
	nameRecognitionCompleted     = "RecognitionCompleted"

	nameStartSynthesis     = "StartSynthesis"
	nameSynthesisCompleted = "SynthesisCompleted"
	nameMetaInfo           = "MetaInfo"

	nameTaskFailed = "TaskFailed"
)

const (
	// headerToken carries the NLS token during the WebSocket handshake.
	headerToken = "X-NLS-Token"
	// statusSuccess is the NLS application level success status.
	statusSuccess = 20000000
)

// messageHeader is the header of a control frame.
type messageHeader struct {
	AppKey    string `json:"appkey,omitempty"`
	MessageID string `json:"message_id,omitempty"`
	TaskID    string `json:"task_id,omitempty"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// messageContext is optional client metadata accepted by the service.
type messageContext struct {
	SDK struct {
		Name     string `json:"name"`
		Version  string `json:"version"`
		Language string `json:"language"`
	} `json:"sdk"`
}

// request is a control frame sent to the service.
type request struct {
	Header  messageHeader  `json:"header"`
	Payload map[string]any `json:"payload,omitempty"`
	Context messageContext `json:"context"`
}

// response is a control frame received from the service.
type response struct {
	Header struct {
		Name       string `json:"name"`
		Namespace  string `json:"namespace"`
		MessageID  string `json:"message_id"`
		TaskID     string `json:"task_id"`
		Status     int    `json:"status"`
		StatusText string `json:"status_text"`
	} `json:"header"`
	Payload map[string]any `json:"payload"`
}

func newRequest(namespace, name, appKey, taskID string, payload map[string]any) *request {
	req := &request{
		Header: messageHeader{
			AppKey:    appKey,
			MessageID: newID(),
			TaskID:    taskID,
			Namespace: namespace,
			Name:      name,
		},
		Payload: payload,
	}
	req.Context.SDK.Name = "mrcpgo"
	req.Context.SDK.Version = "0.1"
	req.Context.SDK.Language = "go"
	return req
}

func parseResponse(data []byte) (*response, error) {
	var resp response
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("alicloud: decode control frame: %w", err)
	}
	if resp.Header.Name == "" {
		return nil, fmt.Errorf("alicloud: control frame without header.name")
	}
	return &resp, nil
}

// ok reports whether the frame carries a success status.
func (r *response) ok() bool {
	return r.Header.Status == 0 || r.Header.Status == statusSuccess
}

// err converts a failed frame into an error.
func (r *response) err() error {
	text := r.Header.StatusText
	if text == "" {
		text = "request failed"
	}
	return fmt.Errorf("alicloud: %s (status %d, message %s)", text, r.Header.Status, r.Header.MessageID)
}

// result returns the recognised text carried by the payload.
func (r *response) result() string {
	if r.Payload == nil {
		return ""
	}
	text, _ := r.Payload["result"].(string)
	return text
}

// confidence converts the payload confidence to the 0-100 NLSML scale. The
// service may omit it, in which case -1 tells the NLSML builder to omit the
// attribute rather than fabricate a score. The service has been seen using both
// a 0-1 and a 0-100 scale.
func (r *response) confidence() int {
	if r.Payload == nil {
		return -1
	}
	raw, ok := r.Payload["confidence"].(float64)
	if !ok {
		return -1
	}
	if raw <= 1 {
		raw *= 100
	}
	return int(math.Round(raw))
}

// instance renders the payload as the NLSML instance element.
func (r *response) instance() string {
	if len(r.Payload) == 0 {
		return ""
	}
	encoded, err := json.Marshal(r.Payload)
	if err != nil {
		return ""
	}
	return string(encoded)
}
