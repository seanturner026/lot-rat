package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
)

const reminderLeadTime = 25 * time.Minute

var (
	publicKey ed25519.PublicKey
	ddbClient *dynamodb.Client
	ddbTable  string
)

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		panic(fmt.Sprintf("env var %s not set", key))
	}
	return v
}

func init() {
	ctx := context.Background()

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		panic(fmt.Sprintf("load aws config: %v", err))
	}

	// SSM_PARAMETER holds the path to the JSON blob; DISCORD_PUBLIC_KEY holds
	// the key within that blob whose value is the Ed25519 public key hex string.
	ssmClient := ssm.NewFromConfig(cfg)
	withDecryption := true
	paramName := mustEnv("SSM_PARAMETER")
	paramKey := mustEnv("SSM_PARAMETER_KEY")

	out, err := ssmClient.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           &paramName,
		WithDecryption: &withDecryption,
	})
	if err != nil {
		panic(fmt.Sprintf("get ssm parameter %s: %v", paramName, err))
	}

	var blob map[string]string
	if err := json.Unmarshal([]byte(*out.Parameter.Value), &blob); err != nil {
		panic(fmt.Sprintf("unmarshal ssm parameter %s: %v", paramName, err))
	}
	publicKeyHex, ok := blob[paramKey]
	if !ok {
		panic(fmt.Sprintf("key %q not found in ssm parameter %s", paramKey, paramName))
	}

	keyBytes, err := hex.DecodeString(publicKeyHex)
	if err != nil {
		panic(fmt.Sprintf("decode discord public key: %v", err))
	}
	publicKey = ed25519.PublicKey(keyBytes)

	ddbClient = dynamodb.NewFromConfig(cfg)
	ddbTable = mustEnv("DYNAMODB_TABLE_NAME")
}

// verify validates the Ed25519 signature Discord attaches to every interaction.
// Discord requires this — requests failing verification must return 401.
func verify(r events.LambdaFunctionURLRequest) bool {
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

func saveReminder(ctx context.Context, userID string, showUnix int64, showName string) error {
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
	_, err = ddbClient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(ddbTable),
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

func handler(ctx context.Context, r events.LambdaFunctionURLRequest) (events.LambdaFunctionURLResponse, error) {
	if !verify(r) {
		fmt.Println("signature verification failed")
		return respond(http.StatusUnauthorized, `{"error":"invalid signature"}`), nil
	}
	fmt.Println("signature verified")

	var ix interaction
	if err := json.Unmarshal([]byte(r.Body), &ix); err != nil {
		return respond(http.StatusBadRequest, `{"error":"bad request"}`), nil
	}

	// Type 1 = PING — Discord sends this to verify the endpoint on setup.
	if ix.Type == 1 {
		fmt.Println("ping received, sending pong")
		return respond(http.StatusOK, `{"type":1}`), nil
	}

	// Type 3 = MESSAGE_COMPONENT (button click).
	if ix.Type == 3 && ix.Data != nil {
		customID := ix.Data.CustomID
		fmt.Printf("button click: custom_id=%s user_id=%s\n", customID, ix.userID())

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

			if err := saveReminder(ctx, userID, showUnix, showName); err != nil {
				fmt.Fprintf(os.Stderr, "save reminder: %v\n", err)
				return respond(http.StatusInternalServerError, `{"error":"internal error"}`), nil
			}
			fmt.Printf("reminder saved: user_id=%s show=%s unix=%d\n", userID, showName, showUnix)

			showTime := time.Unix(showUnix, 0).In(mustLoadLocation("America/New_York"))
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
			fmt.Printf("responding 200 to Discord: %s\n", string(body))
			return respond(http.StatusOK, string(body)), nil
		}
	}

	// Unknown interaction type — ACK.
	fmt.Printf("unknown interaction type %d, sending ack\n", ix.Type)
	return respond(http.StatusOK, `{"type":1}`), nil
}

func mustLoadLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

func main() {
	lambda.Start(handler)
}
