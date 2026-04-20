package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/go-json-experiment/json"
)

const (
	// Discord limits action rows to 5 buttons each, and a message to 5 action rows.
	// We cap at 25 shows which covers any realistic Lot Radio day.
	maxButtons = 25
)

type Event struct {
	Summary     string   `json:"summary"`
	Start       string   `json:"start"`
	End         string   `json:"end"`
	Description string   `json:"description"`
	Genres      []string `json:"genres"`
}

// Discord webhook payload types.
type (
	discordPayload struct {
		Content    string             `json:"content"`
		Components []discordActionRow `json:"components,omitzero"`
	}

	discordActionRow struct {
		Type       int             `json:"type"` // 1 = action row
		Components []discordButton `json:"components"`
	}

	discordButton struct {
		Type     int    `json:"type"`  // 2 = button
		Style    int    `json:"style"` // 1 = primary (blurple)
		Label    string `json:"label"`
		CustomID string `json:"custom_id"`
	}
)

func fetchSchedule() (string, error) {
	req, err := http.NewRequest("GET", "https://www.thelotradio.com/calendar", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func parseSchedule(html string) ([]Event, error) {
	chunkRe := regexp.MustCompile(`self\.__next_f\.push\(\[1,\s*"([\s\S]*?)"\]\)`)
	matches := chunkRe.FindAllStringSubmatch(html, -1)

	var sb strings.Builder
	for _, m := range matches {
		var unescaped string
		if err := json.Unmarshal([]byte(`"`+m[1]+`"`), &unescaped); err != nil {
			continue
		}
		sb.WriteString(unescaped)
	}
	combined := sb.String()

	scheduleRe := regexp.MustCompile(`"schedule":\s*(\[[\s\S]*?\])\s*,\s*"enabled"`)
	sm := scheduleRe.FindStringSubmatch(combined)
	if sm == nil {
		return nil, fmt.Errorf("could not find schedule data in page")
	}

	var events []Event
	if err := json.Unmarshal([]byte(sm[1]), &events); err != nil {
		return nil, fmt.Errorf("failed to parse schedule JSON: %w", err)
	}
	return events, nil
}

func formatTime(t time.Time) string {
	h := t.Hour()
	m := t.Minute()
	period := "am"
	if h >= 12 {
		period = "pm"
	}
	if h > 12 {
		h -= 12
	}
	if h == 0 {
		h = 12
	}
	if m == 0 {
		return fmt.Sprintf("%d%s", h, period)
	}
	return fmt.Sprintf("%d:%02d%s", h, m, period)
}

// cleanDescription extracts the clean text from a description by splitting on
// <br> tags and keeping segments up to (but not including) the first one that
// contains any HTML tag.
func cleanDescription(desc string) string {
	htmlTagRe := regexp.MustCompile(`<[^>]+>`)

	normalized := strings.ReplaceAll(desc, "<br>", "\n")
	segments := strings.Split(normalized, "\n")
	var clean []string
	for _, s := range segments {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if htmlTagRe.MatchString(s) {
			break
		}
		if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
			break
		}
		clean = append(clean, s)
	}
	result := strings.Join(clean, " ")
	result = strings.ReplaceAll(result, "&amp;", "&")
	result = strings.ReplaceAll(result, "&lt;", "<")
	result = strings.ReplaceAll(result, "&gt;", ">")
	result = strings.ReplaceAll(result, "&quot;", "\"")
	result = strings.ReplaceAll(result, "&#39;", "'")
	return result
}

type entry struct {
	start       time.Time
	end         time.Time
	name        string
	description string
	genres      []string
}

func filterAndSortEntries(events []Event, loc *time.Location, today string) []entry {
	var entries []entry
	for _, e := range events {
		start, err := time.Parse(time.RFC3339, e.Start)
		if err != nil {
			continue
		}
		end, err := time.Parse(time.RFC3339, e.End)
		if err != nil {
			continue
		}
		start = start.In(loc)
		end = end.In(loc)

		if start.Format("2006-01-02") != today {
			continue
		}
		if strings.ToUpper(strings.TrimFunc(e.Summary, unicode.IsSpace)) == "RESTREAM" {
			continue
		}
		entries = append(entries, entry{start, end, e.Summary, e.Description, e.Genres})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].start.Before(entries[j].start)
	})
	return entries
}

// buildPayload constructs the Discord webhook payload with schedule content and
// one "Remind me" button per show, grouped into action rows of 5.
//
// Button custom_id format: "remind:{unix_start_timestamp}:{show_name}"
// The receiver lambda decodes this to know when to DM the user.
func buildPayload(events []Event) (discordPayload, error) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return discordPayload{}, err
	}

	today := time.Now().In(loc).Format("2006-01-02")
	entries := filterAndSortEntries(events, loc, today)

	var rows []string
	var buttons []discordButton

	for i, e := range entries {
		s := formatTime(e.start)
		en := formatTime(e.end)

		row := fmt.Sprintf("**%s–%s · %s**", s, en, e.name)
		if len(e.genres) > 0 {
			row += fmt.Sprintf(" [%s]", strings.Join(e.genres, ", "))
		}
		if desc := cleanDescription(e.description); desc != "" {
			row += "\n" + desc
		}
		rows = append(rows, row)

		if i < maxButtons {
			// Truncate label to 80 chars (Discord limit).
			label := e.name
			if len(label) > 77 {
				label = label[:77] + "..."
			}
			buttons = append(buttons, discordButton{
				Type:     2,
				Style:    1,
				Label:    label,
				CustomID: fmt.Sprintf("remind:%d:%s", e.start.Unix(), e.name),
			})
		}
	}

	content := fmt.Sprintf("Today at Lot Radio:\n\n%s\n\nClick a show below to get a DM reminder ~15 minutes before it starts.", strings.Join(rows, "\n\n"))

	// Group buttons into action rows of 5 (Discord limit per row).
	var actionRows []discordActionRow
	for i := 0; i < len(buttons); i += 5 {
		end := i + 5
		if end > len(buttons) {
			end = len(buttons)
		}
		actionRows = append(actionRows, discordActionRow{
			Type:       1,
			Components: buttons[i:end],
		})
	}

	return discordPayload{
		Content:    content,
		Components: actionRows,
	}, nil
}

const discordAPIBase = "https://discord.com/api/v10"

// postToChannel sends the schedule payload to a Discord channel via the bot
// API, which supports message components (buttons) unlike channel webhooks.
func postToChannel(ctx context.Context, channelID, botToken string, payload discordPayload) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		discordAPIBase+"/channels/"+channelID+"/messages",
		bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+botToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("discord api returned status %d: %s", resp.StatusCode, b)
	}
	return nil
}

type discordConfig struct {
	channelID string
	botToken  string
}

// getDiscordConfig returns the channel ID and bot token needed to post via the
// bot API. Local dev reads DISCORD_CHANNEL_ID and DISCORD_BOT_TOKEN directly;
// Lambda fetches the JSON blob at SSM_PARAMETER and reads both keys from it.
func getDiscordConfig(ctx context.Context) (discordConfig, error) {
	if id := os.Getenv("DISCORD_CHANNEL_ID"); id != "" {
		token := os.Getenv("DISCORD_BOT_TOKEN")
		if token == "" {
			return discordConfig{}, fmt.Errorf("DISCORD_BOT_TOKEN must be set when using DISCORD_CHANNEL_ID")
		}
		return discordConfig{channelID: id, botToken: token}, nil
	}
	paramName := os.Getenv("SSM_PARAMETER")
	if paramName == "" {
		return discordConfig{}, fmt.Errorf("SSM_PARAMETER must be set")
	}
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return discordConfig{}, fmt.Errorf("load aws config: %w", err)
	}
	client := ssm.NewFromConfig(cfg)
	withDecryption := true
	out, err := client.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           &paramName,
		WithDecryption: &withDecryption,
	})
	if err != nil {
		return discordConfig{}, fmt.Errorf("ssm get parameter %s: %w", paramName, err)
	}
	if out.Parameter == nil || out.Parameter.Value == nil {
		return discordConfig{}, fmt.Errorf("ssm parameter %s has no value", paramName)
	}
	var blob map[string]string
	if err := json.Unmarshal([]byte(*out.Parameter.Value), &blob); err != nil {
		return discordConfig{}, fmt.Errorf("unmarshal ssm parameter %s: %w", paramName, err)
	}
	for _, key := range []string{"channel_id", "bot_token"} {
		if _, ok := blob[key]; !ok {
			return discordConfig{}, fmt.Errorf("key %q not found in ssm parameter %s", key, paramName)
		}
	}
	return discordConfig{channelID: blob["channel_id"], botToken: blob["bot_token"]}, nil
}

func run(ctx context.Context) error {
	html, err := fetchSchedule()
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}

	events, err := parseSchedule(html)
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}

	payload, err := buildPayload(events)
	if err != nil {
		return fmt.Errorf("build payload: %w", err)
	}

	slog.Info("schedule built", "content", payload.Content)

	dcfg, err := getDiscordConfig(ctx)
	if err != nil {
		return fmt.Errorf("get discord config: %w", err)
	}

	if err := postToChannel(ctx, dcfg.channelID, dcfg.botToken, payload); err != nil {
		return fmt.Errorf("discord: %w", err)
	}
	slog.Info("message sent to discord")
	return nil
}

func handler(ctx context.Context) error {
	return run(ctx)
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if os.Getenv("AWS_LAMBDA_FUNCTION_NAME") != "" {
		lambda.Start(handler)
	} else {
		if err := run(context.Background()); err != nil {
			slog.Error("run failed", "error", err)
			os.Exit(1)
		}
	}
}
