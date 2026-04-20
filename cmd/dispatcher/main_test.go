package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
)

func TestSendDM(t *testing.T) {
	t.Run("successful dm", func(t *testing.T) {
		var reqPaths []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqPaths = append(reqPaths, r.URL.Path)

			if r.URL.Path == "/users/@me/channels" {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"id":"chan123"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		// Override the module-level const by shadowing via the test's scope.
		origBase := discordAPIBase
		discordAPIBase = srv.URL
		defer func() { discordAPIBase = origBase }()

		err := sendDM(context.Background(), "test-token", "user456", "hello!")
		if err != nil {
			t.Fatalf("sendDM() error: %v", err)
		}
		if len(reqPaths) != 2 {
			t.Fatalf("got %d requests, want 2", len(reqPaths))
		}
		if reqPaths[0] != "/users/@me/channels" {
			t.Errorf("first request = %q, want /users/@me/channels", reqPaths[0])
		}
	})

	t.Run("dm channel creation fails", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer srv.Close()

		origBase := discordAPIBase
		discordAPIBase = srv.URL
		defer func() { discordAPIBase = origBase }()

		err := sendDM(context.Background(), "test-token", "user456", "hello!")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}

func TestHandlerHandle(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")

	t.Run("skips non-REMOVE events", func(t *testing.T) {
		h := &Handler{botToken: "tok", loc: loc}
		e := events.DynamoDBEvent{
			Records: []events.DynamoDBEventRecord{
				{EventName: "INSERT"},
			},
		}

		err := h.Handle(context.Background(), e)
		if err != nil {
			t.Fatalf("Handle() error: %v", err)
		}
	})

	t.Run("skips records with nil OldImage", func(t *testing.T) {
		h := &Handler{botToken: "tok", loc: loc}
		e := events.DynamoDBEvent{
			Records: []events.DynamoDBEventRecord{
				{EventName: "REMOVE", Change: events.DynamoDBStreamRecord{OldImage: nil}},
			},
		}

		err := h.Handle(context.Background(), e)
		if err != nil {
			t.Fatalf("Handle() error: %v", err)
		}
	})

	t.Run("skips records with missing fields", func(t *testing.T) {
		h := &Handler{botToken: "tok", loc: loc}
		e := events.DynamoDBEvent{
			Records: []events.DynamoDBEventRecord{
				{
					EventName: "REMOVE",
					Change: events.DynamoDBStreamRecord{
						OldImage: map[string]events.DynamoDBAttributeValue{
							"user_id":   events.NewStringAttribute(""),
							"show_name": events.NewStringAttribute("DJ Set"),
							"remind_at": events.NewNumberAttribute("1750000000"),
						},
					},
				},
			},
		}

		err := h.Handle(context.Background(), e)
		if err != nil {
			t.Fatalf("Handle() error: %v", err)
		}
	})
}
