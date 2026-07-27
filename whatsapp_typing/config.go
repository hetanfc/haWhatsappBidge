package main

import (
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Injected at build time from the add-on version (-X main.version=...), so the
// logs and the MQTT device info can't drift from config.yaml.
var version = "dev"

// Config is the full runtime configuration, built from environment variables.
// The Home Assistant add-on maps its options.json onto these vars in run.sh.
type Config struct {
	Phone       string // digits only, E.164 without '+', e.g. 393331234567
	JIDOverride string // full JID, escape hatch when the contact talks over @lid
	Name        string // display name of the contact, e.g. "Contatto"
	Slug        string // slugified name, used for topics and entity ids

	ComposingTimeout time.Duration // drop to OFF if no refresh arrives within this
	OffDelay         time.Duration // grace after "paused" before flipping to OFF
	Tick             time.Duration // how often the live duration is republished

	MarkOnline bool
	PushName   string
	PairPhone  string // your own number: pair with a code instead of a QR
	DBPath     string

	Publisher       string // "mqtt" | "ha"
	DiscoveryPrefix string
	MQTT            MQTTConfig
	HAURL           string
	HAToken         string

	LogLevel slog.Level
}

type MQTTConfig struct {
	Host     string
	Port     int
	User     string
	Password string
	TLS      bool
	ClientID string
}

var (
	digits    = regexp.MustCompile(`^[0-9]{6,20}$`)
	nonSlug   = regexp.MustCompile(`[^a-z0-9]+`)
	trimUnder = regexp.MustCompile(`^_+|_+$`)
)

func LoadConfig() (Config, error) {
	cfg := Config{
		Phone:            strings.TrimPrefix(strings.TrimSpace(env("WT_PHONE", "")), "+"),
		JIDOverride:      strings.TrimSpace(env("WT_JID", "")),
		Name:             env("WT_NAME", "Contatto"),
		ComposingTimeout: envDur("WT_COMPOSING_TIMEOUT", 20*time.Second),
		OffDelay:         envDur("WT_OFF_DELAY", 3*time.Second),
		Tick:             envDur("WT_TICK", 2*time.Second),
		MarkOnline:       envBool("WT_MARK_ONLINE", true),
		PushName:         env("WT_PUSH_NAME", "Home Assistant"),
		PairPhone:        strings.TrimPrefix(strings.TrimSpace(env("WT_PAIR_PHONE", "")), "+"),
		DBPath:           env("WT_DB_PATH", "/data/whatsapp.db"),
		Publisher:        strings.ToLower(env("WT_PUBLISHER", "mqtt")),
		DiscoveryPrefix:  env("WT_DISCOVERY_PREFIX", "homeassistant"),
		HAURL:            strings.TrimSuffix(env("HA_URL", ""), "/"),
		HAToken:          env("HA_TOKEN", ""),
		LogLevel:         envLevel("WT_LOG_LEVEL", slog.LevelInfo),
		MQTT: MQTTConfig{
			Host:     env("MQTT_HOST", ""),
			Port:     envInt("MQTT_PORT", 1883),
			User:     env("MQTT_USER", ""),
			Password: env("MQTT_PASSWORD", ""),
			TLS:      envBool("MQTT_TLS", false),
		},
	}

	cfg.Slug = slugify(cfg.Name)
	if cfg.Slug == "" {
		cfg.Slug = "contact"
	}
	cfg.MQTT.ClientID = "whatsapp-typing-" + cfg.Slug

	if cfg.Phone == "" && cfg.JIDOverride == "" {
		return cfg, fmt.Errorf("WT_PHONE is required (international number, digits only, e.g. 393331234567)")
	}
	if cfg.Phone != "" && !digits.MatchString(cfg.Phone) {
		return cfg, fmt.Errorf("WT_PHONE %q is not a plain international number (digits only, no '+', no spaces)", cfg.Phone)
	}
	if cfg.PairPhone != "" && !digits.MatchString(cfg.PairPhone) {
		return cfg, fmt.Errorf("WT_PAIR_PHONE %q is not a plain international number (digits only, no '+', no spaces)", cfg.PairPhone)
	}
	if cfg.OffDelay >= cfg.ComposingTimeout {
		return cfg, fmt.Errorf("WT_OFF_DELAY (%s) must be smaller than WT_COMPOSING_TIMEOUT (%s)", cfg.OffDelay, cfg.ComposingTimeout)
	}
	if cfg.Tick < time.Second {
		cfg.Tick = time.Second
	}

	switch cfg.Publisher {
	case "mqtt":
		if cfg.MQTT.Host == "" {
			return cfg, fmt.Errorf("MQTT_HOST is required when WT_PUBLISHER=mqtt")
		}
	case "ha":
		if cfg.HAURL == "" || cfg.HAToken == "" {
			return cfg, fmt.Errorf("HA_URL and HA_TOKEN are required when WT_PUBLISHER=ha")
		}
	default:
		return cfg, fmt.Errorf("WT_PUBLISHER must be 'mqtt' or 'ha', got %q", cfg.Publisher)
	}

	return cfg, nil
}

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = nonSlug.ReplaceAllString(s, "_")
	return trimUnder.ReplaceAllString(s, "")
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(env(key, "")); err == nil {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, err := strconv.ParseBool(env(key, "")); err == nil {
		return v
	}
	return def
}

// envDur accepts either a Go duration ("20s") or a bare number of seconds ("20").
func envDur(key string, def time.Duration) time.Duration {
	raw := env(key, "")
	if raw == "" {
		return def
	}
	if n, err := strconv.Atoi(raw); err == nil {
		return time.Duration(n) * time.Second
	}
	if d, err := time.ParseDuration(raw); err == nil {
		return d
	}
	return def
}

func envLevel(key string, def slog.Level) slog.Level {
	switch strings.ToLower(env(key, "")) {
	case "debug", "trace":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error", "fatal":
		return slog.LevelError
	}
	return def
}
