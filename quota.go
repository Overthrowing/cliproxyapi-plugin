package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// providerKey is both the quota identifier and the auth file "type" this plugin serves.
	providerKey    = "litellm-quota"
	pluginVersion  = "0.1.0"
	schemaVersion  = 1
	requestTimeout = 15 * time.Second
	maxErrorBody   = 4 << 10
)

// The host rejects plugins without an author and repository; the Makefile sets these from git.
var (
	pluginAuthor     string
	pluginRepository string
)

var httpClient = &http.Client{Timeout: requestTimeout}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Wire shapes mirror sdk/pluginapi in CLIProxyAPI v8; only the fields used here are declared.

type registration struct {
	SchemaVersion int            `json:"schema_version"`
	Metadata      pluginMetadata `json:"metadata"`
	Capabilities  capabilities   `json:"capabilities"`
}

type pluginMetadata struct {
	Name             string
	Version          string
	Author           string
	GitHubRepository string
}

type capabilities struct {
	QuotaProvider bool `json:"quota_provider"`
}

type quotaDescribeResponse struct {
	SupportedProviders []string `json:"supported_providers"`
	DisplayName        string   `json:"display_name"`
	SupportsReset      bool     `json:"supports_reset"`
}

type quotaFetchRequest struct {
	AuthIndex   string         `json:"auth_index"`
	StorageJSON []byte         `json:"storage_json"`
	Metadata    map[string]any `json:"metadata"`
}

type quotaFetchResponse struct {
	Subscription *quotaSubscription `json:"subscription,omitempty"`
	Summary      []quotaMetric      `json:"summary,omitempty"`
	Groups       []quotaGroup       `json:"groups,omitempty"`
}

type quotaSubscription struct {
	Plan string `json:"plan,omitempty"`
}

type quotaMetric struct {
	Key      string  `json:"key"`
	Label    string  `json:"label"`
	Value    float64 `json:"value"`
	Format   string  `json:"format,omitempty"`
	Currency string  `json:"currency,omitempty"`
}

type quotaGroup struct {
	DisplayName string        `json:"displayName"`
	Buckets     []quotaBucket `json:"buckets"`
}

type quotaBucket struct {
	Window            string  `json:"window,omitempty"`
	RemainingFraction float64 `json:"remainingFraction"`
	ResetTime         string  `json:"resetTime,omitempty"`
	Description       string  `json:"description,omitempty"`
}

type quotaResetResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}

// credential is the auth-dir JSON file this plugin reads.
type credential struct {
	APIKey  string `json:"api_key"`
	BaseURL string `json:"base_url"`
}

// keyInfo is the subset of LiteLLM's GET /key/info response used for quota.
type keyInfo struct {
	Info struct {
		KeyAlias       string   `json:"key_alias"`
		Spend          float64  `json:"spend"`
		MaxBudget      *float64 `json:"max_budget"`
		BudgetDuration string   `json:"budget_duration"`
		BudgetResetAt  string   `json:"budget_reset_at"`
	} `json:"info"`
}

// handleMethod dispatches one host RPC and always returns an envelope; ok reports success.
func handleMethod(ctx context.Context, method string, payload []byte) (raw []byte, ok bool) {
	defer func() {
		// A panic would otherwise cross the C ABI and take down the host process.
		if recovered := recover(); recovered != nil {
			raw, ok = errorEnvelope("plugin_panic", fmt.Sprintf("%s panicked: %v", method, recovered)), false
		}
	}()

	var result any
	switch method {
	case "plugin.register", "plugin.reconfigure":
		result = registration{
			SchemaVersion: schemaVersion,
			Metadata: pluginMetadata{
				Name:             "LiteLLM Quota",
				Version:          pluginVersion,
				Author:           orDefault(pluginAuthor, "local"),
				GitHubRepository: orDefault(pluginRepository, "local"),
			},
			Capabilities: capabilities{QuotaProvider: true},
		}
	case "plugin.quiesce", "plugin.shutdown":
		result = struct{}{}
	case "quota.identifier":
		result = map[string]string{"identifier": providerKey}
	case "quota.describe":
		result = quotaDescribeResponse{
			SupportedProviders: []string{providerKey},
			DisplayName:        "LiteLLM",
		}
	case "quota.fetch":
		var req quotaFetchRequest
		if errDecode := json.Unmarshal(payload, &req); errDecode != nil {
			return errorEnvelope("invalid_request", "decode quota.fetch request: "+errDecode.Error()), false
		}
		resp, errFetch := fetchQuota(ctx, req)
		if errFetch != nil {
			return errorEnvelope("quota_fetch_failed", errFetch.Error()), false
		}
		result = resp
	case "quota.reset":
		result = quotaResetResponse{Message: "LiteLLM resets budgets on the key's budget_duration schedule"}
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), false
	}

	encoded, errMarshal := json.Marshal(result)
	if errMarshal != nil {
		return errorEnvelope("plugin_error", errMarshal.Error()), false
	}
	raw, _ = json.Marshal(envelope{OK: true, Result: encoded})
	return raw, true
}

func orDefault(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return fallback
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func fetchQuota(ctx context.Context, req quotaFetchRequest) (quotaFetchResponse, error) {
	cred, errCred := resolveCredential(req)
	if errCred != nil {
		return quotaFetchResponse{}, errCred
	}
	endpoint, errURL := keyInfoURL(cred.BaseURL)
	if errURL != nil {
		return quotaFetchResponse{}, errURL
	}

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errReq != nil {
		return quotaFetchResponse{}, fmt.Errorf("build /key/info request: %w", errReq)
	}
	httpReq.Header.Set("Authorization", "Bearer "+cred.APIKey)
	httpReq.Header.Set("Accept", "application/json")

	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		return quotaFetchResponse{}, fmt.Errorf("call %s: %w", endpoint, errDo)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(httpResp.Body, maxErrorBody))
		detail := strings.ReplaceAll(upstreamErrorMessage(body), cred.APIKey, "[redacted]")
		return quotaFetchResponse{}, fmt.Errorf("LiteLLM /key/info returned %d: %s", httpResp.StatusCode, detail)
	}

	var info keyInfo
	if errDecode := json.NewDecoder(httpResp.Body).Decode(&info); errDecode != nil {
		return quotaFetchResponse{}, fmt.Errorf("decode /key/info response: %w", errDecode)
	}
	return buildQuota(info), nil
}

// resolveCredential reads the auth file, falling back to host metadata (the same file, parsed).
func resolveCredential(req quotaFetchRequest) (credential, error) {
	var cred credential
	if len(req.StorageJSON) > 0 {
		if errDecode := json.Unmarshal(req.StorageJSON, &cred); errDecode != nil {
			return credential{}, fmt.Errorf("decode auth file: %w", errDecode)
		}
	}
	if cred.APIKey == "" {
		cred.APIKey, _ = req.Metadata["api_key"].(string)
	}
	if cred.BaseURL == "" {
		cred.BaseURL, _ = req.Metadata["base_url"].(string)
	}
	cred.APIKey = strings.TrimSpace(cred.APIKey)
	cred.BaseURL = strings.TrimSpace(cred.BaseURL)
	switch {
	case cred.APIKey == "":
		return credential{}, errors.New(`auth file is missing "api_key"`)
	case cred.BaseURL == "":
		return credential{}, errors.New(`auth file is missing "base_url"`)
	}
	return cred, nil
}

// keyInfoURL accepts the gateway root or its OpenAI-style /v1 base; /key/info lives at the root.
func keyInfoURL(baseURL string) (string, error) {
	parsed, errParse := url.Parse(baseURL)
	if errParse != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("base_url %q is not an absolute URL", baseURL)
	}
	parsed.Path = strings.TrimSuffix(strings.TrimRight(parsed.Path, "/"), "/v1") + "/key/info"
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func buildQuota(info keyInfo) quotaFetchResponse {
	spend := info.Info.Spend
	resp := quotaFetchResponse{
		Summary: []quotaMetric{usd("spend", "Spent", spend)},
	}
	if alias := strings.TrimSpace(info.Info.KeyAlias); alias != "" {
		resp.Subscription = &quotaSubscription{Plan: alias}
	}

	// A null max_budget means the key is unlimited: report spend only.
	if info.Info.MaxBudget == nil {
		return resp
	}
	budget := *info.Info.MaxBudget
	remaining := budget - spend
	resp.Summary = append(resp.Summary, usd("max_budget", "Budget", budget), usd("remaining", "Remaining", remaining))
	if budget <= 0 {
		return resp
	}

	duration := strings.ToLower(strings.TrimSpace(info.Info.BudgetDuration))
	resetTime := normalizeTime(info.Info.BudgetResetAt)
	bucket := quotaBucket{
		RemainingFraction: math.Max(0, math.Min(1, remaining/budget)),
		Description:       budgetLabel(duration),
	}
	switch {
	case duration == "weekly" || duration == "7d" || duration == "1w":
		// The panel's timeline draws only 5-hour and weekly windows.
		bucket.Window = "weekly"
		bucket.ResetTime = resetTime
	case resetTime != "":
		// Any other reset time would put an undrawable, empty row on the panel's
		// timeline, so the date goes in the label instead.
		if parsed, errParse := time.Parse(time.RFC3339, resetTime); errParse == nil {
			bucket.Description += " · resets " + parsed.Format("Jan 2")
		}
	}
	resp.Groups = []quotaGroup{{DisplayName: "LiteLLM key", Buckets: []quotaBucket{bucket}}}
	return resp
}

// budgetLabel names a budget after LiteLLM's budget_duration: "monthly", "1mo", "30d", "24h".
func budgetLabel(duration string) string {
	switch duration {
	case "":
		return "Total budget"
	case "hourly", "daily", "weekly", "monthly", "yearly":
		return strings.ToUpper(duration[:1]) + duration[1:] + " budget"
	}
	units := map[string]string{"s": "second", "m": "minute", "h": "hour", "d": "day", "w": "week", "mo": "month"}
	periodic := map[string]string{"h": "Hourly", "d": "Daily", "w": "Weekly", "mo": "Monthly"}
	split := strings.IndexFunc(duration, func(r rune) bool { return r < '0' || r > '9' })
	if split > 0 {
		count, unit := duration[:split], duration[split:]
		if count == "1" && periodic[unit] != "" {
			return periodic[unit] + " budget"
		}
		if units[unit] != "" {
			return count + "-" + units[unit] + " budget"
		}
	}
	return "Budget (" + duration + ")"
}

func usd(key, label string, value float64) quotaMetric {
	return quotaMetric{Key: key, Label: label, Value: value, Format: "currency", Currency: "USD"}
}

// normalizeTime converts LiteLLM's Python isoformat timestamps (microseconds, sometimes
// without an offset) to RFC 3339 UTC so browsers parse them consistently.
func normalizeTime(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	layouts := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.999999999",
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999",
	}
	for _, layout := range layouts {
		// Layouts without an offset parse as UTC, which is what LiteLLM stores.
		if parsed, errParse := time.Parse(layout, raw); errParse == nil {
			return parsed.UTC().Format(time.RFC3339)
		}
	}
	return raw
}

// upstreamErrorMessage extracts LiteLLM's error text from its JSON error shapes.
func upstreamErrorMessage(body []byte) string {
	var shaped struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Detail any `json:"detail"`
	}
	if json.Unmarshal(body, &shaped) == nil {
		if shaped.Error != nil && shaped.Error.Message != "" {
			return shaped.Error.Message
		}
		if detail, ok := shaped.Detail.(string); ok && detail != "" {
			return detail
		}
	}
	text := strings.TrimSpace(string(body))
	if text == "" {
		return "empty response body"
	}
	return text
}
