package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"

	"github.com/bwmarrin/discordgo"
	"github.com/rs/zerolog/log"
)

func init() {
	Register("discord_webhook", func(cfg map[string]interface{}) (Notifier, error) {
		webhook, _ := cfg["webhook_url"].(string)
		if webhook == "" {
			return nil, fmt.Errorf("discord_webhook: webhook_url is required")
		}
		return &DiscordWebhookNotifier{WebhookURL: webhook}, nil
	})

	Register("discord", func(cfg map[string]interface{}) (Notifier, error) {
		token, _ := cfg["token"].(string)
		if t := os.Getenv("DISCORD_BOT_TOKEN"); t != "" {
			token = t
		}
		guildID, _ := cfg["guild_id"].(string)
		if g := os.Getenv("DISCORD_GUILD_ID"); g != "" {
			guildID = g
		}
		categoryID, _ := cfg["category_id"].(string)
		channelID, _ := cfg["channel_id"].(string)
		notifyChannelID, _ := cfg["notify_channel_id"].(string)
		if nc := os.Getenv("DISCORD_NOTIFY_CHANNEL_ID"); nc != "" {
			notifyChannelID = nc
		}

		if token == "" {
			return nil, fmt.Errorf("discord: token is required (config or DISCORD_BOT_TOKEN env)")
		}
		if guildID == "" && channelID == "" {
			return nil, fmt.Errorf("discord: guild_id or channel_id is required")
		}

		dg, err := discordgo.New("Bot " + token)
		if err != nil {
			return nil, fmt.Errorf("discord: error creating Discord session: %w", err)
		}

		return &DiscordBotNotifier{
			session:       dg,
			GuildID:       guildID,
			CategoryID:    categoryID,
			ChannelID:     channelID,
			NotifyChannelID: notifyChannelID,
			channelCache:  &sync.Map{},
		}, nil
	})
}

// DiscordWebhookNotifier sends notifications to a Discord webhook.
type DiscordWebhookNotifier struct {
	WebhookURL string
}

func (n *DiscordWebhookNotifier) Name() string { return "discord-webhook" }
func (n *DiscordWebhookNotifier) GetMetrics() *Metrics { return nil }

func (n *DiscordWebhookNotifier) Ping(ctx context.Context) error {
	// Discord webhooks don't have a dedicated ping endpoint.
	// Validate by doing a GET which returns webhook info.
	req, err := http.NewRequestWithContext(ctx, "GET", n.WebhookURL, nil)
	if err != nil {
		return fmt.Errorf("discord webhook ping: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("discord webhook ping: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("discord webhook ping: status %d", resp.StatusCode)
	}
	return nil
}

func (n *DiscordWebhookNotifier) Notify(ctx context.Context, notification Notification) error {
	message := ResolveTemplate(notification.Message, notification)

	color := 0x00ff00 // green: success
	switch notification.State {
	case "failure":
		color = 0xff0000 // red
	case "pending":
		color = 0x3498db // blue
	}

	embed := map[string]interface{}{
		"title":       fmt.Sprintf("[%s] %s", notification.HookName, message),
		"description": fmt.Sprintf("**Repo:** %s\n**Event:** %s.%s\n**Sender:** %s", notification.Repo, notification.Event, notification.Action, notification.Sender),
		"color":       color,
	}

	if notification.Stdout != "" {
		stdout := notification.Stdout
		if len(stdout) > 1000 {
			stdout = stdout[:1000] + "..."
		}
		embed["fields"] = []map[string]interface{}{
			{
				"name":  "Output",
				"value": fmt.Sprintf("```\n%s\n```", stdout),
			},
		}
	}

	body, _ := json.Marshal(map[string]interface{}{
		"embeds": []map[string]interface{}{embed},
	})

	req, _ := http.NewRequestWithContext(ctx, "POST", n.WebhookURL, bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("discord webhook notification failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("discord webhook returned error status: %d", resp.StatusCode)
	}

	return nil
}

// DiscordBotNotifier sends notifications using a native Discord Bot.
// The session is created once and reused. Channel IDs are cached per repo slug.
type DiscordBotNotifier struct {
	session       *discordgo.Session
	GuildID       string
	CategoryID    string
	ChannelID     string
	NotifyChannelID string // dedicated channel ID to route all notifications to
	channelCache  *sync.Map // slug -> channelID
}

func (n *DiscordBotNotifier) Name() string         { return "discord-bot" }
func (n *DiscordBotNotifier) GetMetrics() *Metrics  { return nil }

func (n *DiscordBotNotifier) Ping(ctx context.Context) error {
	_, err := n.session.User("@me")
	if err != nil {
		return fmt.Errorf("discord bot ping: %w", err)
	}

	if n.NotifyChannelID != "" {
		_, err := n.session.ChannelMessageSendEmbed(n.NotifyChannelID, &discordgo.MessageEmbed{
			Title:       "Eventic Started",
			Description: "Eventic client is online and connected.",
			Color:       0x00ff00,
		})
		if err != nil {
			return fmt.Errorf("discord bot ping: failed to send startup message to notify_channel_id: %w", err)
		}
	}

	return nil
}

func (n *DiscordBotNotifier) Notify(ctx context.Context, notification Notification) error {
	channelID, err := n.resolveChannel(notification.Repo)
	if err != nil {
		return err
	}

	message := ResolveTemplate(notification.Message, notification)

	color := 0x00ff00 // green: success
	switch notification.State {
	case "failure":
		color = 0xff0000 // red
	case "pending":
		color = 0x3498db // blue
	}

	embed := &discordgo.MessageEmbed{
		Title:       fmt.Sprintf("[%s] %s", notification.HookName, message),
		Description: fmt.Sprintf("**Repo:** %s\n**Event:** %s.%s\n**Sender:** %s", notification.Repo, notification.Event, notification.Action, notification.Sender),
		Color:       color,
		Fields:      []*discordgo.MessageEmbedField{},
	}

	if notification.Stdout != "" {
		stdout := notification.Stdout
		if len(stdout) > 1000 {
			stdout = stdout[:1000] + "..."
		}
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:  "Output",
			Value: fmt.Sprintf("```\n%s\n```", stdout),
		})
	}

	_, err = n.session.ChannelMessageSendEmbed(channelID, embed)
	if err != nil {
		return fmt.Errorf("error sending message: %w", err)
	}

	return nil
}

// resolveChannel returns the Discord channel ID for a repo, using cache.
// Priority: ChannelID (static ID) > NotifyChannelID (dedicated channel) > per-repo auto-create.
func (n *DiscordBotNotifier) resolveChannel(repo string) (string, error) {
	// Static channel ID takes priority.
	if n.ChannelID != "" {
		return n.ChannelID, nil
	}

	// Dedicated channel: route all notifications to a single channel by ID.
	if n.NotifyChannelID != "" {
		return n.NotifyChannelID, nil
	}

	slug := repoSlug(repo)

	// Check cache first.
	if cached, ok := n.channelCache.Load(slug); ok {
		return cached.(string), nil
	}

	// Look up existing channels in the guild.
	channels, err := n.session.GuildChannels(n.GuildID)
	if err != nil {
		return "", fmt.Errorf("error fetching guild channels: %w", err)
	}

	for _, ch := range channels {
		if ch.Name == slug {
			n.channelCache.Store(slug, ch.ID)
			return ch.ID, nil
		}
	}

	// Create new channel.
	st, err := n.session.GuildChannelCreateComplex(n.GuildID, discordgo.GuildChannelCreateData{
		Name:     slug,
		Type:     discordgo.ChannelTypeGuildText,
		ParentID: n.CategoryID,
	})
	if err != nil {
		return "", fmt.Errorf("error creating channel: %w", err)
	}
	log.Info().Str("channel", slug).Str("id", st.ID).Msg("created Discord channel")
	n.channelCache.Store(slug, st.ID)
	return st.ID, nil
}
