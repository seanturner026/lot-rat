package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

type mockDynamoDBClient struct {
	putItemOutput *dynamodb.PutItemOutput
	err           error
	lastInput     *dynamodb.PutItemInput
}

func (m *mockDynamoDBClient) PutItem(_ context.Context, input *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	m.lastInput = input
	return m.putItemOutput, m.err
}

func TestVerify(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)

	tests := []struct {
		name string
		req  events.LambdaFunctionURLRequest
		want bool
	}{
		{
			name: "valid signature",
			req: func() events.LambdaFunctionURLRequest {
				ts := "1234567890"
				body := `{"type":1}`
				sig := ed25519.Sign(priv, []byte(ts+body))
				return events.LambdaFunctionURLRequest{
					Headers: map[string]string{
						"x-signature-ed25519":   hex.EncodeToString(sig),
						"x-signature-timestamp": ts,
					},
					Body: body,
				}
			}(),
			want: true,
		},
		{
			name: "missing signature header",
			req: events.LambdaFunctionURLRequest{
				Headers: map[string]string{"x-signature-timestamp": "123"},
				Body:    "{}",
			},
			want: false,
		},
		{
			name: "missing timestamp header",
			req: events.LambdaFunctionURLRequest{
				Headers: map[string]string{"x-signature-ed25519": "abcd"},
				Body:    "{}",
			},
			want: false,
		},
		{
			name: "invalid hex in signature",
			req: events.LambdaFunctionURLRequest{
				Headers: map[string]string{
					"x-signature-ed25519":   "not-hex",
					"x-signature-timestamp": "123",
				},
				Body: "{}",
			},
			want: false,
		},
		{
			name: "tampered body",
			req: func() events.LambdaFunctionURLRequest {
				ts := "1234567890"
				sig := ed25519.Sign(priv, []byte(ts+`{"type":1}`))
				return events.LambdaFunctionURLRequest{
					Headers: map[string]string{
						"x-signature-ed25519":   hex.EncodeToString(sig),
						"x-signature-timestamp": ts,
					},
					Body: `{"type":2}`,
				}
			}(),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := verify(tt.req, pub)
			if got != tt.want {
				t.Errorf("verify() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestInteractionUserID(t *testing.T) {
	tests := []struct {
		name string
		ix   interaction
		want string
	}{
		{
			name: "dm interaction uses user field",
			ix:   interaction{User: &struct{ ID string `json:"id"` }{ID: "user123"}},
			want: "user123",
		},
		{
			name: "guild interaction uses member field",
			ix: interaction{Member: &struct {
				User struct {
					ID string `json:"id"`
				} `json:"user"`
			}{User: struct {
				ID string `json:"id"`
			}{ID: "member456"}}},
			want: "member456",
		},
		{
			name: "no user or member returns empty",
			ix:   interaction{},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.ix.userID()
			if got != tt.want {
				t.Errorf("userID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSaveReminder(t *testing.T) {
	t.Run("writes correct record to dynamo", func(t *testing.T) {
		mock := &mockDynamoDBClient{putItemOutput: &dynamodb.PutItemOutput{}}
		showUnix := int64(1750000000)

		err := saveReminder(context.Background(), mock, "reminders", "user123", showUnix, "DJ Set")
		if err != nil {
			t.Fatalf("saveReminder() error: %v", err)
		}
		if mock.lastInput == nil {
			t.Fatal("PutItem was not called")
		}
		if *mock.lastInput.TableName != "reminders" {
			t.Errorf("table = %q, want %q", *mock.lastInput.TableName, "reminders")
		}
	})

	t.Run("returns dynamo error", func(t *testing.T) {
		mock := &mockDynamoDBClient{err: fmt.Errorf("throttled")}

		err := saveReminder(context.Background(), mock, "reminders", "user123", 1750000000, "DJ Set")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}

func TestHandleHandle(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	loc, _ := time.LoadLocation("America/New_York")

	signRequest := func(body string) events.LambdaFunctionURLRequest {
		ts := "1234567890"
		sig := ed25519.Sign(priv, []byte(ts+body))
		return events.LambdaFunctionURLRequest{
			Headers: map[string]string{
				"x-signature-ed25519":   hex.EncodeToString(sig),
				"x-signature-timestamp": ts,
			},
			Body: body,
		}
	}

	tests := []struct {
		name       string
		req        events.LambdaFunctionURLRequest
		ddbErr     error
		wantStatus int
	}{
		{
			name:       "invalid signature returns 401",
			req:        events.LambdaFunctionURLRequest{Headers: map[string]string{}, Body: "{}"},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "ping returns pong",
			req:        signRequest(`{"type":1}`),
			wantStatus: http.StatusOK,
		},
		{
			name:       "button click saves reminder",
			req:        signRequest(`{"type":3,"data":{"custom_id":"remind:1750000000:DJ Set"},"user":{"id":"user123"}}`),
			wantStatus: http.StatusOK,
		},
		{
			name:       "button click with dynamo error returns 500",
			req:        signRequest(`{"type":3,"data":{"custom_id":"remind:1750000000:DJ Set"},"user":{"id":"user123"}}`),
			ddbErr:     fmt.Errorf("throttled"),
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:       "unknown type returns ack",
			req:        signRequest(`{"type":99}`),
			wantStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockDynamoDBClient{
				putItemOutput: &dynamodb.PutItemOutput{},
				err:           tt.ddbErr,
			}
			h := &Handler{
				publicKey: pub,
				ddbClient: mock,
				ddbTable:  "reminders",
				loc:       loc,
			}

			resp, err := h.Handle(context.Background(), tt.req)
			if err != nil {
				t.Fatalf("Handle() error: %v", err)
			}
			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
		})
	}
}
