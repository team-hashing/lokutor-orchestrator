package orchestrator

import (
	"context"
	"testing"
	"time"
)

func TestManagedStream_Interruption(t *testing.T) {
	stt := &MockSTTProvider{transcribeResult: "hello"}
	llm := &MockLLMProvider{completeResult: "world"}
	tts := &MockTTSProvider{synthesizeResult: []byte{1, 2, 3}}
	vad := NewRMSVAD(0.1, 100*time.Millisecond)

	orch := NewWithVAD(stt, llm, tts, vad, DefaultConfig())
	session := NewConversationSession("test")

	stream := orch.NewManagedStream(context.Background(), session)
	defer stream.Close()

	// Simulate user speaking
	// We need loud enough samples to trigger RMS VAD
	loudChunk := make([]byte, 100)
	for i := 0; i < 100; i += 2 {
		loudChunk[i] = 0xFF
		loudChunk[i+1] = 0x7F
	}

	err := stream.Write(loudChunk)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// Check for USER_SPEAKING event
	select {
	case ev := <-stream.Events():
		if ev.Type != UserSpeaking {
			t.Errorf("Expected USER_SPEAKING, got %v", ev.Type)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("Timed out waiting for USER_SPEAKING")
	}
}
