package decodermonitor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testEmail() EmailConfig {
	return EmailConfig{ //nolint:gosec // Explicitly fake fixture credential, never usable in production.
		TenantID: "00000000-0000-0000-0000-000000000001", ClientID: "00000000-0000-0000-0000-000000000002",
		ClientSecret: "fixture+/&=not-a-real-secret", FromAddress: "sender@example.invalid", Recipients: "one@example.invalid, two@example.invalid",
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func mailResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func checkTokenForm(t *testing.T, r *http.Request, config *EmailConfig) {
	t.Helper()
	if err := r.ParseForm(); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"client_secret": config.ClientSecret, "client_id": config.ClientID,
		"scope": "https://graph.microsoft.com/.default", "grant_type": "client_credentials",
	} {
		if r.Form.Get(key) != want {
			t.Errorf("incorrect form-encoded field: %s", key)
		}
	}
}

func TestGraphProtocolAndLazyConstruction(t *testing.T) {
	t.Parallel()
	config := testEmail()
	client := newGraphSender(&config)
	if client.tokenURL != "https://login.microsoftonline.com/"+config.TenantID+"/oauth2/v2.0/token" ||
		client.mailURL != "https://graph.microsoft.com/v1.0/users/sender@example.invalid/sendMail" {
		t.Fatal("production endpoints are not fixed to Microsoft")
	}
	calls := 0
	client.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodPost {
			t.Fatal("request was not POST")
		}
		if r.URL.String() == client.tokenURL {
			checkTokenForm(t, r, &config)
			return mailResponse(200, `{"access_token":"fixture-token","token_type":"Bearer","expires_in":3600}`), nil
		}
		if r.URL.String() != client.mailURL || r.Header.Get("Authorization") != "Bearer fixture-token" || r.Header.Get("Content-Type") != "application/json" {
			t.Fatal("mail endpoint/authorization/content-type is wrong")
		}
		var payload struct {
			Message graphMessage `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Message.Subject != "Test" || payload.Message.Body.ContentType != "Text" || payload.Message.Body.Content != "Body" ||
			len(payload.Message.ToRecipients) != 2 || payload.Message.ToRecipients[1].EmailAddress.Address != "two@example.invalid" {
			t.Fatal("unexpected Graph JSON payload")
		}
		return mailResponse(http.StatusAccepted, ""), nil
	})
	if err := client.SendMail(t.Context(), "Test", "Body"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("expected one token request and one mail request, got %d", calls)
	}
}

func TestGraphFailureDoesNotExposeSecrets(t *testing.T) {
	for _, tc := range []struct {
		name       string
		tokenReply string
		tokenCode  int
		mailCode   int
	}{
		{"token_denied", "sensitive upstream diagnostic", 401, 202},
		{"token_transient", "sensitive upstream diagnostic", 503, 202},
		{"token_malformed", "sensitive upstream diagnostic", 200, 202},
		{"token_null", "null", 200, 202},
		{"token_missing", `{}`, 200, 202},
		{"token_oversize", strings.Repeat("x", 65537), 200, 202},
		{"token_type", `{"access_token":"sensitive","token_type":"Basic","expires_in":3600}`, 200, 202},
		{"token_expired", `{"access_token":"sensitive","token_type":"Bearer","expires_in":0}`, 200, 202},
		{"token_injection", `{"access_token":"sensitive\r\ninjected","token_type":"Bearer","expires_in":3600}`, 200, 202},
		{"mail_denied", `{"access_token":"sensitive","token_type":"Bearer","expires_in":3600}`, 200, 403},
		{"mail_unexpected_success", `{"access_token":"sensitive","token_type":"Bearer","expires_in":3600}`, 200, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			config := testEmail()
			client := newGraphSender(&config)
			client.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() == client.tokenURL {
					return mailResponse(tc.tokenCode, tc.tokenReply), nil
				}
				return mailResponse(tc.mailCode, "sensitive upstream diagnostic"), nil
			})
			err := client.SendMail(t.Context(), "Test", "Body")
			if err == nil || strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), config.ClientSecret) {
				t.Fatalf("failure was accepted or secret exposed: %v", err)
			}
		})
	}
}

func TestGraphCancellationIncludesTokenAcquisition(t *testing.T) {
	t.Parallel()
	config := testEmail()
	client := newGraphSender(&config)
	client.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := client.SendMail(ctx, "Test", "Body"); !errors.Is(err, context.Canceled) {
		t.Fatalf("token acquisition ignored cancellation: %v", err)
	}
}

func TestGraphRedirectsNeverForwardCredentials(t *testing.T) {
	for _, endpoint := range []string{"token", "mail"} {
		t.Run(endpoint, func(t *testing.T) {
			t.Parallel()
			config := testEmail()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if endpoint == "mail" && r.URL.Path == "/token" {
					_, _ = io.WriteString(w, `{"access_token":"fixture-token","token_type":"Bearer","expires_in":3600}`)
					return
				}
				if r.URL.Path == "/redirected" {
					t.Error("redirect followed; could disclose credentials")
					return
				}
				w.Header().Set("Location", "/redirected")
				w.WriteHeader(http.StatusTemporaryRedirect)
			}))
			defer server.Close()
			client := newGraphSender(&config)
			client.tokenURL, client.mailURL = server.URL+"/token", server.URL+"/mail"
			if err := client.SendMail(t.Context(), "Test", "Body"); err == nil {
				t.Fatal("redirect accepted as delivery")
			}
			want := int32(1)
			if endpoint == "mail" {
				want = 2
			}
			if calls.Load() != want {
				t.Fatalf("redirect followed: %d requests, want %d", calls.Load(), want)
			}
		})
	}
}

func TestRetryAfterPersistsAcrossRestart(t *testing.T) {
	// Whole seconds, so the HTTP date round-trips exactly.
	at := time.Now().Truncate(time.Second)
	for _, tc := range []struct{ name, retry string }{
		{"seconds", "120"}, {"http_date", at.Add(2 * time.Minute).UTC().Format(http.TimeFormat)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, store := testStore(t)
			cfg.SendMail = true
			if err := store.Update(func(s *State) error {
				s.Outbox = []Mail{{ID: 1, Created: at, Subject: "Test", Body: "Body"}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			config := testEmail()
			client := newGraphSender(&config)
			client.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() == client.tokenURL {
					return mailResponse(200, `{"access_token":"fixture-token","token_type":"Bearer","expires_in":3600}`), nil
				}
				response := mailResponse(http.StatusTooManyRequests, "do not expose this diagnostic")
				response.Header.Set("Retry-After", tc.retry)
				return response, nil
			})
			runner := Runner{Config: cfg, Store: store, Sender: client}
			if err := runner.deliver(t.Context(), at); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenStore(&cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reopened.Close() }()
			state, err := reopened.Snapshot()
			if err != nil || len(state.Outbox) != 1 || state.Outbox[0].NextAttempt.Before(at.Add(2*time.Minute)) {
				t.Fatalf("rate-limit deadline not durable: %+v %v", state.Outbox, err)
			}
		})
	}
}

func TestEmailConfigAndCheckModeStayOffline(t *testing.T) {
	t.Parallel()
	config := testEmail()
	data, err := json.Marshal(config) //nolint:gosec // Serialize only fake fixture credentials to a private temporary config.
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.SendMail, cfg.EmailFile = true, privateConfig(t, string(data))
	if sender, err := NewMailSender(&cfg); err != nil || sender == nil || len(sender.(*graphSender).recipients) != 2 {
		t.Fatalf("valid offline configuration rejected: %v", err)
	}
	for _, tc := range []struct {
		name   string
		change func(*EmailConfig)
	}{
		{"tenant_path", func(c *EmailConfig) { c.TenantID = "../../foreign" }},
		{"client_missing", func(c *EmailConfig) { c.ClientID = "" }},
		{"secret_missing", func(c *EmailConfig) { c.ClientSecret = "" }},
		{"mailbox_display_name", func(c *EmailConfig) { c.FromAddress = "User <sender@example.invalid>" }},
		{"recipient_control", func(c *EmailConfig) { c.Recipients = "one@example.invalid\r\ninjected" }},
		{"recipient_missing", func(c *EmailConfig) { c.Recipients = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			invalid := testEmail()
			tc.change(&invalid)
			if err := invalid.Validate(); err == nil || strings.Contains(err.Error(), invalid.ClientSecret) && invalid.ClientSecret != "" {
				t.Fatalf("invalid settings accepted or secret echoed: %v", err)
			}
		})
	}
}

func TestRetryAfterBounds(t *testing.T) {
	t.Parallel()
	now := time.Now()
	for value, want := range map[string]time.Duration{
		"120": 120 * time.Second, "0": 0, "-9223372037": 0, "999999999": 24 * time.Hour, "soon": 0,
		now.Add(-time.Minute).UTC().Format(http.TimeFormat): 0,
	} {
		var rejection *deliveryError
		response := mailResponse(http.StatusTooManyRequests, "")
		response.Header.Set("Retry-After", value)
		_ = response.Body.Close()
		if !errors.As(rejected(response, now), &rejection) {
			t.Fatal("rejection lost its type")
		}
		if got := rejection.retryAfter; (want == 0 && !got.IsZero()) || (want != 0 && !got.Equal(now.Add(want))) {
			t.Errorf("Retry-After %q: got %v, want +%v", value, got, want)
		}
	}
}
