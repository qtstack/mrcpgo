package tencent

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"
)

// asrSentence is one entry of sentences.sentence_list.
type asrSentence struct {
	Sentence     string `json:"sentence"`
	SentenceType int    `json:"sentence_type"`
	SentenceID   int32  `json:"sentence_id"`
	SpeakerID    int32  `json:"speaker_id"`
	StartTime    uint32 `json:"start_time"`
	EndTime      uint32 `json:"end_time"`
}

// asrResponse is a message from the recognition WebSocket.
type asrResponse struct {
	Code      int    `json:"code"`
	Message   string `json:"message"`
	VoiceID   string `json:"voice_id"`
	MessageID string `json:"message_id"`
	Final     int    `json:"final"`
	Sentences *struct {
		SentenceList []asrSentence `json:"sentence_list"`
	} `json:"sentences"`
}

// ttsResponse is a text message from the synthesis WebSocket.
type ttsResponse struct {
	Code      int    `json:"code"`
	Message   string `json:"message"`
	SessionID string `json:"session_id"`
	RequestID string `json:"request_id"`
	MessageID string `json:"message_id"`
	Final     int    `json:"final"`
	Ready     int    `json:"ready"`
	Heartbeat int    `json:"heartbeat"`
}

// error converts a failed response into an error.
func (r *asrResponse) err() error {
	return fmt.Errorf("tencent: %s (code %d, message %s)", r.Message, r.Code, r.MessageID)
}

func (r *ttsResponse) err() error {
	return fmt.Errorf("tencent: %s (code %d, message %s)", r.Message, r.Code, r.MessageID)
}

// actionMessage is a JSON command sent to the streaming synthesis service.
type actionMessage struct {
	SessionID string `json:"session_id"`
	MessageID string `json:"message_id"`
	Action    string `json:"action"`
	Data      string `json:"data"`
}

// End of stream marker sent to the recognition service.
const asrEndMessage = `{"type":"end"}`

// Synthesis actions.
const (
	actionSynthesis = "ACTION_SYNTHESIS"
	actionComplete  = "ACTION_COMPLETE"
)

// newID returns a 32 character identifier suitable for voice_id, session_id and
// message_id fields.
func newID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return hex.EncodeToString(buf[:])
}
