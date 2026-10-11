package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/OwlShack/OwlShack/internal/store"
)

// With the radio down no companion is running, but its stored threads are still there to read.
func TestListConversations_RadioDownListsStoredThreads(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "convos.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c := store.Companion{Name: "home"}
	now := time.Now()
	st.WriteSync(func() {
		if err = st.Companions.Create(ctx, &c); err != nil {
			return
		}
		if err = st.Channels.Create(ctx, &store.CompanionChannel{CompanionID: c.ID, Name: "Public", FloodScope: "inherit"}); err != nil {
			return
		}
		err = st.Messages.Insert(ctx, &store.Message{CompanionID: c.ID, Channel: "Public", Sender: "bob", Text: "hi", Direction: "rx", Timestamp: now, ReceivedAt: now, FloodScope: "everywhere"})
	})
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/companions/home/conversations", nil))
	var got []conversationJSON
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		t.Fatalf("%d %s, want 200 and the stored threads", rec.Code, rec.Body)
	}
	if len(got) != 1 || got[0].Channel != "Public" || got[0].LastMessage == nil || got[0].LastMessage.Text != "hi" {
		t.Errorf("threads %+v, want Public with its last message", got)
	}

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/companions/nobody/conversations", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown companion: %d, want 404", rec.Code)
	}
}
