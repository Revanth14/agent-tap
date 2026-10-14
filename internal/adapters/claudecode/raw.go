package claudecode

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"sync"
)

const (
	rawBufferMax  = 64 << 20 // Total bytes kept across all spans
	rawPayloadMax = 8 << 20  // Larger payloads are not kept
)

// rawPayload is one span's raw request and response bytes. They hold prompts
// and file contents, so they live in memory only and are never persisted.
type rawPayload struct {
	Request          []byte
	Response         []byte
	RequestTooLarge  bool
	ResponseTooLarge bool
}

func newRawPayload(req []byte, resp *cappedBuffer) *rawPayload {
	p := &rawPayload{}
	if len(req) > rawPayloadMax {
		p.RequestTooLarge = true
	} else {
		p.Request = req
	}
	if resp != nil {
		if resp.over {
			p.ResponseTooLarge = true
		} else {
			p.Response = resp.buf.Bytes()
		}
	}
	return p
}

func (p *rawPayload) size() int { return len(p.Request) + len(p.Response) }

// rawBuffer keeps recent raw payloads by span ID up to a total byte budget,
// dropping the oldest first.
type rawBuffer struct {
	max int

	mu      sync.Mutex
	entries map[string]*rawPayload
	order   []string // Oldest first
	total   int
}

func newRawBuffer(max int) *rawBuffer {
	return &rawBuffer{max: max, entries: make(map[string]*rawPayload)}
}

func (b *rawBuffer) put(id string, p *rawPayload) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if old, ok := b.entries[id]; ok {
		b.total -= old.size()
		b.order = slices.DeleteFunc(b.order, func(o string) bool { return o == id })
	}
	for b.total+p.size() > b.max && len(b.order) > 0 {
		oldest := b.order[0]
		b.order = b.order[1:]
		b.total -= b.entries[oldest].size()
		delete(b.entries, oldest)
	}
	b.entries[id] = p
	b.order = append(b.order, id)
	b.total += p.size()
}

func (b *rawBuffer) get(id string) (*rawPayload, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.entries[id]
	return p, ok
}

// RawHandler serves GET /api/spans/{id}/raw: the span's raw request and
// response, or 404 once they have left memory.
func (rec *Recorder) RawHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := rec.raw.get(r.PathValue("id"))
		if !ok {
			http.Error(w, "atap: payload no longer in memory", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(struct {
			Request          string `json:"request"`
			Response         string `json:"response"`
			RequestTooLarge  bool   `json:"request_too_large"`
			ResponseTooLarge bool   `json:"response_too_large"`
		}{string(p.Request), string(p.Response), p.RequestTooLarge, p.ResponseTooLarge})
	})
}

// cappedBuffer keeps the first limit bytes written to it and notes whether
// more arrived. Writes never fail, so it can sit behind an io.TeeReader.
type cappedBuffer struct {
	limit int
	buf   bytes.Buffer
	over  bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.over {
		return len(p), nil
	}
	if c.buf.Len()+len(p) > c.limit {
		c.over = true
		c.buf = bytes.Buffer{}
		return len(p), nil
	}
	return c.buf.Write(p)
}
