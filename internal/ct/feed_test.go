package ct

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

// The session reuses one buffer for every message, so a short message after a long
// one must not carry the long one's tail, and an unreadable message must be counted
// without dropping the socket.
func TestTheSessionReadsEveryMessageThroughOneBuffer(t *testing.T) {
	long := `{"message_type":"certificate_update","data":{"leaf_cert":{"all_domains":["` +
		strings.Repeat("a", 4000) + `.example"]}}}`
	short := `{"message_type":"certificate_update","data":{"leaf_cert":{"all_domains":["b.example"]}}}`
	messages := []string{long, short, `not json`, short}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		for _, m := range messages {
			if err := conn.Write(r.Context(), websocket.MessageText, []byte(m)); err != nil {
				return
			}
		}
		_ = conn.Close(websocket.StatusNormalClosure, "done")
	}))
	defer server.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	matcher := New(nil, nil, nil, DefaultOptions(), log)
	feed := NewFeed("ws"+strings.TrimPrefix(server.URL, "http"), matcher, log)

	if err := feed.session(context.Background()); websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatalf("the session ended with %v, not the server's close", err)
	}

	matcher.mu.Lock()
	defer matcher.mu.Unlock()
	if matcher.frames != 3 || matcher.malformed != 1 {
		t.Fatalf("handled %d frames and %d unreadable, want 3 and 1", matcher.frames, matcher.malformed)
	}
}
