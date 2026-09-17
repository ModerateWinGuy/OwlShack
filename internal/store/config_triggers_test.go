package store

import (
	"reflect"
	"testing"
)

// A trigger carries a dozen columns and every one is written positionally, so an added field that
// misses one statement is a silent data loss at best and a placeholder-count error at worst. This
// round-trip fails on either.
func TestTriggerRepo_RoundTrip(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	companionID := mkCompanion(t, st, "bot")

	ch := &CompanionChannel{CompanionID: companionID, Name: "Public"}
	if err := st.Channels.Create(t.Context(), ch); err != nil {
		t.Fatalf("Channels.Create: %v", err)
	}

	want := &Trigger{
		CompanionID:        companionID,
		FailoverPattern:    `^@\[{{.Sender | reQuote}}\].+`,
		FailoverTimeout:    10,
		Type:               "cap",
		Template:           "{{.Headline}}",
		CharLimitBehaviour: sptr("truncate"),
		MatchPatterns:      []string{"severity:^(Extreme|Severe)$", "area:(?i)northland"},
		Contacts:           []string{"aabbccdd"},
		RetryTimeout:       i64ptr(7),
		MaxRetries:         iptr(4),
		PathHashSize:       iptr(2),
		Schedule:           sptr("@every 15m"),
		URL:                "https://example.com/cap.atom",
		ChannelIDs:         []int64{ch.ID},
	}
	if err := st.Triggers.Create(t.Context(), want); err != nil {
		t.Fatalf("Triggers.Create: %v", err)
	}

	got, err := st.Triggers.Get(t.Context(), want.ID)
	if err != nil {
		t.Fatalf("Triggers.Get: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after Create/Get:\n got %+v\nwant %+v", got, want)
	}

	want.FailoverPattern = `^pong$`
	want.FailoverTimeout = 20
	want.URL = "https://example.com/other.atom"
	want.Schedule = sptr("@every 2h")
	want.MatchPatterns = []string{"event:(?i)tsunami"}
	if err := st.Triggers.Update(t.Context(), want); err != nil {
		t.Fatalf("Triggers.Update: %v", err)
	}
	if got, err = st.Triggers.Get(t.Context(), want.ID); err != nil {
		t.Fatalf("Triggers.Get after update: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after Update/Get:\n got %+v\nwant %+v", got, want)
	}

	all, err := st.Triggers.List(t.Context())
	if err != nil || len(all) != 1 || !reflect.DeepEqual(&all[0], want) {
		t.Fatalf("List = %+v, error = %v", all, err)
	}

	list, err := st.Triggers.ListByCompanion(t.Context(), companionID)
	if err != nil {
		t.Fatalf("ListByCompanion: %v", err)
	}
	if len(list) != 1 || !reflect.DeepEqual(&list[0], want) {
		t.Fatalf("ListByCompanion returned %+v", list)
	}
}
