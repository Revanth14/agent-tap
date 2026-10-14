package telemetry

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// newTestHub returns a store, its hub, the server and the server's /api/ws URL.
func newTestHub(t *testing.T) (*Store, *Hub, *httptest.Server, string) {
	t.Helper()
	s, err := OpenStore("", 0)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHub(s)
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return s, h, srv, "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/ws"
}

func TestHubServeState(t *testing.T) {
	s, _, srv, _ := newTestHub(t)
	mustNoErr(t, s.UpsertSession(Session{ID: "s1", MaxContextWindow: 200_000}))

	resp, err := http.Get(srv.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var snap StateSnapshot
	mustNoErr(t, json.NewDecoder(resp.Body).Decode(&snap))
	if len(snap.Sessions) != 1 || snap.Sessions[0].ID != "s1" || snap.Sessions[0].MaxContextWindow != 200_000 {
		t.Errorf("snapshot sessions = %+v", snap.Sessions)
	}
}

func TestHubWebSocketSnapshotThenEvents(t *testing.T) {
	s, h, _, wsURL := newTestHub(t)
	mustNoErr(t, s.UpsertSpan(InferenceSpan{ID: "sp1", Status: "decoding"}))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	var first struct {
		Type    EventType     `json:"type"`
		Payload StateSnapshot `json:"payload"`
	}
	mustNoErr(t, conn.ReadJSON(&first))
	if first.Type != EventSnapshot || len(first.Payload.Spans) != 1 || first.Payload.Spans[0].ID != "sp1" {
		t.Fatalf("first event = %+v, want snapshot containing sp1", first)
	}

	// Receiving the snapshot means the client is registered.
	done := InferenceSpan{ID: "sp1", Status: "completed"}
	mustNoErr(t, s.UpsertSpan(done))
	mustNoErr(t, h.Broadcast(EventSpanCompleted, done))

	var ev struct {
		Type    EventType     `json:"type"`
		Payload InferenceSpan `json:"payload"`
	}
	mustNoErr(t, conn.ReadJSON(&ev))
	if ev.Type != EventSpanCompleted || ev.Payload.ID != "sp1" || ev.Payload.Status != "completed" {
		t.Errorf("event = %+v, want span.completed for sp1", ev)
	}
}

func TestHubRejectsCrossOriginWebSocket(t *testing.T) {
	_, _, _, wsURL := newTestHub(t)
	hdr := http.Header{"Origin": {"https://evil.example"}}
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, hdr)
	if err == nil {
		conn.Close()
		t.Fatal("cross-origin handshake succeeded, want rejection")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("response = %v, want 403", resp)
	}
}
