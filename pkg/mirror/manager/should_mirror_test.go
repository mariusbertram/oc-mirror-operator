package manager

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
)

func newTestManager(token string, states map[string]imagestate.ImageState) *MirrorManager {
	return &MirrorManager{
		workerToken: token,
		imageStates: states,
	}
}

func doShouldMirror(t *testing.T, m *MirrorManager, dest, token string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/should-mirror?dest="+dest, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	m.handleShouldMirror(rr, req)
	return rr.Code
}

func TestHandleShouldMirror(t *testing.T) {
	const tok = "secret-token"
	states := map[string]imagestate.ImageState{
		"is1": {
			"reg/repo:pending":  {State: "Pending"},
			"reg/repo:mirrored": {State: "Mirrored"},
			"reg/repo:failed":   {State: "Failed"},
		},
	}
	m := newTestManager(tok, states)

	cases := []struct {
		name     string
		dest     string
		token    string
		method   string
		wantCode int
	}{
		{"pending → 200", "reg/repo:pending", tok, http.MethodGet, http.StatusOK},
		{"failed → 200", "reg/repo:failed", tok, http.MethodGet, http.StatusOK},
		{"mirrored → 410", "reg/repo:mirrored", tok, http.MethodGet, http.StatusGone},
		{"removed/unknown → 410", "reg/repo:gone", tok, http.MethodGet, http.StatusGone},
		{"missing token → 401", "reg/repo:pending", "", http.MethodGet, http.StatusUnauthorized},
		{"wrong token → 401", "reg/repo:pending", "wrong", http.MethodGet, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := doShouldMirror(t, m, tc.dest, tc.token)
			if got != tc.wantCode {
				t.Fatalf("got %d, want %d", got, tc.wantCode)
			}
		})
	}

	t.Run("missing dest → 400", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/should-mirror", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rr := httptest.NewRecorder()
		m.handleShouldMirror(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("got %d, want 400", rr.Code)
		}
	})

	t.Run("wrong method → 405", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/should-mirror?dest=x", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rr := httptest.NewRecorder()
		m.handleShouldMirror(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("got %d, want 405", rr.Code)
		}
	})
}
