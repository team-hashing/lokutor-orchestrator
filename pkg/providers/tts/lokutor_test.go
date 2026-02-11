package tts

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

func TestLokutorTTS(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "closing")

		var req map[string]interface{}
		err = wsjson.Read(r.Context(), conn, &req)
		if err != nil {
			return
		}

		conn.Write(r.Context(), websocket.MessageBinary, []byte{1, 2, 3})
		conn.Write(r.Context(), websocket.MessageBinary, []byte{4, 5, 6})
		conn.Write(r.Context(), websocket.MessageText, []byte("EOS"))
	}))
	defer server.Close()

	tts := &LokutorTTS{
		apiKey: "test-key",
		host:   strings.TrimPrefix(server.URL, "http://"),
		scheme: "ws",
	}

	var audio []byte
	err := tts.StreamSynthesize(context.Background(), "hello", orchestrator.VoiceF1, orchestrator.LanguageEn, func(chunk []byte) error {
		audio = append(audio, chunk...)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(audio) != 6 {
		t.Errorf("expected 6 bytes, got %d", len(audio))
	}

	if tts.Name() != "lokutor" {
		t.Errorf("expected lokutor, got %s", tts.Name())
	}

	tts.Close()
}
