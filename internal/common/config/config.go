// Package config loads and validates platform configuration from
// environment variables. Load is fail-fast: every missing or invalid
// variable is reported in a single error, not one at a time.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// AuthMode selects the JWT validation strategy. Closed enum.
type AuthMode string

const (
	// AuthModeLocalHS256 validates HS256 tokens with AUTH_HS256_SECRET
	// (dev/tests and the fake-JWT helpers use the same code path).
	AuthModeLocalHS256 AuthMode = "local-hs256"
	// AuthModeJWKS is a documented future (OIDC/JWKS validation).
	// Selecting it makes startup fail with an actionable message.
	AuthModeJWKS AuthMode = "jwks"
)

// PSPMode controls the fake payment gateway outcome (component tests).
type PSPMode string

const (
	PSPModeSuccess PSPMode = "success"
	PSPModeDecline PSPMode = "decline"
	// PSPModeFlaky fails the first Charge with a network error and
	// succeeds on retry — exercises retry and refund branches.
	PSPModeFlaky PSPMode = "flaky"
)

// LogFormat selects the slog handler. Closed enum.
type LogFormat string

const (
	LogFormatJSON LogFormat = "json"
	LogFormatText LogFormat = "text"
)

// Config is the full, typed runtime configuration of the monolith.
type Config struct {
	PlatformCurrency string // PLATFORM_CURRENCY (required): ISO-4217 alpha code, e.g. "EUR"

	// CommissionBasisPoints is the platform's cut snapshotted onto every
	// invoice at issue time (1 bp = 0.01%, valid range 0..10000).
	CommissionBasisPoints int // COMMISSION_BASIS_POINTS (default 1000 = 10%)

	// Listing policy snapshotted onto every auction at listing time (§2.1).
	VerifyAboveMinor   int64         // VERIFY_ABOVE_MINOR (default 100000): verified_bid_threshold, minor units
	SnipeWindow        time.Duration // SNIPE_WINDOW (default 5m): bid inside the window extends the deadline
	SnipeExtension     time.Duration // SNIPE_EXTENSION (default 5m): extension per snipe bid
	SnipeMaxExtensions int           // SNIPE_MAX_EXTENSIONS (default 3): cap of extensions per auction

	PaymentTerm    time.Duration // PAYMENT_TERM (default 48h): invoice due_at = issued_at + PaymentTerm
	PSPMode        PSPMode       // PSP_MODE (default success)
	RelistDelay    time.Duration // RELIST_DELAY (default 1h): relisted auction starts_at = now + delay
	RelistDuration time.Duration // RELIST_DURATION (default 24h): relisted auction bidding window length

	ClosingPollInterval time.Duration // CLOSING_POLL_INTERVAL (default 1s)
	ExpiryPollInterval  time.Duration // EXPIRY_POLL_INTERVAL (default 5s)
	BusPollInterval     time.Duration // BUS_POLL_INTERVAL (default 100ms): idle wait of SQL subscribers; bounds per-hop event latency

	HTTPPort    int    // HTTP_PORT (default 8080)
	DatabaseURL string // DATABASE_URL (required)

	OTELExporterOTLPEndpoint string // OTEL_EXPORTER_OTLP_ENDPOINT (default localhost:4317), OTLP gRPC

	AuthMode        AuthMode // AUTH_MODE (default local-hs256)
	AuthHS256Secret string   // AUTH_HS256_SECRET (required when AUTH_MODE=local-hs256)

	LogFormat LogFormat // LOG_FORMAT (default json)
}

// Load reads the configuration from the process environment.
// On failure the returned error enumerates ALL problems at once.
func Load() (Config, error) {
	return load(os.LookupEnv)
}

func load(lookup func(string) (string, bool)) (Config, error) {
	l := &loader{lookup: lookup}

	cfg := Config{
		PlatformCurrency:         l.required("PLATFORM_CURRENCY"),
		CommissionBasisPoints:    l.intInRange("COMMISSION_BASIS_POINTS", 1000, 0, 10_000),
		VerifyAboveMinor:         l.int64NonNegative("VERIFY_ABOVE_MINOR", 100_000),
		SnipeWindow:              l.duration("SNIPE_WINDOW", 5*time.Minute),
		SnipeExtension:           l.duration("SNIPE_EXTENSION", 5*time.Minute),
		SnipeMaxExtensions:       l.intInRange("SNIPE_MAX_EXTENSIONS", 3, 0, 1000),
		PaymentTerm:              l.duration("PAYMENT_TERM", 48*time.Hour),
		PSPMode:                  PSPMode(l.enum("PSP_MODE", string(PSPModeSuccess), string(PSPModeSuccess), string(PSPModeDecline), string(PSPModeFlaky))),
		RelistDelay:              l.duration("RELIST_DELAY", time.Hour),
		RelistDuration:           l.duration("RELIST_DURATION", 24*time.Hour),
		ClosingPollInterval:      l.duration("CLOSING_POLL_INTERVAL", time.Second),
		ExpiryPollInterval:       l.duration("EXPIRY_POLL_INTERVAL", 5*time.Second),
		BusPollInterval:          l.duration("BUS_POLL_INTERVAL", 100*time.Millisecond),
		HTTPPort:                 l.port("HTTP_PORT", 8080),
		DatabaseURL:              l.required("DATABASE_URL"),
		OTELExporterOTLPEndpoint: l.optional("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4317"),
		AuthMode:                 AuthMode(l.enum("AUTH_MODE", string(AuthModeLocalHS256), string(AuthModeLocalHS256), string(AuthModeJWKS))),
		AuthHS256Secret:          l.optional("AUTH_HS256_SECRET", ""),
		LogFormat:                LogFormat(l.enum("LOG_FORMAT", string(LogFormatJSON), string(LogFormatJSON), string(LogFormatText))),
	}

	if cfg.PlatformCurrency != "" && !isCurrencyCode(cfg.PlatformCurrency) {
		l.fail("PLATFORM_CURRENCY", fmt.Sprintf("must be a 3-letter uppercase ISO-4217 code, got %q", cfg.PlatformCurrency))
	}
	if cfg.AuthMode == AuthModeLocalHS256 && cfg.AuthHS256Secret == "" {
		l.fail("AUTH_HS256_SECRET", "required when AUTH_MODE=local-hs256")
	}

	if len(l.errs) > 0 {
		return Config{}, fmt.Errorf("invalid configuration:\n%w", errors.Join(l.errs...))
	}
	return cfg, nil
}

// loader accumulates every problem instead of failing on the first one.
type loader struct {
	lookup func(string) (string, bool)
	errs   []error
}

func (l *loader) fail(key, reason string) {
	l.errs = append(l.errs, fmt.Errorf("%s: %s", key, reason))
}

func (l *loader) required(key string) string {
	v, ok := l.lookup(key)
	if !ok || v == "" {
		l.fail(key, "required environment variable is not set")
		return ""
	}
	return v
}

func (l *loader) optional(key, def string) string {
	v, ok := l.lookup(key)
	if !ok || v == "" {
		return def
	}
	return v
}

func (l *loader) duration(key string, def time.Duration) time.Duration {
	raw, ok := l.lookup(key)
	if !ok || raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		l.fail(key, fmt.Sprintf("must be a Go duration (e.g. \"48h\", \"5s\"), got %q", raw))
		return def
	}
	if d <= 0 {
		l.fail(key, fmt.Sprintf("must be positive, got %q", raw))
		return def
	}
	return d
}

func (l *loader) intInRange(key string, def, minVal, maxVal int) int {
	raw, ok := l.lookup(key)
	if !ok || raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < minVal || v > maxVal {
		l.fail(key, fmt.Sprintf("must be an integer in [%d, %d], got %q", minVal, maxVal, raw))
		return def
	}
	return v
}

func (l *loader) int64NonNegative(key string, def int64) int64 {
	raw, ok := l.lookup(key)
	if !ok || raw == "" {
		return def
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		l.fail(key, fmt.Sprintf("must be a non-negative integer, got %q", raw))
		return def
	}
	return v
}

func (l *loader) port(key string, def int) int {
	raw, ok := l.lookup(key)
	if !ok || raw == "" {
		return def
	}
	p, err := strconv.Atoi(raw)
	if err != nil || p < 1 || p > 65535 {
		l.fail(key, fmt.Sprintf("must be a TCP port (1-65535), got %q", raw))
		return def
	}
	return p
}

func (l *loader) enum(key, def string, allowed ...string) string {
	raw, ok := l.lookup(key)
	if !ok || raw == "" {
		return def
	}
	for _, a := range allowed {
		if raw == a {
			return raw
		}
	}
	l.fail(key, fmt.Sprintf("must be one of %v, got %q", allowed, raw))
	return def
}

func isCurrencyCode(s string) bool {
	if len(s) != 3 {
		return false
	}
	for _, r := range s {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}
