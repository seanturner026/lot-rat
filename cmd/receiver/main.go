package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/go-json-experiment/json"
)

const reminderLeadTime = 25 * time.Minute

// DynamoDBClient is the subset of the DynamoDB API used by this package.
type DynamoDBClient interface {
	PutItem(ctx context.Context, input *dynamodb.PutItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
}

// Handler is the Lambda entrypoint for the receiver.
type Handler struct {
	publicKey ed25519.PublicKey
	ddbClient DynamoDBClient
	ddbTable  string
	loc       *time.Location
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
	tableName := os.Getenv("DYNAMODB_TABLE_NAME")
	if tableName == "" {
		return nil, fmt.Errorf("env var DYNAMODB_TABLE_NAME not set")
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
	publicKeyHex, ok := blob[paramKey]
	if !ok {
		return nil, fmt.Errorf("key %q not found in ssm parameter %s", paramKey, paramName)
	}

	keyBytes, err := hex.DecodeString(publicKeyHex)
	if err != nil {
		return nil, fmt.Errorf("decode discord public key: %w", err)
	}

	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return nil, fmt.Errorf("load timezone: %w", err)
	}

	return &Handler{
		publicKey: ed25519.PublicKey(keyBytes),
		ddbClient: dynamodb.NewFromConfig(cfg),
		ddbTable:  tableName,
		loc:       loc,
	}, nil
}

// verify validates the Ed25519 signature Discord attaches to every interaction.
// Discord requires this — requests failing verification must return 401.
func verify(r events.LambdaFunctionURLRequest, publicKey ed25519.PublicKey) bool {
	sig := r.Headers["x-signature-ed25519"]
	ts := r.Headers["x-signature-timestamp"]
	if sig == "" || ts == "" {
		return false
	}
	sigBytes, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	msg := []byte(ts + r.Body)
	return ed25519.Verify(publicKey, msg, sigBytes)
}

type interaction struct {
	Type int `json:"type"`
	Data *struct {
		CustomID string `json:"custom_id"`
	} `json:"data"`
	User *struct {
		ID string `json:"id"`
	} `json:"user"`
	Member *struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	} `json:"member"`
}

// userID returns the Discord user ID from whichever field Discord populates.
// Guild interactions use member.user.id; DM interactions use user.id.
func (i interaction) userID() string {
	if i.User != nil {
		return i.User.ID
	}
	if i.Member != nil {
		return i.Member.User.ID
	}
	return ""
}

type reminderRecord struct {
	PK       string `dynamodbav:"pk"`
	UserID   string `dynamodbav:"user_id"`
	ShowName string `dynamodbav:"show_name"`
	RemindAt int64  `dynamodbav:"remind_at"` // unix epoch — also the TTL attribute
}

func saveReminder(ctx context.Context, client DynamoDBClient, tableName, userID string, showUnix int64, showName string) error {
	remindAt := showUnix - int64(reminderLeadTime.Seconds())
	rec := reminderRecord{
		PK:       fmt.Sprintf("user#%s#show#%d", userID, showUnix),
		UserID:   userID,
		ShowName: showName,
		RemindAt: remindAt,
	}
	item, err := attributevalue.MarshalMap(rec)
	if err != nil {
		return fmt.Errorf("marshal reminder: %w", err)
	}
	_, err = client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(tableName),
		Item:      item,
	})
	return err
}

func respond(status int, body string) events.LambdaFunctionURLResponse {
	return events.LambdaFunctionURLResponse{
		StatusCode: status,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       body,
	}
}

func (h *Handler) Handle(ctx context.Context, r events.LambdaFunctionURLRequest) (events.LambdaFunctionURLResponse, error) {
	if !verify(r, h.publicKey) {
		slog.Warn("signature verification failed")
		return respond(http.StatusUnauthorized, `{"error":"invalid signature"}`), nil
	}
	slog.Info("signature verified")

	var ix interaction
	if err := json.Unmarshal([]byte(r.Body), &ix); err != nil {
		return respond(http.StatusBadRequest, `{"error":"bad request"}`), nil
	}

	// Type 1 = PING — Discord sends this to verify the endpoint on setup.
	if ix.Type == 1 {
		slog.Info("ping received, sending pong")
		return respond(http.StatusOK, `{"type":1}`), nil
	}

	// Type 3 = MESSAGE_COMPONENT (button click).
	if ix.Type == 3 && ix.Data != nil {
		customID := ix.Data.CustomID
		slog.Info("button click", "custom_id", customID, "user_id", ix.userID())

		// custom_id format: "remind:{unix_start_timestamp}:{show_name}"
		if strings.HasPrefix(customID, "remind:") {
			parts := strings.SplitN(customID, ":", 3)
			if len(parts) != 3 {
				return respond(http.StatusBadRequest, `{"error":"malformed custom_id"}`), nil
			}

			showUnix, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				return respond(http.StatusBadRequest, `{"error":"invalid timestamp"}`), nil
			}
			showName := parts[2]
			userID := ix.userID()

			if userID == "" {
				return respond(http.StatusBadRequest, `{"error":"no user id"}`), nil
			}

			if err := saveReminder(ctx, h.ddbClient, h.ddbTable, userID, showUnix, showName); err != nil {
				slog.Error("save reminder", "error", err)
				return respond(http.StatusInternalServerError, `{"error":"internal error"}`), nil
			}
			slog.Info("reminder saved", "user_id", userID, "show", showName, "unix", showUnix)

			showTime := time.Unix(showUnix, 0).In(h.loc)
			ackMsg := fmt.Sprintf("I'll send you a notification at some point before **%s** starts at %s, hopefully! Lol",
				showName, showTime.Format("3:04 PM"))

			// Type 4 = CHANNEL_MESSAGE_WITH_SOURCE, flags: 64 = ephemeral (only visible to clicker).
			body, _ := json.Marshal(map[string]any{
				"type": 4,
				"data": map[string]any{
					"content": ackMsg,
					"flags":   64,
				},
			})
			slog.Info("responding to discord", "status", 200, "body", string(body))
			return respond(http.StatusOK, string(body)), nil
		}
	}

	// Unknown interaction type — ACK.
	slog.Info("unknown interaction type, sending ack", "type", ix.Type)
	return respond(http.StatusOK, `{"type":1}`), nil
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
