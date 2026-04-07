package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

func init() {
	Register("slack", func(cfg map[string]interface{}) (Notifier, error) {
		webhook, _ := cfg["webhook_url"].(string)
		if webhook == "" {
			return nil, fmt.Errorf("slack: webhook_url is required")
		}
		return &SlackNotifier{WebhookURL: webhook}, nil
	})
}

// SlackNotifier sends notifications to a Slack webhook using Block Kit.
type SlackNotifier struct {
	WebhookURL string
}

func (n *SlackNotifier) Name() string         { return "slack" }
func (n *SlackNotifier) GetMetrics() *Metrics  { return nil }

func (n *SlackNotifier) Ping(ctx context.Context) error {
	// Slack webhooks don't have a health endpoint. Send a minimal request
	// to verify the URL is reachable and returns 200 (Slack returns
	// "missing_text_or_fallback_or_attachments" but 400, so we just check
	// the URL is valid and connectable by sending a harmless payload).
	body, _ := json.Marshal(map[string]string{"text": ""})
	req, err := http.NewRequestWithContext(ctx, "POST", n.WebhookURL, bytes.NewBuffer(body))
	if err != nil {
		return fmt.Errorf("slack ping: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("slack ping: %w", err)
	}
	defer resp.Body.Close()
	// 400 is expected for empty text; 403/404 means bad URL/token.
	if resp.StatusCode == 403 || resp.StatusCode == 404 {
		return fmt.Errorf("slack ping: invalid webhook (status %d)", resp.StatusCode)
	}
	return nil
}

func (n *SlackNotifier) Notify(ctx context.Context, notification Notification) error {
	message := ResolveTemplate(notification.Message, notification)

	color := "#36a64f" // green
	if notification.State == "failure" {
		color = "#ff0000"
	}

	// Build Block Kit payload with attachment for color sidebar.
	blocks := []map[string]interface{}{
		{
			"type": "section",
			"text": map[string]string{
				"type": "mrkdwn",
				"text": fmt.Sprintf("*[%s]* %s", notification.HookName, message),
			},
		},
		{
			"type": "section",
			"fields": []map[string]string{
				{"type": "mrkdwn", "text": fmt.Sprintf("*Repo:*\n%s", notification.Repo)},
				{"type": "mrkdwn", "text": fmt.Sprintf("*Event:*\n%s.%s", notification.Event, notification.Action)},
				{"type": "mrkdwn", "text": fmt.Sprintf("*Sender:*\n%s", notification.Sender)},
				{"type": "mrkdwn", "text": fmt.Sprintf("*State:*\n%s", notification.State)},
			},
		},
	}

	if notification.Stdout != "" {
		stdout := notification.Stdout
		if len(stdout) > 2900 {
			stdout = stdout[:2900] + "..."
		}
		blocks = append(blocks, map[string]interface{}{
			"type": "section",
			"text": map[string]string{
				"type": "mrkdwn",
				"text": fmt.Sprintf("*Output:*\n```\n%s\n```", stdout),
			},
		})
	}

	payload := map[string]interface{}{
		"attachments": []map[string]interface{}{
			{
				"color":  color,
				"blocks": blocks,
			},
		},
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, "POST", n.WebhookURL, bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("slack notification failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("slack returned error status: %d", resp.StatusCode)
	}

	return nil
}
