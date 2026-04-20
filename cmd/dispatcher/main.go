package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/go-json-experiment/json"
)

var discordAPIBase = "https://discord.com/api/v10"

// Handler is the Lambda entrypoint for the dispatcher.
type Handler struct {
	botToken string
	loc      *time.Location
}

func newHandler(ctx context.Context) (*Handler, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	paramName := os.Getenv("SSM_PARAMETER")
	if paramName == "" {
		return nil, fmt.Errorf("env var SSM_PARAMETER not set")
	}
	paramKey := os.Getenv("SSM_PARAMETER_KEY")
	if paramKey == "" {
		return nil, fmt.Errorf("env var SSM_PARAMETER_KEY not set")
	}

	ssmClient := ssm.NewFromConfig(cfg)
	withDecryption := true
	out, err := ssmClient.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           &paramName,
		WithDecryption: &withDecryption,
	})
	if err != nil {
		return nil, fmt.Errorf("ssm get parameter %s: %w", paramName, err)
	}

	var blob map[string]string
	if err := json.Unmarshal([]byte(*out.Parameter.Value), &blob); err != nil {
		return nil, fmt.Errorf("unmarshal ssm parameter %s: %w", paramName, err)
	}
	val, ok := blob[paramKey]
	if !ok {
		return nil, fmt.Errorf("key %q not found in ssm parameter %s", paramKey, paramName)
	}

	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return nil, fmt.Errorf("load timezone: %w", err)
	}

	return &Handler{
		botToken: val,
		loc:      loc,
	}, nil
}

// sendDM opens a DM channel with the user then posts the reminder message.
// Discord requires two API calls: create DM channel, then send message.
func sendDM(ctx context.Context, botToken, userID, message string) error {
	// Step 1: create (or retrieve existing) DM channel.
	dmBody, _ := json.Marshal(map[string]string{"recipient_id": userID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		discordAPIBase+"/users/@me/channels",
		strings.NewReader(string(dmBody)))
	if err != nil {
		return fmt.Errorf("build create-dm request: %w", err)
	}
	req.Header.Set("Authorization", "Bot "+botToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("create dm channel: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("create dm channel returned %d", resp.StatusCode)
	}

	var channel struct {
		ID string `json:"id"`
	}
	chanBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read dm channel response: %w", err)
	}
	if err := json.Unmarshal(chanBody, &channel); err != nil {
		return fmt.Errorf("decode dm channel response: %w", err)
	}

	// Step 2: send the message.
	msgBody, _ := json.Marshal(map[string]string{"content": message})
	req2, err := http.NewRequestWithContext(ctx, http.MethodPost,
		discordAPIBase+"/channels/"+channel.ID+"/messages",
		strings.NewReader(string(msgBody)))
	if err != nil {
		return fmt.Errorf("build send-message request: %w", err)
	}
	req2.Header.Set("Authorization", "Bot "+botToken)
	req2.Header.Set("Content-Type", "application/json")

	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		return fmt.Errorf("send message: %w", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK && resp2.StatusCode != http.StatusCreated {
		return fmt.Errorf("send message returned %d", resp2.StatusCode)
	}
	return nil
}

// Handle is triggered by DynamoDB Streams on REMOVE events (TTL expiry).
// Each removed record is a reminder that is now due to fire.
func (h *Handler) Handle(ctx context.Context, e events.DynamoDBEvent) error {
	var errs []string
	for _, record := range e.Records {
		// Only act on TTL-triggered removals, not manual deletes or other ops.
		if record.EventName != "REMOVE" {
			continue
		}
		if record.Change.OldImage == nil {
			continue
		}

		userID := record.Change.OldImage["user_id"].String()
		showName := record.Change.OldImage["show_name"].String()
		remindAt, err := record.Change.OldImage["remind_at"].Integer()
		if err != nil {
			slog.Error("parse remind_at", "error", err)
			continue
		}

		if userID == "" || showName == "" {
			slog.Warn("skipping record with missing fields")
			continue
		}

		// remind_at = showStart - 25m, so showStart = remindAt + 25m
		showTime := time.Unix(remindAt+25*60, 0).In(h.loc)
		msg := fmt.Sprintf("**%s** starts at %s — tune in at https://www.thelotradio.com",
			showName, showTime.Format("3:04 PM"))

		if err := sendDM(ctx, h.botToken, userID, msg); err != nil {
			slog.Error("send dm failed", "user_id", userID, "error", err)
			errs = append(errs, fmt.Sprintf("user %s: %v", userID, err))
			continue
		}
		slog.Info("reminded user", "user_id", userID, "show", showName)
	}

	if len(errs) > 0 {
		return fmt.Errorf("some reminders failed: %s", strings.Join(errs, "; "))
	}
	return nil
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	ctx := context.Background()
	h, err := newHandler(ctx)
	if err != nil {
		slog.Error("init failed", "error", err)
		os.Exit(1)
	}
	lambda.Start(h.Handle)
}
