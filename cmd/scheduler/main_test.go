package main

import (
	"testing"
	"time"
)

func TestFormatTime(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")

	tests := []struct {
		name string
		time time.Time
		want string
	}{
		{name: "midnight", time: time.Date(2025, 1, 1, 0, 0, 0, 0, loc), want: "12am"},
		{name: "noon", time: time.Date(2025, 1, 1, 12, 0, 0, 0, loc), want: "12pm"},
		{name: "1pm", time: time.Date(2025, 1, 1, 13, 0, 0, 0, loc), want: "1pm"},
		{name: "9am", time: time.Date(2025, 1, 1, 9, 0, 0, 0, loc), want: "9am"},
		{name: "with minutes", time: time.Date(2025, 1, 1, 14, 30, 0, 0, loc), want: "2:30pm"},
		{name: "11:59pm", time: time.Date(2025, 1, 1, 23, 59, 0, 0, loc), want: "11:59pm"},
		{name: "12:05am", time: time.Date(2025, 1, 1, 0, 5, 0, 0, loc), want: "12:05am"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatTime(tt.time)
			if got != tt.want {
				t.Errorf("formatTime() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCleanDescription(t *testing.T) {
	tests := []struct {
		name string
		desc string
		want string
	}{
		{name: "plain text", desc: "A great show about jazz", want: "A great show about jazz"},
		{name: "br tags join with space", desc: "Line one<br>Line two", want: "Line one Line two"},
		{name: "stops at html tag", desc: "Good part<br><a href=\"x\">link</a>", want: "Good part"},
		{name: "stops at url", desc: "Good part<br>https://example.com", want: "Good part"},
		{name: "html entities", desc: "Rock &amp; Roll", want: "Rock & Roll"},
		{name: "empty", desc: "", want: ""},
		{name: "only html", desc: "<div>stuff</div>", want: ""},
		{name: "multiple entities", desc: "&lt;tag&gt; &amp; &quot;quoted&quot; &#39;apos&#39;", want: "<tag> & \"quoted\" 'apos'"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cleanDescription(tt.desc)
			if got != tt.want {
				t.Errorf("cleanDescription() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFilterAndSortEntries(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")

	events := []Event{
		{Summary: "Show B", Start: "2025-06-15T18:00:00-04:00", End: "2025-06-15T20:00:00-04:00"},
		{Summary: "Show A", Start: "2025-06-15T10:00:00-04:00", End: "2025-06-15T12:00:00-04:00"},
		{Summary: "Tomorrow Show", Start: "2025-06-16T10:00:00-04:00", End: "2025-06-16T12:00:00-04:00"},
		{Summary: "RESTREAM", Start: "2025-06-15T14:00:00-04:00", End: "2025-06-15T16:00:00-04:00"},
		{Summary: "  restream  ", Start: "2025-06-15T15:00:00-04:00", End: "2025-06-15T17:00:00-04:00"},
	}

	t.Run("filters to today and sorts by start time", func(t *testing.T) {
		entries := filterAndSortEntries(events, loc, "2025-06-15")

		if len(entries) != 2 {
			t.Fatalf("got %d entries, want 2", len(entries))
		}
		if entries[0].name != "Show A" {
			t.Errorf("first entry = %q, want %q", entries[0].name, "Show A")
		}
		if entries[1].name != "Show B" {
			t.Errorf("second entry = %q, want %q", entries[1].name, "Show B")
		}
	})

	t.Run("no matching date returns empty", func(t *testing.T) {
		entries := filterAndSortEntries(events, loc, "2025-06-20")
		if len(entries) != 0 {
			t.Errorf("got %d entries, want 0", len(entries))
		}
	})

	t.Run("skips events with bad timestamps", func(t *testing.T) {
		bad := []Event{{Summary: "Bad", Start: "not-a-date", End: "2025-06-15T12:00:00-04:00"}}
		entries := filterAndSortEntries(bad, loc, "2025-06-15")
		if len(entries) != 0 {
			t.Errorf("got %d entries, want 0", len(entries))
		}
	})
}

func TestParseSchedule(t *testing.T) {
	t.Run("extracts schedule from next_f chunks", func(t *testing.T) {
		// Simulate the __next_f format with a minimal schedule payload.
		html := `<script>self.__next_f.push([1, "{\"schedule\": [{\"summary\": \"DJ Set\", \"start\": \"2025-06-15T10:00:00-04:00\", \"end\": \"2025-06-15T12:00:00-04:00\", \"description\": \"\", \"genres\": [\"House\"]}], \"enabled\": true}"])</script>`

		events, err := parseSchedule(html)
		if err != nil {
			t.Fatalf("parseSchedule() error: %v", err)
		}
		if len(events) != 1 {
			t.Fatalf("got %d events, want 1", len(events))
		}
		if events[0].Summary != "DJ Set" {
			t.Errorf("summary = %q, want %q", events[0].Summary, "DJ Set")
		}
		if len(events[0].Genres) != 1 || events[0].Genres[0] != "House" {
			t.Errorf("genres = %v, want [House]", events[0].Genres)
		}
	})

	t.Run("returns error when no schedule found", func(t *testing.T) {
		_, err := parseSchedule("<html><body>no data here</body></html>")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}
