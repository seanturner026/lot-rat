package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
)

const ssmParamName = "/lot-rat/discord-webhook-url"

type Event struct {
	Summary     string   `json:"summary"`
	Start       string   `json:"start"`
	End         string   `json:"end"`
	Description string   `json:"description"`
	Genres      []string `json:"genres"`
}

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
	var htmlTagRe = regexp.MustCompile(`<[^>]+>`)

	// Normalize: treat both <br> and newlines as segment separators
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
		// Stop at bare URLs
		if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
			break
		}
		clean = append(clean, s)
	}
	result := strings.Join(clean, " ")
	// Decode common HTML entities
	result = strings.ReplaceAll(result, "&amp;", "&")
	result = strings.ReplaceAll(result, "&lt;", "<")
	result = strings.ReplaceAll(result, "&gt;", ">")
	result = strings.ReplaceAll(result, "&quot;", "\"")
	result = strings.ReplaceAll(result, "&#39;", "'")
	return result
}

func buildMessage(events []Event) (string, error) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return "", err
	}

	now := time.Now().In(loc)
	today := now.Format("2006-01-02")

	type entry struct {
		start       time.Time
		end         time.Time
		name        string
		description string
		genres      []string
	}

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

	var rows []string
	for _, e := range entries {
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
	}

	schedule := strings.Join(rows, "\n\n")
	message := fmt.Sprintf("Today at Lot Radio:\n\n%s", schedule)
	return message, nil
}

func sendDiscord(message, webhookURL string) error {
	payload, err := json.Marshal(map[string]string{"content": message})
	if err != nil {
		return err
	}
	resp, err := http.Post(webhookURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("discord webhook returned status %d", resp.StatusCode)
	}
	return nil
}

// getWebhookURL returns the Discord webhook URL.
// For local development, it reads from the DISCORD_WEBHOOK_URL env var.
// In Lambda (where the env var is absent), it fetches from SSM Parameter Store.
func getWebhookURL(ctx context.Context) (string, error) {
	if url := os.Getenv("DISCORD_WEBHOOK_URL"); url != "" {
		return url, nil
	}

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return "", fmt.Errorf("load aws config: %w", err)
	}

	client := ssm.NewFromConfig(cfg)
	paramName := ssmParamName
	withDecryption := true
	out, err := client.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           &paramName,
		WithDecryption: &withDecryption,
	})
	if err != nil {
		return "", fmt.Errorf("ssm get parameter %s: %w", ssmParamName, err)
	}

	if out.Parameter == nil || out.Parameter.Value == nil {
		return "", fmt.Errorf("ssm parameter %s has no value", ssmParamName)
	}

	return *out.Parameter.Value, nil
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

	message, err := buildMessage(events)
	if err != nil {
		return fmt.Errorf("build message: %w", err)
	}

	fmt.Println(message)

	webhookURL, err := getWebhookURL(ctx)
	if err != nil {
		return fmt.Errorf("get webhook url: %w", err)
	}

	if err := sendDiscord(message, webhookURL); err != nil {
		return fmt.Errorf("discord: %w", err)
	}
	fmt.Println("Message sent to Discord.")

	return nil
}

func handler(ctx context.Context) error {
	return run(ctx)
}

func main() {
	if os.Getenv("AWS_LAMBDA_FUNCTION_NAME") != "" {
		lambda.Start(handler)
	} else {
		if err := run(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	}
}
