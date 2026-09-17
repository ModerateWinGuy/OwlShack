package trigger

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/meshcore-go/OwlShack/internal/config"
)

func TestCAPTrigger_DecoratesFromLinkedAlert(t *testing.T) {
	t.Parallel()
	alert, err := os.ReadFile("testdata/cap_alert.xml")
	if err != nil {
		t.Fatal(err)
	}
	fs := newFeedServer(t)
	body := string(alert)
	fs.alertBody.Store(&body)

	tr, fired := newTestCAP(t, config.TriggerConfig{URL: fs.feedURL()})
	tr.poll(context.Background())

	fs.publish("severe-weather")
	tr.poll(context.Background())

	if len(*fired) != 1 {
		t.Fatalf("fired %d events, want 1", len(*fired))
	}
	data := (*fired)[0].Data
	for field, want := range map[string]string{
		"Headline":   "Heavy Rain Warning",
		"Severity":   "Moderate",
		"Urgency":    "Immediate",
		"Certainty":  "Likely",
		"MsgType":    "Update",
		"Status":     "Actual",
		"Event":      "rain",
		"SenderName": "Meteorological Service of New Zealand Limited",
	} {
		if got, _ := data[field].(string); got != want {
			t.Errorf("%s = %q, want %q", field, got, want)
		}
	}
	if areas, _ := data["Areas"].(string); !strings.Contains(areas, "Northland") {
		t.Errorf("Areas = %q, want it to name Northland", areas)
	}
	if expires, _ := data["Expires"].(time.Time); expires.IsZero() {
		t.Error("Expires is zero; templates cannot show when an alert lapses")
	}
}

func TestCAPTrigger_UnfetchableAlertIsNotRetriedForever(t *testing.T) {
	t.Parallel()
	fs := newFeedServer(t)
	fs.alertCode.Store(http.StatusNotFound)

	tr, fired := newTestCAP(t, config.TriggerConfig{URL: fs.feedURL()})
	tr.poll(context.Background())

	fs.publish("gone")
	tr.poll(context.Background())
	tr.poll(context.Background())
	tr.poll(context.Background())

	if len(*fired) != 0 {
		t.Fatalf("fired %d events for an alert that cannot be read", len(*fired))
	}
	if hits := fs.alertHits.Load(); hits != 1 {
		t.Fatalf("fetched the dead alert %d times, want 1: a 404 will not become an alert", hits)
	}
}

func TestCAPTrigger_TransientFailureIsRetried(t *testing.T) {
	t.Parallel()
	fs := newFeedServer(t)
	fs.alertCode.Store(http.StatusServiceUnavailable)

	tr, fired := newTestCAP(t, config.TriggerConfig{URL: fs.feedURL()})
	tr.poll(context.Background())

	fs.publish("flaky")
	tr.poll(context.Background())
	if len(*fired) != 0 {
		t.Fatalf("fired despite a 503")
	}

	alert, err := os.ReadFile("testdata/cap_alert.xml")
	if err != nil {
		t.Fatal(err)
	}
	body := string(alert)
	fs.alertBody.Store(&body)
	fs.alertCode.Store(http.StatusOK)

	tr.poll(context.Background())
	if len(*fired) != 1 {
		t.Fatalf("fired %d events once the publisher recovered, want 1", len(*fired))
	}
}

// The fixture is a *Moderate* alert whose instruction text reads "A Severe Weather Warning ...
// favourable for severe weather". Matching one regex over every field joined would let a
// severity:Severe filter through on that prose alone, which is why patterns name a field.
func TestCAPTrigger_SeverityFilterIgnoresTheWordInProse(t *testing.T) {
	t.Parallel()
	alert, err := os.ReadFile("testdata/cap_alert.xml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(alert), "Severe Weather Warning") {
		t.Fatal("fixture no longer contains the prose this test turns on")
	}
	body := string(alert)

	cases := []struct {
		name    string
		pattern string
		want    bool
	}{
		{"its own severity", `severity:^Moderate$`, true},
		{"a higher severity must not leak through the instruction text", `severity:(?i)severe`, false},
		{"its urgency", `urgency:^Immediate$`, true},
		{"its message type", `msgtype:^(Alert|Update)$`, true},
		{"the word in the field that really holds it", `instruction:(?i)severe weather`, true},
		{"its event", `event:(?i)rain`, true},
		{"an event it is not", `event:(?i)tsunami`, false},
		{"its area", `area:(?i)northland`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := newFeedServer(t)
			fs.alertBody.Store(&body)
			match := []string{c.pattern}
			tr, fired := newTestCAP(t, config.TriggerConfig{URL: fs.feedURL(), Match: &match})
			tr.poll(context.Background())
			fs.publish("wx")
			tr.poll(context.Background())

			if got := len(*fired) == 1; got != c.want {
				t.Errorf("pattern %s fired=%v, want %v", c.pattern, got, c.want)
			}
		})
	}
}

func TestCAPTrigger_FieldsNarrowTogether(t *testing.T) {
	t.Parallel()
	alert, err := os.ReadFile("testdata/cap_alert.xml")
	if err != nil {
		t.Fatal(err)
	}
	body := string(alert)

	// Severity holds, area does not: narrowing by both must reject the alert.
	fs := newFeedServer(t)
	fs.alertBody.Store(&body)
	match := []string{`severity:^Moderate$`, `area:(?i)canterbury`}
	tr, fired := newTestCAP(t, config.TriggerConfig{URL: fs.feedURL(), Match: &match})
	tr.poll(context.Background())
	fs.publish("wx")
	tr.poll(context.Background())

	if len(*fired) != 0 {
		t.Fatalf("fired with only one of two fields satisfied")
	}
}
