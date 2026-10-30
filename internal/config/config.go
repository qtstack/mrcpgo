// Package config loads and validates the server configuration.
package config

import (
	"fmt"
	"log/slog"
	"net"
	"os"
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
	// Type is the engine implementation. Only "mock" is built in.
	Type string `yaml:"type"`
	// RecognitionDelay is the simulated latency of the mock recognizer.
	RecognitionDelay Duration `yaml:"recognition_delay"`
	// FramesPerUtterance is the number of 20 ms frames a mock SPEAK produces.
	FramesPerUtterance int `yaml:"frames_per_utterance"`
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
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("config: parse %s: %w", path, err)
	}
	return cfg, cfg.Validate()
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
	if c.MRCP.Engine.Type != "mock" {
		return fmt.Errorf("config: mrcp.engine.type %q is not supported", c.MRCP.Engine.Type)
	}
	if c.MRCP.Engine.FramesPerUtterance < 0 {
		return fmt.Errorf("config: mrcp.engine.frames_per_utterance must not be negative")
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
