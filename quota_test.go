package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testKey = "sk-test-0123456789"

func newLiteLLM(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/key/info" {
			t.Errorf("path = %q, want /key/info", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testKey {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

func fetchRequest(t *testing.T, baseURL string) []byte {
	t.Helper()
	storage, _ := json.Marshal(map[string]string{"type": providerKey, "api_key": testKey, "base_url": baseURL})
	raw, _ := json.Marshal(map[string]any{"auth_index": "1", "storage_json": storage})
	return raw
}

func callFetch(t *testing.T, payload []byte) (envelope, quotaFetchResponse) {
	t.Helper()
	raw, _ := handleMethod(context.Background(), "quota.fetch", payload)
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	var resp quotaFetchResponse
	if env.OK {
		if err := json.Unmarshal(env.Result, &resp); err != nil {
			t.Fatalf("decode result: %v", err)
		}
	}
	return env, resp
}

func TestFetchBudgetedKey(t *testing.T) {
	server := newLiteLLM(t, http.StatusOK, `{"key":"abc","info":{
		"key_alias":"maps-dev","spend":1.25,"max_budget":5.0,
		"budget_duration":"monthly","budget_reset_at":"2026-11-01T00:00:00.123456"}}`)

	// A /v1 base (as used for OpenAI-compatible routing) must still hit the root /key/info.
	env, resp := callFetch(t, fetchRequest(t, server.URL+"/v1/"))
	if !env.OK {
		t.Fatalf("fetch failed: %+v", env.Error)
	}
	if resp.Subscription == nil || resp.Subscription.Plan != "maps-dev" {
		t.Fatalf("subscription = %+v", resp.Subscription)
	}
	want := map[string]float64{"spend": 1.25, "max_budget": 5, "remaining": 3.75}
	if len(resp.Summary) != len(want) {
		t.Fatalf("summary = %+v", resp.Summary)
	}
	for _, metric := range resp.Summary {
		if metric.Value != want[metric.Key] || metric.Format != "currency" || metric.Currency != "USD" {
			t.Errorf("metric %+v", metric)
		}
	}
	if len(resp.Groups) != 1 || len(resp.Groups[0].Buckets) != 1 {
		t.Fatalf("groups = %+v", resp.Groups)
	}
	// The panel's timeline can't draw a monthly window, so the reset date moves into
	// the label and no reset time is sent.
	bucket := resp.Groups[0].Buckets[0]
	if bucket.RemainingFraction != 0.75 || bucket.Window != "" || bucket.ResetTime != "" ||
		bucket.Description != "Monthly budget · resets Nov 1" {
		t.Errorf("bucket = %+v", bucket)
	}
}

func TestFetchWeeklyBudgetKeepsTimelineWindow(t *testing.T) {
	server := newLiteLLM(t, http.StatusOK, `{"info":{"spend":0,"max_budget":2,
		"budget_duration":"7d","budget_reset_at":"2026-11-03 00:00:00.5+00:00"}}`)

	_, resp := callFetch(t, fetchRequest(t, server.URL))
	bucket := resp.Groups[0].Buckets[0]
	if bucket.Window != "weekly" || bucket.ResetTime != "2026-11-03T00:00:00Z" || bucket.Description != "7-day budget" {
		t.Errorf("bucket = %+v", bucket)
	}
}

func TestBudgetLabel(t *testing.T) {
	for in, want := range map[string]string{
		"":        "Total budget",
		"monthly": "Monthly budget",
		"1mo":     "Monthly budget",
		"30d":     "30-day budget",
		"1d":      "Daily budget",
		"24h":     "24-hour budget",
		"2w":      "2-week budget",
		"odd":     "Budget (odd)",
	} {
		if got := budgetLabel(in); got != want {
			t.Errorf("budgetLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFetchUnlimitedKeyReportsSpendOnly(t *testing.T) {
	server := newLiteLLM(t, http.StatusOK, `{"info":{"spend":2.5,"max_budget":null,"budget_reset_at":null}}`)

	env, resp := callFetch(t, fetchRequest(t, server.URL))
	if !env.OK {
		t.Fatalf("fetch failed: %+v", env.Error)
	}
	if len(resp.Summary) != 1 || resp.Summary[0].Key != "spend" || len(resp.Groups) != 0 || resp.Subscription != nil {
		t.Errorf("resp = %+v", resp)
	}
}

func TestFetchOverspentKeyClampsMeter(t *testing.T) {
	server := newLiteLLM(t, http.StatusOK, `{"info":{"spend":6,"max_budget":5}}`)

	_, resp := callFetch(t, fetchRequest(t, server.URL))
	if got := resp.Groups[0].Buckets[0].RemainingFraction; got != 0 {
		t.Errorf("remaining fraction = %v, want 0", got)
	}
	if got := resp.Summary[2].Value; got != -1 {
		t.Errorf("remaining = %v, want -1", got)
	}
}

func TestFetchUpstreamErrorRedactsKey(t *testing.T) {
	server := newLiteLLM(t, http.StatusUnauthorized,
		`{"error":{"message":"Authentication Error, invalid key `+testKey+`"}}`)

	env, _ := callFetch(t, fetchRequest(t, server.URL))
	if env.OK || env.Error == nil {
		t.Fatal("expected error envelope")
	}
	if !strings.Contains(env.Error.Message, "401") || strings.Contains(env.Error.Message, testKey) {
		t.Errorf("message = %q", env.Error.Message)
	}
}

func TestFetchFallsBackToMetadata(t *testing.T) {
	server := newLiteLLM(t, http.StatusOK, `{"info":{"spend":0,"max_budget":1}}`)
	payload, _ := json.Marshal(map[string]any{
		"auth_index": "1",
		"metadata":   map[string]any{"api_key": testKey, "base_url": server.URL},
	})

	if env, _ := callFetch(t, payload); !env.OK {
		t.Fatalf("fetch failed: %+v", env.Error)
	}
}

func TestFetchRequiresKeyAndBaseURL(t *testing.T) {
	for name, storage := range map[string]string{
		"api_key":  `{"base_url":"https://litellm.example"}`,
		"base_url": `{"api_key":"sk-x"}`,
	} {
		payload, _ := json.Marshal(map[string]any{"storage_json": []byte(storage)})
		env, _ := callFetch(t, payload)
		if env.OK || !strings.Contains(env.Error.Message, name) {
			t.Errorf("missing %s: %+v", name, env.Error)
		}
	}
}

func TestKeyInfoURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://litellm.example":          "https://litellm.example/key/info",
		"https://litellm.example/":         "https://litellm.example/key/info",
		"https://litellm.example/v1":       "https://litellm.example/key/info",
		"https://gw.example/litellm/v1/":   "https://gw.example/litellm/key/info",
		"https://litellm.example/?foo=bar": "https://litellm.example/key/info",
	} {
		if got, err := keyInfoURL(in); err != nil || got != want {
			t.Errorf("keyInfoURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := keyInfoURL("litellm.example"); err == nil {
		t.Error("expected error for relative URL")
	}
}

func TestRegisterDeclaresOnlyQuotaProvider(t *testing.T) {
	raw, ok := handleMethod(context.Background(), "plugin.register", nil)
	if !ok {
		t.Fatalf("register failed: %s", raw)
	}
	var env struct {
		Result struct {
			Metadata     map[string]string `json:"metadata"`
			Capabilities map[string]any    `json:"capabilities"`
		} `json:"result"`
	}
	_ = json.Unmarshal(raw, &env)
	if len(env.Result.Capabilities) != 1 || env.Result.Capabilities["quota_provider"] != true {
		t.Errorf("capabilities = %v", env.Result.Capabilities)
	}
	// CLIProxyAPI's validPlugin rejects registrations missing any of these.
	for _, field := range []string{"Name", "Version", "Author", "GitHubRepository"} {
		if env.Result.Metadata[field] == "" {
			t.Errorf("metadata %s is empty: %v", field, env.Result.Metadata)
		}
	}

	if _, ok := handleMethod(context.Background(), "executor.execute", nil); ok {
		t.Error("unknown method should fail")
	}
}
