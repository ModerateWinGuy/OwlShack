package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OwlShack/OwlShack/internal/store"
)

// downBackend runs only the companion named "up"; any other stored companion waits for its radio.
type downBackend struct{ Backend }

func (downBackend) Companion(name string) (MessageSender, DMSender, bool) {
	if name != "up" {
		return nil, nil, false
	}
	return func(string, string) error { return nil }, func(string, string) error { return nil }, true
}

func (downBackend) Repeater(string) (*RepeaterOps, bool) { return nil, false }

// A companion whose radio is reconnecting is unavailable, not missing; a bad DM key is the caller's fault, not a 500.
func TestCompanionDown_AnswersWhatIsWrong(t *testing.T) {
	t.Parallel()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "down.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, n := range []string{"up", "down"} {
		c := store.Companion{Name: n}
		st.WriteSync(func() {
			if err := st.Companions.Create(context.Background(), &c); err != nil {
				t.Fatal(err)
			}
		})
	}
	s := &Server{store: st, mux: http.NewServeMux(), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.routes()
	s.SetBackend(downBackend{})

	post := `{"channel":"#westest","text":"hi"}`
	for _, tc := range []struct {
		name, path, body string
		want             int
	}{
		{"post while the radio reconnects", "/api/companions/down/messages", post, http.StatusServiceUnavailable},
		{"repeater CLI while the radio reconnects", "/api/companions/down/repeaters/aa/cli", `{"command":"ver"}`, http.StatusServiceUnavailable},
		{"no such companion", "/api/companions/ghost/messages", post, http.StatusNotFound},
		{"DM with no key", "/api/companions/up/messages", `{"channel":"dm:","text":"hi"}`, http.StatusBadRequest},
		{"DM with a short key", "/api/companions/up/messages", `{"channel":"dm:abcd","text":"hi"}`, http.StatusBadRequest},
		{"DM with a full key", "/api/companions/up/messages", `{"channel":"dm:` + strings.Repeat("ab", 32) + `","text":"hi"}`, http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))
			if rec.Code != tc.want {
				t.Errorf("POST %s = %d %s, want %d", tc.path, rec.Code, rec.Body.String(), tc.want)
			}
		})
	}
}
