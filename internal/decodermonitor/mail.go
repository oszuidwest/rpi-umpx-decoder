package decodermonitor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var guid = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// EmailConfig uses the same client-credentials fields as zwfm-encoder, without
// depending on that application or distributing tenant credentials in source.
type EmailConfig struct {
	TenantID     string `json:"tenant_id"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	FromAddress  string `json:"from_address"`
	Recipients   string `json:"recipients"`
}

// Validate checks local configuration only, never fetching a token or mail.
func (c *EmailConfig) Validate() error {
	if c == nil || !guid.MatchString(c.TenantID) || !guid.MatchString(c.ClientID) {
		return fmt.Errorf("M365 tenant_id and client_id must be GUIDs")
	}
	if strings.TrimSpace(c.ClientSecret) == "" {
		return fmt.Errorf("M365 client_secret is required")
	}
	if !bareAddress(c.FromAddress) {
		return fmt.Errorf("M365 from_address must be a bare mailbox address")
	}
	recipients := c.recipients()
	if len(recipients) == 0 || len(recipients) > 100 {
		return fmt.Errorf("M365 requires between 1 and 100 recipients")
	}
	for _, recipient := range recipients {
		if !bareAddress(recipient) {
			return fmt.Errorf("M365 recipients must be comma-separated bare mailbox addresses")
		}
	}
	return nil
}

func bareAddress(value string) bool {
	if strings.ContainsAny(value, "\r\n\x00") {
		return false
	}
	address, err := mail.ParseAddress(value)
	return err == nil && address.Address == value
}

func (c *EmailConfig) recipients() []string {
	var result []string
	for part := range strings.SplitSeq(c.Recipients, ",") {
		if address := strings.TrimSpace(part); address != "" {
			result = append(result, address)
		}
	}
	return result
}

type graphSender struct {
	config   EmailConfig
	client   *http.Client
	tokenURL string
	mailURL  string
}

func newGraphSender(cfg *EmailConfig) *graphSender {
	return &graphSender{
		config: *cfg,
		client: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			// Never forward credentials/tokens, even to another HTTPS location.
			return http.ErrUseLastResponse
		}},
		tokenURL: "https://login.microsoftonline.com/" + cfg.TenantID + "/oauth2/v2.0/token",
		mailURL:  "https://graph.microsoft.com/v1.0/users/" + url.PathEscape(cfg.FromAddress) + "/sendMail",
	}
}

type graphRecipient struct {
	EmailAddress struct {
		Address string `json:"address"`
	} `json:"emailAddress"`
}

type graphMessage struct {
	Subject string `json:"subject"`
	Body    struct {
		ContentType string `json:"contentType"`
		Content     string `json:"content"`
	} `json:"body"`
	ToRecipients []graphRecipient `json:"toRecipients"`
}

// SendMail performs one attempt. The runner durably retries failures, including
// Retry-After, rather than sleeping inside the observation loop or losing mail.
// Tokens are short-lived, fetched per attempt and never persisted or logged.
func (c *graphSender) SendMail(ctx context.Context, recipients []string, subject, body string) error {
	if len(recipients) == 0 {
		return fmt.Errorf("M365 requires recipients")
	}
	message := graphMessage{Subject: subject}
	message.Body.ContentType, message.Body.Content = "Text", body
	for _, address := range recipients {
		if !bareAddress(address) {
			return fmt.Errorf("M365 recipient is invalid")
		}
		var recipient graphRecipient
		recipient.EmailAddress.Address = address
		message.ToRecipients = append(message.ToRecipients, recipient)
	}
	data, err := json.Marshal(struct {
		Message graphMessage `json:"message"`
	}{message})
	if err != nil {
		return fmt.Errorf("encode M365 notification: %w", err)
	}
	token, err := c.token(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.mailURL, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create M365 mail request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusAccepted {
		return rejected(resp, time.Now())
	}
	return nil
}

func (c *graphSender) token(ctx context.Context) (string, error) {
	form := url.Values{
		"client_id": {c.config.ClientID}, "client_secret": {c.config.ClientSecret},
		"scope": {"https://graph.microsoft.com/.default"}, "grant_type": {"client_credentials"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("create M365 token request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", rejected(resp, time.Now())
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil || len(data) > 65536 {
		return "", fmt.Errorf("M365 token response unavailable or oversized")
	}
	var token struct {
		AccessToken string `json:"access_token"`
		Type        string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(data, &token); err != nil || token.AccessToken == "" ||
		!strings.EqualFold(token.Type, "Bearer") || token.ExpiresIn <= 0 || strings.ContainsAny(token.AccessToken, "\r\n\x00 ") {
		return "", fmt.Errorf("M365 token response is invalid")
	}
	return token.AccessToken, nil
}

func (c *graphSender) do(req *http.Request) (*http.Response, error) {
	// Endpoints are fixed to Microsoft HTTPS; test overrides are private.
	resp, err := c.client.Do(req)
	if err != nil {
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		// Do not echo upstream/transport errors: OAuth diagnostics can carry secrets.
		return nil, fmt.Errorf("M365 request failed")
	}
	return resp, nil
}

type deliveryError struct {
	status     int
	retryAfter time.Time
}

func (e *deliveryError) Error() string {
	return fmt.Sprintf("M365 request rejected (HTTP %d)", e.status)
}

func rejected(resp *http.Response, now time.Time) error {
	result := &deliveryError{status: resp.StatusCode}
	value := resp.Header.Get("Retry-After")
	if delay, err := strconv.ParseInt(value, 10, 64); err == nil && delay > 0 {
		result.retryAfter = now.Add(time.Duration(min(delay, 86400)) * time.Second)
	} else if at, err := http.ParseTime(value); err == nil && at.After(now) {
		result.retryAfter = minTime(at, now.Add(24*time.Hour))
	}
	return result
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
