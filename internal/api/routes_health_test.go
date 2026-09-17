package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubBackend embeds the interface so only Health needs a body; anything else the handler touched
// would panic loudly rather than pass silently.
type stubBackend struct {
	Backend
	info HealthInfo
}

func (s stubBackend) Health() HealthInfo { return s.info }

func getHealth(t *testing.T, s *Server) (*http.Response, HealthInfo) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	var info HealthInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("decoding %s: %v", rec.Body.String(), err)
	}
	return rec.Result(), info
}

// The status code is deliberately not a second opinion: a monitor that cannot reach OwlShack fails
// the request already, so spending it here would take the choice of what to alert on away.
func TestHealth_AlwaysAnswers200(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		backend  Backend
		wantStat string
	}{
		{"no backend installed", nil, "degraded"},
		{"healthy", stubBackend{}, "ok"},
		{"radio down", stubBackend{info: HealthInfo{Problems: []string{"radio: modem not connected"}}}, "degraded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer(nil, nil, nil)
			if tc.backend != nil {
				s.SetBackend(tc.backend)
			}
			resp, info := getHealth(t, s)
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200 even when unhealthy", resp.StatusCode)
			}
			if info.Status != tc.wantStat {
				t.Errorf("status = %q, want %q", info.Status, tc.wantStat)
			}
		})
	}
}

// A JSON null here would break $count(problems) in a monitor's query, which is the simplest thing
// an operator can write against this endpoint.
func TestHealth_ProblemsIsNeverNull(t *testing.T) {
	t.Parallel()
	s := NewServer(nil, nil, nil)
	s.SetBackend(stubBackend{})

	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if got := string(raw["problems"]); got != "[]" {
		t.Errorf("problems rendered as %s, want []", got)
	}
}

func TestHealth_StampsVersionAndUptime(t *testing.T) {
	t.Parallel()
	s := NewServer(nil, nil, nil)
	s.SetBackend(stubBackend{})
	_, info := getHealth(t, s)

	if info.Version == "" {
		t.Error("version is empty; a monitor cannot tell which build answered")
	}
	if info.UptimeSecs < 0 {
		t.Errorf("uptimeSecs = %d", info.UptimeSecs)
	}
}

// The health endpoint may be reachable from the internet, where a pubkey links a public hostname to
// a mesh identity that public maps resolve to coordinates, and a transport error names a private
// broker's address. Neither has any monitoring value, so neither may reappear in the wire shape —
// this walks the rendered JSON rather than the struct, so an embedded or renamed field is caught too.
func TestHealth_PublishesNoIdentifyingFields(t *testing.T) {
	t.Parallel()
	full := HealthInfo{
		Radio:   RadioHealth{Connected: true, Transport: "kiss"},
		Brokers: []BrokerHealth{{Name: "b", Enabled: true, Connected: true}},
	}
	body, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}

	var tree any
	if err := json.Unmarshal(body, &tree); err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{
		"pubkey", "lasterror", "lat", "lon", "psk", "privatekey", "password",
		"companions", "repeater", // node identities: no monitoring value, and they name the mesh node
	} {
		if found := findKey(tree, banned); found {
			t.Errorf("health JSON carries a %q field: %s", banned, body)
		}
	}
}

// findKey reports whether any object anywhere in the tree has this key, compared case-insensitively.
func findKey(v any, want string) bool {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if strings.EqualFold(k, want) || findKey(child, want) {
				return true
			}
		}
	case []any:
		for _, child := range t {
			if findKey(child, want) {
				return true
			}
		}
	}
	return false
}
