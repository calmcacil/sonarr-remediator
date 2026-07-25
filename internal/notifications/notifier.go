package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/smtp"
	"strings"
	"sync"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/logging"
)

// Event represents a notification to send.
type Event struct {
	Type    string
	Title   string
	Message string
	Details map[string]any
}

// Notifier dispatches notifications to configured backends.
type Notifier struct {
	config      Config
	rateLimiter map[string]time.Time
	mu          sync.Mutex
}

// Config holds notification backend settings.
type Config struct {
	DiscordWebhook string
	SlackWebhook   string
	Gotify         GotifyConfig
	Ntfy           NtfyConfig
	Webhook        WebhookConfig
	Email          EmailConfig
	Events         map[string][]string
}

type GotifyConfig struct {
	URL      string
	Token    string
	Priority int
}

type NtfyConfig struct {
	URL      string
	Topic    string
	Token    string
	Priority int
}

type WebhookConfig struct {
	URL          string
	Method       string
	Headers      map[string]string
	BodyTemplate string
}

type EmailConfig struct {
	Enabled      bool
	SMTPHost     string
	SMTPPort     int
	SMTPUsername string
	SMTPPassword string
	From         string
	To           []string
}

// New creates a new Notifier.
func New(cfg Config) *Notifier {
	return &Notifier{
		config:      cfg,
		rateLimiter: make(map[string]time.Time),
	}
}

// Send dispatches an event to configured channels, respecting rate limits.
func (n *Notifier) Send(event Event) {
	n.mu.Lock()
	last, exists := n.rateLimiter[event.Type]
	if exists && time.Since(last) < 30*time.Minute {
		n.mu.Unlock()
		return
	}
	n.rateLimiter[event.Type] = time.Now()
	n.mu.Unlock()

	channels, ok := n.config.Events[event.Type]
	if !ok || len(channels) == 0 {
		return
	}

	for _, ch := range channels {
		switch strings.ToLower(ch) {
		case "discord":
			n.sendDiscord(event)
		case "slack":
			n.sendSlack(event)
		case "gotify":
			n.sendGotify(event)
		case "ntfy":
			n.sendNtfy(event)
		case "webhook":
			n.sendWebhook(event)
		case "email":
			n.sendEmail(event)
		}
	}
}

func (n *Notifier) sendDiscord(event Event) {
	if n.config.DiscordWebhook == "" {
		return
	}
	color := 16705372
	if strings.Contains(event.Type, "failed") {
		color = 15548997
	}

	payload := map[string]any{
		"embeds": []map[string]any{
			{
				"title":       event.Title,
				"description": event.Message,
				"color":       color,
				"footer":      map[string]string{"text": "Sonarr Recovery Agent"},
			},
		},
	}

	data, _ := json.Marshal(payload)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", n.config.DiscordWebhook, bytes.NewReader(data))
	if err != nil {
		logging.Logger.Error("discord notification failed", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logging.Logger.Error("discord notification failed", "error", err)
		return
	}
	resp.Body.Close()
}

func (n *Notifier) sendSlack(event Event) {
	if n.config.SlackWebhook == "" {
		return
	}
	payload := map[string]string{
		"text": fmt.Sprintf("*%s*\n%s", event.Title, event.Message),
	}
	data, _ := json.Marshal(payload)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", n.config.SlackWebhook, bytes.NewReader(data))
	if err != nil {
		logging.Logger.Error("slack notification failed", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logging.Logger.Error("slack notification failed", "error", err)
		return
	}
	resp.Body.Close()
}

func (n *Notifier) sendGotify(event Event) {
	if n.config.Gotify.URL == "" || n.config.Gotify.Token == "" {
		return
	}
	payload := map[string]any{
		"title":    event.Title,
		"message":  event.Message,
		"priority": n.config.Gotify.Priority,
	}
	data, _ := json.Marshal(payload)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	url := strings.TrimRight(n.config.Gotify.URL, "/") + "/message"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(data))
	if err != nil {
		logging.Logger.Error("gotify notification failed", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gotify-Key", n.config.Gotify.Token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logging.Logger.Error("gotify notification failed", "error", err)
		return
	}
	resp.Body.Close()
}

func (n *Notifier) sendNtfy(event Event) {
	if n.config.Ntfy.URL == "" || n.config.Ntfy.Topic == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	url := fmt.Sprintf("%s/%s", strings.TrimRight(n.config.Ntfy.URL, "/"), n.config.Ntfy.Topic)
	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(event.Message))
	if err != nil {
		logging.Logger.Error("ntfy notification failed", "error", err)
		return
	}
	req.Header.Set("Title", event.Title)
	if n.config.Ntfy.Token != "" {
		req.Header.Set("Authorization", "Bearer "+n.config.Ntfy.Token)
	}
	req.Header.Set("Priority", fmt.Sprintf("%d", n.config.Ntfy.Priority))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logging.Logger.Error("ntfy notification failed", "error", err)
		return
	}
	resp.Body.Close()
}

func (n *Notifier) sendWebhook(event Event) {
	if n.config.Webhook.URL == "" {
		return
	}
	body := event.Message
	if n.config.Webhook.BodyTemplate != "" {
		body = n.config.Webhook.BodyTemplate
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, n.config.Webhook.Method, n.config.Webhook.URL, strings.NewReader(body))
	if err != nil {
		logging.Logger.Error("webhook notification failed", "error", err)
		return
	}
	for k, v := range n.config.Webhook.Headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logging.Logger.Error("webhook notification failed", "error", err)
		return
	}
	resp.Body.Close()
}

func (n *Notifier) sendEmail(event Event) {
	if !n.config.Email.Enabled || n.config.Email.SMTPHost == "" {
		return
	}
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\n\r\n%s",
		n.config.Email.From,
		strings.Join(n.config.Email.To, ", "),
		event.Title,
		event.Message,
	)

	auth := smtp.PlainAuth("", n.config.Email.SMTPUsername, n.config.Email.SMTPPassword, n.config.Email.SMTPHost)
	addr := fmt.Sprintf("%s:%d", n.config.Email.SMTPHost, n.config.Email.SMTPPort)

	err := smtp.SendMail(addr, auth, n.config.Email.From, n.config.Email.To, []byte(msg))
	if err != nil {
		logging.Logger.Error("email notification failed", "error", err)
	}
}
