package config

import (
	"strings"
	"testing"
	"time"
)

// envLookup builds a deterministic lookup so tests never depend on the
// host environment and can run in parallel.
func envLookup(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

func validEnv() map[string]string {
	return map[string]string{
		"PLATFORM_CURRENCY": "EUR",
		"DATABASE_URL":      "postgres://molot:molot@localhost:5432/molot",
		"AUTH_HS256_SECRET": "test-secret",
	}
}

func TestLoadFailFastListsAllMissingRequired(t *testing.T) {
	t.Parallel()

	_, err := load(envLookup(map[string]string{}))
	if err == nil {
		t.Fatal("Load with empty env must fail")
	}

	msg := err.Error()
	for _, key := range []string{"PLATFORM_CURRENCY", "DATABASE_URL", "AUTH_HS256_SECRET"} {
		if !strings.Contains(msg, key) {
			t.Errorf("error must mention %s in one shot, got:\n%s", key, msg)
		}
	}
}

func TestLoadCollectsAllInvalidValuesAtOnce(t *testing.T) {
	t.Parallel()

	env := validEnv()
	env["PAYMENT_TERM"] = "two days"
	env["HTTP_PORT"] = "99999"
	env["PSP_MODE"] = "explode"
	env["LOG_FORMAT"] = "yaml"

	_, err := load(envLookup(env))
	if err == nil {
		t.Fatal("Load must fail on invalid values")
	}

	msg := err.Error()
	for _, key := range []string{"PAYMENT_TERM", "HTTP_PORT", "PSP_MODE", "LOG_FORMAT"} {
		if !strings.Contains(msg, key) {
			t.Errorf("error must mention %s in one shot, got:\n%s", key, msg)
		}
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := load(envLookup(validEnv()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	testCases := []struct {
		name string
		got  any
		want any
	}{
		{"PaymentTerm", cfg.PaymentTerm, 48 * time.Hour},
		{"PSPMode", cfg.PSPMode, PSPModeSuccess},
		{"RelistDelay", cfg.RelistDelay, time.Hour},
		{"RelistDuration", cfg.RelistDuration, 24 * time.Hour},
		{"ClosingPollInterval", cfg.ClosingPollInterval, time.Second},
		{"ExpiryPollInterval", cfg.ExpiryPollInterval, 5 * time.Second},
		{"HTTPPort", cfg.HTTPPort, 8080},
		{"OTELExporterOTLPEndpoint", cfg.OTELExporterOTLPEndpoint, "localhost:4317"},
		{"AuthMode", cfg.AuthMode, AuthModeLocalHS256},
		{"LogFormat", cfg.LogFormat, LogFormatJSON},
	}

	for _, tc := range testCases {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want default %v", tc.name, tc.got, tc.want)
		}
	}
}

func TestLoadParsesTypedValues(t *testing.T) {
	t.Parallel()

	env := validEnv()
	env["PAYMENT_TERM"] = "30s"
	env["PSP_MODE"] = "flaky"
	env["RELIST_DELAY"] = "10m"
	env["RELIST_DURATION"] = "72h"
	env["CLOSING_POLL_INTERVAL"] = "250ms"
	env["EXPIRY_POLL_INTERVAL"] = "1s"
	env["HTTP_PORT"] = "9090"
	env["OTEL_EXPORTER_OTLP_ENDPOINT"] = "collector:4317"
	env["LOG_FORMAT"] = "text"

	cfg, err := load(envLookup(env))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.PlatformCurrency != "EUR" {
		t.Errorf("PlatformCurrency = %q", cfg.PlatformCurrency)
	}
	if cfg.PaymentTerm != 30*time.Second {
		t.Errorf("PaymentTerm = %v", cfg.PaymentTerm)
	}
	if cfg.PSPMode != PSPModeFlaky {
		t.Errorf("PSPMode = %v", cfg.PSPMode)
	}
	if cfg.RelistDelay != 10*time.Minute || cfg.RelistDuration != 72*time.Hour {
		t.Errorf("relist window = %v/%v", cfg.RelistDelay, cfg.RelistDuration)
	}
	if cfg.ClosingPollInterval != 250*time.Millisecond || cfg.ExpiryPollInterval != time.Second {
		t.Errorf("poll intervals = %v/%v", cfg.ClosingPollInterval, cfg.ExpiryPollInterval)
	}
	if cfg.HTTPPort != 9090 {
		t.Errorf("HTTPPort = %d", cfg.HTTPPort)
	}
	if cfg.OTELExporterOTLPEndpoint != "collector:4317" {
		t.Errorf("OTLP endpoint = %q", cfg.OTELExporterOTLPEndpoint)
	}
	if cfg.LogFormat != LogFormatText {
		t.Errorf("LogFormat = %v", cfg.LogFormat)
	}
}

func TestLoadValidation(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name       string
		mutate     func(env map[string]string)
		wantErrFor string
	}{
		{
			name:       "currency must be 3 uppercase letters",
			mutate:     func(env map[string]string) { env["PLATFORM_CURRENCY"] = "eur" },
			wantErrFor: "PLATFORM_CURRENCY",
		},
		{
			name:       "currency must not be 2 letters",
			mutate:     func(env map[string]string) { env["PLATFORM_CURRENCY"] = "EU" },
			wantErrFor: "PLATFORM_CURRENCY",
		},
		{
			name:       "duration must be positive",
			mutate:     func(env map[string]string) { env["PAYMENT_TERM"] = "-1h" },
			wantErrFor: "PAYMENT_TERM",
		},
		{
			name:       "port must be numeric",
			mutate:     func(env map[string]string) { env["HTTP_PORT"] = "eight" },
			wantErrFor: "HTTP_PORT",
		},
		{
			name:       "auth mode is a closed enum",
			mutate:     func(env map[string]string) { env["AUTH_MODE"] = "none" },
			wantErrFor: "AUTH_MODE",
		},
		{
			name: "hs256 secret required in local mode",
			mutate: func(env map[string]string) {
				delete(env, "AUTH_HS256_SECRET")
			},
			wantErrFor: "AUTH_HS256_SECRET",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			env := validEnv()
			tc.mutate(env)

			_, err := load(envLookup(env))
			if err == nil {
				t.Fatal("Load must fail")
			}
			if !strings.Contains(err.Error(), tc.wantErrFor) {
				t.Errorf("error must mention %s, got:\n%s", tc.wantErrFor, err)
			}
		})
	}
}

func TestLoadJWKSModeDoesNotRequireSecret(t *testing.T) {
	t.Parallel()

	env := validEnv()
	delete(env, "AUTH_HS256_SECRET")
	env["AUTH_MODE"] = "jwks"

	cfg, err := load(envLookup(env))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AuthMode != AuthModeJWKS {
		t.Errorf("AuthMode = %v, want jwks", cfg.AuthMode)
	}
}
