// Package config loads and validates the server configuration.
package config

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration document.
type Config struct {
	Log  Log  `yaml:"log"`
	SIP  SIP  `yaml:"sip"`
	RTP  RTP  `yaml:"rtp"`
	MRCP MRCP `yaml:"mrcp"`
	HTTP HTTP `yaml:"http"`
}

// Log configures structured logging.
type Log struct {
	// Level is one of debug, info, warn or error.
	Level string `yaml:"level"`
	// Format is "text" or "json".
	Format string `yaml:"format"`
}

// SIP configures the SIP user agent.
type SIP struct {
	// Listen is the UDP listen address, for example "0.0.0.0:5060".
	Listen string `yaml:"listen"`
	// AdvertiseAddr overrides the address written into Contact and SDP. When
	// empty the address of the interface used to reach the peer is used.
	AdvertiseAddr string `yaml:"advertise_addr"`
	// UserAgent is the User-Agent header value.
	UserAgent string `yaml:"user_agent"`
	// ContactUser is the user part of the Contact URI.
	ContactUser string `yaml:"contact_user"`
	// ServerName is the s= line of generated SDP.
	ServerName string `yaml:"server_name"`
}

// RTP configures the media listeners.
type RTP struct {
	// ListenHost is the local address RTP sockets bind to. Wildcard is the
	// default and is correct for a single homed host.
	ListenHost string `yaml:"listen_host"`
}

// MRCP configures the control channels and engines.
type MRCP struct {
	// ListenHost is the local address control listeners bind to.
	ListenHost string `yaml:"listen_host"`
	// Engine selects and configures the speech engine implementation.
	Engine Engine `yaml:"engine"`
}

// Engine configures the speech engine.
type Engine struct {
	// Type is the engine implementation: "mock", "alicloud", "tencent" or
	// "xfyun".
	Type string `yaml:"type"`
	// RecognitionDelay is the simulated latency of the mock recognizer.
	RecognitionDelay Duration `yaml:"recognition_delay"`
	// FramesPerUtterance is the number of 20 ms frames a mock SPEAK produces.
	FramesPerUtterance int `yaml:"frames_per_utterance"`
	// AliCloud configures the Alibaba Cloud Intelligent Speech engine.
	AliCloud AliCloud `yaml:"alicloud"`
	// Tencent configures the Tencent Cloud speech engine.
	Tencent Tencent `yaml:"tencent"`
	// Xfyun configures the iFlytek (讯飞) speech engine.
	Xfyun Xfyun `yaml:"xfyun"`
}

// Xfyun configures iFlytek real-time transcription (rtasr) and the online
// speech synthesis WebSocket API.
type Xfyun struct {
	// AppID is the application id from the iFlytek console.
	AppID string `yaml:"app_id"`
	// APIKey is the API key of the speech service.
	APIKey string `yaml:"api_key"`
	// APISecret is the API secret used for synthesis request signing.
	APISecret string `yaml:"api_secret"`
	// ASREndpoint overrides the transcription URL, which defaults to
	// wss://rtasr.xfyun.cn/v1/ws.
	ASREndpoint string `yaml:"asr_endpoint"`
	// TTSEndpoint overrides the synthesis URL, which defaults to
	// wss://tts-api.xfyun.cn/v2/tts.
	TTSEndpoint string `yaml:"tts_endpoint"`
	// Timeout bounds connection establishment and control writes.
	Timeout Duration `yaml:"timeout"`
	// ASR configures real-time transcription.
	ASR XfyunASR `yaml:"asr"`
	// TTS configures speech synthesis.
	TTS XfyunTTS `yaml:"tts"`
}

// XfyunASR configures real-time transcription.
type XfyunASR struct {
	// SampleRate is the PCM rate sent upstream. The rtasr service requires
	// 16 kHz, so 8 kHz telephony audio is upsampled.
	SampleRate int `yaml:"sample_rate"`
	// Lang is the language/locale parameter, "cn" by default.
	Lang string `yaml:"lang"`
	// ChunkMS is how much audio is buffered before each upstream frame. The
	// service recommends 40 ms (1280 bytes at 16 kHz).
	ChunkMS int `yaml:"chunk_ms"`
	// Params are extra query parameters, for example "pd" or "transType".
	Params map[string]string `yaml:"params"`
}

// XfyunTTS configures online speech synthesis.
type XfyunTTS struct {
	// Voice is the vcn (speaker) parameter, for example "xiaoyan".
	Voice string `yaml:"voice"`
	// SampleRate is 8000 or 16000; 16 kHz output is downsampled before
	// mu-law encoding.
	SampleRate int `yaml:"sample_rate"`
	// Speed, Volume and Pitch are in the range 0..100, defaulting to 50.
	Speed  int `yaml:"speed"`
	Volume int `yaml:"volume"`
	Pitch  int `yaml:"pitch"`
	// Params are extra business parameters such as bgs, reg or rdn. Values are
	// merged into the JSON request, so numbers stay numbers.
	Params map[string]any `yaml:"params"`
}

// Tencent configures Tencent Cloud real-time speech recognition v2 and
// streaming speech synthesis.
type Tencent struct {
	// AppID is the numeric Tencent Cloud AppId, written as a string so leading
	// zeros and YAML number formatting cannot alter it.
	AppID string `yaml:"app_id"`
	// SecretID and SecretKey are the API credentials used to sign the
	// WebSocket URLs.
	SecretID  string `yaml:"secret_id"`
	SecretKey string `yaml:"secret_key"`
	// ASREndpoint overrides the recognition URL, which defaults to
	// wss://asr.cloud.tencent.com/asr/v2/<app_id>.
	ASREndpoint string `yaml:"asr_endpoint"`
	// TTSEndpoint overrides the synthesis URL, which defaults to
	// wss://tts.cloud.tencent.com/stream_wsv2.
	TTSEndpoint string `yaml:"tts_endpoint"`
	// Timeout bounds connection establishment and control writes.
	Timeout Duration `yaml:"timeout"`
	// ASR configures real-time recognition.
	ASR TencentASR `yaml:"asr"`
	// TTS configures speech synthesis.
	TTS TencentTTS `yaml:"tts"`
}

// TencentASR configures real-time recognition v2.
type TencentASR struct {
	// EngineModel is the engine_model_type query parameter, for example
	// "16k_zh" or "8k_zh".
	EngineModel string `yaml:"engine_model_type"`
	// SampleRate is the PCM rate sent upstream: 8000 or 16000. Telephony audio
	// is upsampled when 16000 is selected.
	SampleRate int `yaml:"sample_rate"`
	// NeedVAD enables server side voice activity detection.
	NeedVAD bool `yaml:"need_vad"`
	// VadSilenceTime is the silence duration in milliseconds that closes a
	// sentence. Zero keeps the service default.
	VadSilenceTime int `yaml:"vad_silence_time"`
	// ConvertNumMode controls Arabic number conversion: 0 none, 1 smart, 3
	// maths aware.
	ConvertNumMode int `yaml:"convert_num_mode"`
	// ChunkMS is how much audio is buffered before each upstream frame.
	ChunkMS int `yaml:"chunk_ms"`
	// Params are extra query parameters, for example hotword_id,
	// customization_id or hotword_list. Values are strings.
	Params map[string]string `yaml:"params"`
}

// TencentTTS configures streaming speech synthesis.
type TencentTTS struct {
	// VoiceType is the Tencent voice id.
	VoiceType int `yaml:"voice_type"`
	// SampleRate is 8000 or 16000. 16 kHz output is downsampled to the 8 kHz
	// telephony rate before mu-law encoding.
	SampleRate int `yaml:"sample_rate"`
	// Speed is in the range -2..6 and Volume in -10..10.
	Speed  float64 `yaml:"speed"`
	Volume float64 `yaml:"volume"`
	// EmotionCategory and EmotionIntensity select an emotional voice.
	EmotionCategory  string `yaml:"emotion_category"`
	EmotionIntensity int    `yaml:"emotion_intensity"`
	// SegmentRate is the sentence splitting sensitivity, 0..2.
	SegmentRate int `yaml:"segment_rate"`
	// Params are extra query parameters.
	Params map[string]string `yaml:"params"`
}

// AliCloud configures Alibaba Cloud Intelligent Speech Interaction (NLS).
type AliCloud struct {
	// AppKey is the project appkey from the Intelligent Speech console.
	AppKey string `yaml:"app_key"`
	// AccessKeyID and AccessKeySecret are used to obtain a short lived NLS
	// token. They are not needed when Token is set.
	AccessKeyID     string `yaml:"access_key_id"`
	AccessKeySecret string `yaml:"access_key_secret"`
	// Region selects the NLS gateway and token service, for example
	// "cn-shanghai".
	Region string `yaml:"region"`
	// Endpoint overrides the WebSocket endpoint. Empty derives
	// wss://nls-gateway.<region>.aliyuncs.com/ws/v1.
	Endpoint string `yaml:"endpoint"`
	// TokenEndpoint overrides the token service URL. Empty derives
	// https://nls-meta.<region>.aliyuncs.com.
	TokenEndpoint string `yaml:"token_endpoint"`
	// Token is a pre-obtained NLS token. When set, no token request is made.
	Token string `yaml:"token"`
	// TokenRefreshBefore refreshes a cached token this long before it expires.
	TokenRefreshBefore Duration `yaml:"token_refresh_before"`
	// Timeout bounds connection establishment and control writes.
	Timeout Duration `yaml:"timeout"`
	// ASR configures real-time speech recognition.
	ASR AliCloudASR `yaml:"asr"`
	// TTS configures speech synthesis.
	TTS AliCloudTTS `yaml:"tts"`
}

// AliCloudASR configures real-time recognition.
type AliCloudASR struct {
	// Format is the audio encoding sent upstream. Only "pcm" is supported.
	Format string `yaml:"format"`
	// SampleRate is 8000 or 16000. 8 kHz avoids resampling telephony audio;
	// 16 kHz input is upsampled from the 8 kHz stream.
	SampleRate int `yaml:"sample_rate"`
	// EnableIntermediateResult asks the service for partial results.
	EnableIntermediateResult bool `yaml:"enable_intermediate_result"`
	// EnablePunctuationPrediction adds punctuation to results.
	EnablePunctuationPrediction bool `yaml:"enable_punctuation_prediction"`
	// EnableInverseTextNormalization converts spoken numbers to digits.
	EnableInverseTextNormalization bool `yaml:"enable_inverse_text_normalization"`
	// ChunkMS is how much audio is buffered before each upstream frame.
	ChunkMS int `yaml:"chunk_ms"`
	// Params are merged verbatim into the StartRecognition payload, for
	// example max_sentence_silence, vocabulary_id or customization_id.
	Params map[string]any `yaml:"params"`
}

// AliCloudTTS configures synthesis.
type AliCloudTTS struct {
	// Voice is the synthesizer voice name, for example "xiaoyun".
	Voice string `yaml:"voice"`
	// Format is the audio encoding requested from the service. Only "pcm" is
	// supported.
	Format string `yaml:"format"`
	// SampleRate is 8000 or 16000. 16 kHz output is downsampled to the 8 kHz
	// telephony rate before mu-law encoding.
	SampleRate int `yaml:"sample_rate"`
	// Volume is the service volume, 0-100.
	Volume int `yaml:"volume"`
	// SpeechRate and PitchRate are in the range -500 to 500.
	SpeechRate int `yaml:"speech_rate"`
	PitchRate  int `yaml:"pitch_rate"`
	// LongText uses the long text synthesis namespace.
	LongText bool `yaml:"long_text"`
	// Params are merged verbatim into the StartSynthesis payload.
	Params map[string]any `yaml:"params"`
}

// HTTP configures the optional health endpoint.
type HTTP struct {
	// Listen enables the health endpoint when non-empty, for example
	// "127.0.0.1:8080".
	Listen string `yaml:"listen"`
}

// Duration is a time.Duration that unmarshals from strings such as "200ms".
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var raw string
	if err := value.Decode(&raw); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("config: invalid duration %q: %w", raw, err)
	}
	*d = Duration(parsed)
	return nil
}

// Value returns the standard library duration.
func (d Duration) Value() time.Duration { return time.Duration(d) }

// Default returns the documented defaults. Loading a configuration file merges
// its values over this baseline.
func Default() Config {
	return Config{
		Log: Log{Level: "info", Format: "text"},
		SIP: SIP{
			Listen:      "0.0.0.0:5060",
			UserAgent:   "mrcpgo/0.1",
			ContactUser: "mrcpgo",
			ServerName:  "mrcpgo",
		},
		RTP: RTP{ListenHost: "0.0.0.0"},
		MRCP: MRCP{
			ListenHost: "0.0.0.0",
			Engine: Engine{
				Type:               "mock",
				RecognitionDelay:   Duration(200 * time.Millisecond),
				FramesPerUtterance: 50,
				AliCloud: AliCloud{
					Region:             "cn-shanghai",
					TokenRefreshBefore: Duration(5 * time.Minute),
					Timeout:            Duration(5 * time.Second),
					ASR: AliCloudASR{
						Format:                         "pcm",
						SampleRate:                     8000,
						EnablePunctuationPrediction:    true,
						EnableInverseTextNormalization: true,
						ChunkMS:                        100,
					},
					TTS: AliCloudTTS{
						Voice:      "xiaoyun",
						Format:     "pcm",
						SampleRate: 16000,
						Volume:     50,
					},
				},
				Tencent: Tencent{
					Timeout: Duration(5 * time.Second),
					ASR: TencentASR{
						EngineModel:    "16k_zh",
						SampleRate:     16000,
						NeedVAD:        true,
						ConvertNumMode: 1,
						ChunkMS:        100,
					},
					TTS: TencentTTS{
						SampleRate: 16000,
					},
				},
				Xfyun: Xfyun{
					Timeout: Duration(5 * time.Second),
					ASR: XfyunASR{
						SampleRate: 16000,
						Lang:       "cn",
						ChunkMS:    40,
					},
					TTS: XfyunTTS{
						Voice:      "xiaoyan",
						SampleRate: 16000,
						Speed:      50,
						Volume:     50,
						Pitch:      50,
					},
				},
			},
		},
	}
}

// Load reads path, merging it over the defaults. An empty path returns the
// defaults.
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, cfg.Validate()
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("config: read %s: %w", path, err)
	}
	dec := yaml.NewDecoder(strings.NewReader(expandEnv(string(data))))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("config: parse %s: %w", path, err)
	}
	return cfg, cfg.Validate()
}

// envRef matches ${NAME} references so secrets can be supplied by the
// environment instead of being written into the file. Only the braced form is
// expanded, which leaves literal dollar signs in other values untouched.
var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

func expandEnv(s string) string {
	return envRef.ReplaceAllStringFunc(s, func(match string) string {
		name := envRef.FindStringSubmatch(match)[1]
		if value, ok := os.LookupEnv(name); ok {
			return value
		}
		return match
	})
}

// Validate reports configuration problems before the server starts.
func (c *Config) Validate() error {
	if _, _, err := net.SplitHostPort(c.SIP.Listen); err != nil {
		return fmt.Errorf("config: sip.listen %q: %w", c.SIP.Listen, err)
	}
	if _, err := ParseLogLevel(c.Log.Level); err != nil {
		return err
	}
	switch c.Log.Format {
	case "text", "json":
	default:
		return fmt.Errorf("config: log.format %q must be text or json", c.Log.Format)
	}
	switch c.MRCP.Engine.Type {
	case "mock":
		if c.MRCP.Engine.FramesPerUtterance < 0 {
			return fmt.Errorf("config: mrcp.engine.frames_per_utterance must not be negative")
		}
	case "alicloud":
		if err := c.MRCP.Engine.AliCloud.validate(); err != nil {
			return err
		}
	case "tencent":
		if err := c.MRCP.Engine.Tencent.validate(); err != nil {
			return err
		}
	case "xfyun":
		if err := c.MRCP.Engine.Xfyun.validate(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("config: mrcp.engine.type %q is not supported (want mock, alicloud, tencent or xfyun)", c.MRCP.Engine.Type)
	}
	if c.SIP.ContactUser == "" {
		return fmt.Errorf("config: sip.contact_user must not be empty")
	}
	if c.SIP.ServerName == "" {
		return fmt.Errorf("config: sip.server_name must not be empty")
	}
	if c.HTTP.Listen != "" {
		if _, _, err := net.SplitHostPort(c.HTTP.Listen); err != nil {
			return fmt.Errorf("config: http.listen %q: %w", c.HTTP.Listen, err)
		}
	}
	return nil
}

// validate checks the Alibaba Cloud engine settings.
func (a *AliCloud) validate() error {
	if a.AppKey == "" {
		return fmt.Errorf("config: mrcp.engine.alicloud.app_key is required")
	}
	if a.Token == "" && (a.AccessKeyID == "" || a.AccessKeySecret == "") {
		return fmt.Errorf("config: mrcp.engine.alicloud requires token, or access_key_id and access_key_secret")
	}
	if a.Region == "" {
		return fmt.Errorf("config: mrcp.engine.alicloud.region is required")
	}
	if a.Timeout.Value() <= 0 {
		return fmt.Errorf("config: mrcp.engine.alicloud.timeout must be positive")
	}
	if a.TokenRefreshBefore.Value() < 0 {
		return fmt.Errorf("config: mrcp.engine.alicloud.token_refresh_before must not be negative")
	}
	if err := validateAliCloudAudio("asr", a.ASR.Format, a.ASR.SampleRate); err != nil {
		return err
	}
	if a.ASR.ChunkMS < 20 || a.ASR.ChunkMS > 1000 {
		return fmt.Errorf("config: mrcp.engine.alicloud.asr.chunk_ms %d must be between 20 and 1000", a.ASR.ChunkMS)
	}
	if err := validateAliCloudAudio("tts", a.TTS.Format, a.TTS.SampleRate); err != nil {
		return err
	}
	if a.TTS.Voice == "" {
		return fmt.Errorf("config: mrcp.engine.alicloud.tts.voice is required")
	}
	if a.TTS.Volume < 0 || a.TTS.Volume > 100 {
		return fmt.Errorf("config: mrcp.engine.alicloud.tts.volume %d must be between 0 and 100", a.TTS.Volume)
	}
	if a.TTS.SpeechRate < -500 || a.TTS.SpeechRate > 500 {
		return fmt.Errorf("config: mrcp.engine.alicloud.tts.speech_rate %d must be between -500 and 500", a.TTS.SpeechRate)
	}
	if a.TTS.PitchRate < -500 || a.TTS.PitchRate > 500 {
		return fmt.Errorf("config: mrcp.engine.alicloud.tts.pitch_rate %d must be between -500 and 500", a.TTS.PitchRate)
	}
	return nil
}

func validateAliCloudAudio(section, format string, sampleRate int) error {
	if format != "pcm" {
		return fmt.Errorf("config: mrcp.engine.alicloud.%s.format %q is not supported, only pcm", section, format)
	}
	if sampleRate != 8000 && sampleRate != 16000 {
		return fmt.Errorf("config: mrcp.engine.alicloud.%s.sample_rate %d must be 8000 or 16000", section, sampleRate)
	}
	return nil
}

// validate checks the Tencent Cloud engine settings.
func (t *Tencent) validate() error {
	if t.AppID == "" {
		return fmt.Errorf("config: mrcp.engine.tencent.app_id is required")
	}
	if _, err := strconv.ParseInt(t.AppID, 10, 64); err != nil {
		return fmt.Errorf("config: mrcp.engine.tencent.app_id %q must be numeric", t.AppID)
	}
	if t.SecretID == "" || t.SecretKey == "" {
		return fmt.Errorf("config: mrcp.engine.tencent.secret_id and secret_key are required")
	}
	if t.Timeout.Value() <= 0 {
		return fmt.Errorf("config: mrcp.engine.tencent.timeout must be positive")
	}
	if t.ASR.EngineModel == "" {
		return fmt.Errorf("config: mrcp.engine.tencent.asr.engine_model_type is required")
	}
	if err := validateSampleRate("tencent.asr", t.ASR.SampleRate); err != nil {
		return err
	}
	if t.ASR.ChunkMS < 20 || t.ASR.ChunkMS > 1000 {
		return fmt.Errorf("config: mrcp.engine.tencent.asr.chunk_ms %d must be between 20 and 1000", t.ASR.ChunkMS)
	}
	if t.ASR.VadSilenceTime < 0 {
		return fmt.Errorf("config: mrcp.engine.tencent.asr.vad_silence_time must not be negative")
	}
	if t.ASR.ConvertNumMode < 0 || t.ASR.ConvertNumMode > 3 {
		return fmt.Errorf("config: mrcp.engine.tencent.asr.convert_num_mode %d must be between 0 and 3", t.ASR.ConvertNumMode)
	}
	if err := validateSampleRate("tencent.tts", t.TTS.SampleRate); err != nil {
		return err
	}
	if t.TTS.Speed < -2 || t.TTS.Speed > 6 {
		return fmt.Errorf("config: mrcp.engine.tencent.tts.speed %v must be between -2 and 6", t.TTS.Speed)
	}
	if t.TTS.Volume < -10 || t.TTS.Volume > 10 {
		return fmt.Errorf("config: mrcp.engine.tencent.tts.volume %v must be between -10 and 10", t.TTS.Volume)
	}
	if t.TTS.SegmentRate < 0 || t.TTS.SegmentRate > 2 {
		return fmt.Errorf("config: mrcp.engine.tencent.tts.segment_rate %d must be between 0 and 2", t.TTS.SegmentRate)
	}
	if t.TTS.EmotionIntensity != 0 && (t.TTS.EmotionIntensity < 50 || t.TTS.EmotionIntensity > 200) {
		return fmt.Errorf("config: mrcp.engine.tencent.tts.emotion_intensity %d must be between 50 and 200", t.TTS.EmotionIntensity)
	}
	if t.TTS.EmotionIntensity != 0 && t.TTS.EmotionCategory == "" {
		return fmt.Errorf("config: mrcp.engine.tencent.tts.emotion_category is required when emotion_intensity is set")
	}
	return nil
}

// validate checks the iFlytek engine settings.
func (x *Xfyun) validate() error {
	if x.AppID == "" || x.APIKey == "" || x.APISecret == "" {
		return fmt.Errorf("config: mrcp.engine.xfyun requires app_id, api_key and api_secret")
	}
	if x.Timeout.Value() <= 0 {
		return fmt.Errorf("config: mrcp.engine.xfyun.timeout must be positive")
	}
	if x.ASR.SampleRate != 16000 {
		return fmt.Errorf("config: mrcp.engine.xfyun.asr.sample_rate must be 16000 (rtasr requires 16 kHz)")
	}
	if x.ASR.ChunkMS < 20 || x.ASR.ChunkMS > 1000 {
		return fmt.Errorf("config: mrcp.engine.xfyun.asr.chunk_ms %d must be between 20 and 1000", x.ASR.ChunkMS)
	}
	if x.TTS.Voice == "" {
		return fmt.Errorf("config: mrcp.engine.xfyun.tts.voice is required")
	}
	if err := validateSampleRate("xfyun.tts", x.TTS.SampleRate); err != nil {
		return err
	}
	for name, value := range map[string]int{
		"speed":  x.TTS.Speed,
		"volume": x.TTS.Volume,
		"pitch":  x.TTS.Pitch,
	} {
		if value < 0 || value > 100 {
			return fmt.Errorf("config: mrcp.engine.xfyun.tts.%s %d must be between 0 and 100", name, value)
		}
	}
	return nil
}

func validateSampleRate(section string, rate int) error {
	if rate != 8000 && rate != 16000 {
		return fmt.Errorf("config: mrcp.engine.%s.sample_rate %d must be 8000 or 16000", section, rate)
	}
	return nil
}

// ParseLogLevel converts a configured level into a slog level.
func ParseLogLevel(level string) (slog.Level, error) {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("config: log.level %q is not supported", level)
	}
}

// NewLogger builds the process logger described by the configuration.
func (c *Config) NewLogger() (*slog.Logger, error) {
	level, err := ParseLogLevel(c.Log.Level)
	if err != nil {
		return nil, err
	}
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if c.Log.Format == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(handler), nil
}
