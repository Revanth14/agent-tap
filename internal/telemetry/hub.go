package telemetry

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// EventType is the WSEvent.Type of a broadcast.
type EventType string

const (
	EventSnapshot       EventType = "snapshot"
	EventSessionUpdated EventType = "session.updated"
	EventAgentUpserted  EventType = "agent.upserted"
	EventSpanStarted    EventType = "span.started"
	EventSpanDelta      EventType = "span.delta"
	EventSpanCompleted  EventType = "span.completed"
	EventToolUpdated    EventType = "tool.updated"
	EventUsageUpdated   EventType = "usage.updated"
)

const (
	clientSendBuffer = 256
	writeTimeout     = 10 * time.Second
)

// Hub serves state over REST and fans WSEvents out to WebSocket clients.
type Hub struct {
	store *Store
	// The upgrader's default CheckOrigin rejects cross-origin handshakes, so
	// an arbitrary web page cannot subscribe to the telemetry stream.
	upgrader websocket.Upgrader

	mu      sync.Mutex
	clients map[*wsClient]struct{}
}

type wsClient struct {
	conn *websocket.Conn
	send chan []byte // closed by the hub when the client is removed
}

// NewHub returns a hub serving state from store.
func NewHub(store *Store) *Hub {
	return &Hub{store: store, clients: make(map[*wsClient]struct{})}
}

// Register mounts GET /api/state, GET /api/statusline and GET /api/ws on mux.
func (h *Hub) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/state", h.serveState)
	mux.HandleFunc("GET /api/statusline", h.serveStatusLine)
	mux.HandleFunc("GET /api/ws", h.serveWS)
}

// serveState writes the current StateSnapshot as JSON.
func (h *Hub) serveState(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(h.store.Snapshot()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// serveStatusLine writes the status line for the session named by the
// session query parameter, as plain text.
func (h *Hub) serveStatusLine(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprint(w, h.store.StatusLine(r.URL.Query().Get("session"), time.Now()))
}

// serveWS upgrades to a WebSocket, sends a snapshot event, then streams every
// broadcast event until the client disconnects or falls behind.
func (h *Hub) serveWS(w http.ResponseWriter, r *http.Request) {
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade has already written the HTTP error
	}
	c := &wsClient{conn: conn, send: make(chan []byte, clientSendBuffer)}

	// Snapshot and registration happen under h.mu, which Broadcast also takes.
	// Because writers update the store before broadcasting, every change is
	// either in this snapshot or delivered as an event after it; some may be
	// both, which upsert-style events tolerate.
	h.mu.Lock()
	msg, err := encodeEvent(EventSnapshot, h.store.Snapshot())
	if err != nil {
		h.mu.Unlock()
		conn.Close()
		return
	}
	c.send <- msg
	h.clients[c] = struct{}{}
	h.mu.Unlock()

	go c.writeLoop()
	c.readLoop()
	h.remove(c)
}

// Broadcast sends an event to every connected client. A client whose buffer
// is full is disconnected rather than allowed to stall the publisher.
func (h *Hub) Broadcast(eventType EventType, payload any) error {
	msg, err := encodeEvent(eventType, payload)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		select {
		case c.send <- msg:
		default:
			delete(h.clients, c)
			close(c.send)
		}
	}
	return nil
}

func (h *Hub) remove(c *wsClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		close(c.send)
	}
}

func encodeEvent(eventType EventType, payload any) ([]byte, error) {
	msg, err := json.Marshal(WSEvent{Type: string(eventType), Timestamp: time.Now().UTC(), Payload: payload})
	if err != nil {
		return nil, fmt.Errorf("telemetry: encode %s event: %w", eventType, err)
	}
	return msg, nil
}

// readLoop discards client messages and returns once the connection fails or
// closes. Reading is required for gorilla to process close frames.
func (c *wsClient) readLoop() {
	c.conn.SetReadLimit(4096)
	for {
		if _, _, err := c.conn.NextReader(); err != nil {
			return
		}
	}
}

// writeLoop drains c.send to the connection until the hub closes the channel
// or a write fails, then closes the connection, which ends readLoop.
func (c *wsClient) writeLoop() {
	defer c.conn.Close()
	for msg := range c.send {
		c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			return
		}
	}
	c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	c.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
}
