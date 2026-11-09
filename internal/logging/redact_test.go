// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package logging

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	// testToken is a Hugging Face style access token.
	testToken = "hf_abcdefghijklmnopqrstuvwxyz012345"
	// testProxyPassword is the password of the configured proxy.
	testProxyPassword = "pr0xy-p4ssword"
	// testAuthToken is the credential of an Authorization header.
	testAuthToken = "auth-token-0123456789"
	// testBasicPassword is the base64 payload of a Basic auth header.
	testBasicPassword = "dXNlcjpwYXNzd29yZA=="
)

// secrets lists every credential that must never reach a sink.
func secrets() []string {
	return []string{testToken, testProxyPassword, testAuthToken, testBasicPassword}
}

// attrsText renders the attribute set of a record as text.
func attrsText(t *testing.T, attrs map[string]any) string {
	t.Helper()
	if len(attrs) == 0 {
		return ""
	}
	encoded, err := json.Marshal(attrs)
	if err != nil {
		t.Fatalf("marshal attrs: %v", err)
	}
	return string(encoded)
}

func TestRedactRemovesCredentialsFromText(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		secret string
	}{
		{
			name:   "authorization bearer header",
			input:  "Authorization: Bearer " + testAuthToken,
			secret: testAuthToken,
		},
		{
			name:   "proxy authorization header",
			input:  "Proxy-Authorization: Basic " + testBasicPassword,
			secret: testBasicPassword,
		},
		{
			name:   "token assignment",
			input:  "token=" + testToken,
			secret: testToken,
		},
		{
			name:   "hugging face token in free text",
			input:  "using " + testToken + " for the request",
			secret: testToken,
		},
		{
			name:   "url credentials",
			input:  "proxy url http://user:" + testProxyPassword + "@proxy.example:8080",
			secret: testProxyPassword,
		},
		{
			name:   "password assignment",
			input:  "proxy password: " + testProxyPassword,
			secret: testProxyPassword,
		},
		{
			name:   "api key assignment",
			input:  `api_key="` + testAuthToken + `"`,
			secret: testAuthToken,
		},
		{
			name:   "cookie header",
			input:  "Cookie: session=" + testAuthToken,
			secret: testAuthToken,
		},
		{
			name:   "bare bearer credential",
			input:  "Bearer " + testAuthToken,
			secret: testAuthToken,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Redact(tc.input)
			if strings.Contains(got, tc.secret) {
				t.Errorf("Redact(%q) = %q, the credential survived", tc.input, got)
			}
			if !strings.Contains(got, RedactedMarker) {
				t.Errorf("Redact(%q) = %q, want a redaction marker", tc.input, got)
			}
		})
	}
}

func TestRedactKeepsOrdinaryText(t *testing.T) {
	cases := []string{
		"downloaded 42 bytes from org/model",
		"tokenizer.json was written to the cache",
		"max_tokens reached",
		"waiting for the server to answer",
		"",
	}
	for _, input := range cases {
		if got := Redact(input); got != input {
			t.Errorf("Redact(%q) = %q, want it unchanged", input, got)
		}
	}
}

func TestIsSensitiveKey(t *testing.T) {
	sensitive := []string{
		"token",
		"hf_token",
		"HF_TOKEN",
		"password",
		"proxy_password",
		"proxyPassword",
		"passwd",
		"secret",
		"client_secret",
		"api_key",
		"apiKey",
		"x-api-key",
		"authorization",
		"Authorization",
		"credential",
		"cookie",
		"access_key",
		"private_key",
		"session_id",
	}
	for _, key := range sensitive {
		if !IsSensitiveKey(key) {
			t.Errorf("IsSensitiveKey(%q) = false, want true", key)
		}
	}

	harmless := []string{"repo", "path", "bytes", "username", "status", "component", "elapsed"}
	for _, key := range harmless {
		if IsSensitiveKey(key) {
			t.Errorf("IsSensitiveKey(%q) = true, want false", key)
		}
	}
}

func TestPipelineRedactsSecretsFromEverySink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hfdesk.log")

	var console bytes.Buffer
	p, err := New(Options{
		Console:       true,
		ConsoleWriter: &console,
		File:          true,
		FilePath:      path,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	logger := p.Logger()
	logger.Info("request failed with Authorization: Bearer "+testAuthToken,
		"token", testToken,
		"proxy_password", testProxyPassword,
		"authorization", "Bearer "+testAuthToken,
		"proxy", "http://user:"+testBasicPassword+"@proxy.example:8080",
	)

	records, _ := p.Records(0, OldestFirst)
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}

	fileData, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}

	outputs := map[string]string{
		"console": console.String(),
		"file":    string(fileData),
		"message": records[0].Message,
		"attrs":   attrsText(t, records[0].Attrs),
	}
	for name, output := range outputs {
		for _, secret := range secrets() {
			if strings.Contains(output, secret) {
				t.Errorf("%s output contains the credential %q: %q", name, secret, output)
			}
		}
	}

	if !strings.Contains(console.String(), RedactedMarker) {
		t.Errorf("the console output carries no redaction marker: %q", console.String())
	}
	if got := records[0].Attrs["token"]; got != RedactedMarker {
		t.Errorf("attrs[token] = %v, want %q", got, RedactedMarker)
	}
	if got := records[0].Attrs["proxy_password"]; got != RedactedMarker {
		t.Errorf("attrs[proxy_password] = %v, want %q", got, RedactedMarker)
	}
	if got := records[0].Attrs["authorization"]; got != RedactedMarker {
		t.Errorf("attrs[authorization] = %v, want %q", got, RedactedMarker)
	}
}

func TestRedactionKeepsOrdinaryAttributes(t *testing.T) {
	p, _ := newTestPipeline(t, Options{})

	p.Logger().Info("download started", "repo", "org/model", "bytes", int64(42))

	records, _ := p.Records(0, OldestFirst)
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	if got := records[0].Attrs["repo"]; got != "org/model" {
		t.Errorf("attrs[repo] = %v, want %q", got, "org/model")
	}
	if got := records[0].Attrs["bytes"]; got != int64(42) {
		t.Errorf("attrs[bytes] = %v, want 42", got)
	}
}

func TestRedactionAppliesToGroupedAttributes(t *testing.T) {
	p, _ := newTestPipeline(t, Options{})

	p.Logger().WithGroup("proxy").Info("configured",
		"password", testProxyPassword,
		"url", "http://user:"+testBasicPassword+"@proxy.example:8080",
	)

	records, _ := p.Records(0, OldestFirst)
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	text := attrsText(t, records[0].Attrs)
	for _, secret := range secrets() {
		if strings.Contains(text, secret) {
			t.Errorf("grouped attributes contain the credential %q: %q", secret, text)
		}
	}
}

// testConfig is a configuration structure that carries credentials.
type testConfig struct {
	Token    string `json:"token"`
	Endpoint string `json:"endpoint"`
	Proxy    struct {
		Password string `json:"password"`
	} `json:"proxy"`
	Hidden string `json:"-"`
}

func TestRedactionCoversConfigurationStructures(t *testing.T) {
	p, console := newTestPipeline(t, Options{})

	cfg := testConfig{
		Token:    testToken,
		Endpoint: "https://huggingface.co",
		Hidden:   testProxyPassword,
	}
	cfg.Proxy.Password = testProxyPassword

	p.Logger().Info("configuration loaded", "config", cfg)

	records, _ := p.Records(0, OldestFirst)
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	outputs := map[string]string{
		"console": console.String(),
		"attrs":   attrsText(t, records[0].Attrs),
	}
	for name, output := range outputs {
		for _, secret := range secrets() {
			if strings.Contains(output, secret) {
				t.Errorf("%s output contains the credential %q: %q", name, secret, output)
			}
		}
	}
	if got := records[0].Attrs["config"]; !strings.Contains(got.(map[string]any)["token"].(string), RedactedMarker) {
		t.Errorf("the token field was not redacted: %v", got)
	}
}

func TestRedactionCoversHeaderMaps(t *testing.T) {
	p, console := newTestPipeline(t, Options{})

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+testAuthToken)
	headers.Set("X-Api-Key", testToken)

	p.Logger().Info("request prepared", "headers", headers)

	records, _ := p.Records(0, OldestFirst)
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	outputs := map[string]string{
		"console": console.String(),
		"attrs":   attrsText(t, records[0].Attrs),
	}
	for name, output := range outputs {
		for _, secret := range secrets() {
			if strings.Contains(output, secret) {
				t.Errorf("%s output contains the credential %q: %q", name, secret, output)
			}
		}
	}
}

func TestRedactionCoversStringMaps(t *testing.T) {
	p, console := newTestPipeline(t, Options{})

	p.Logger().Info("credentials", "values", map[string]string{
		"password": testProxyPassword,
		"user":     "alice",
	})

	records, _ := p.Records(0, OldestFirst)
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	outputs := map[string]string{
		"console": console.String(),
		"attrs":   attrsText(t, records[0].Attrs),
	}
	for name, output := range outputs {
		if strings.Contains(output, testProxyPassword) {
			t.Errorf("%s output contains the credential: %q", name, output)
		}
	}
}
