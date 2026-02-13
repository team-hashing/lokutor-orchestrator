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

	
	
	loudChunk := make([]byte, 100)
	for i := 0; i < 100; i += 2 {
		loudChunk[i] = 0xFF
		loudChunk[i+1] = 0x7F
	}

	
	for i := 0; i < 20; i++ {
		stream.Write(loudChunk)
	}

	
	select {
	case ev := <-stream.Events():
		if ev.Type != UserSpeaking {
			t.Errorf("Expected USER_SPEAKING, got %v", ev.Type)
		}
	case <-time.After(500 * time.Millisecond): 
		t.Error("Timed out waiting for USER_SPEAKING")
	}
}

func TestManagedStream_EchoSuppression(t *testing.T) {
	stt := &MockSTTProvider{transcribeResult: "hello"}
	llm := &MockLLMProvider{completeResult: "world"}
	tts := &MockTTSProvider{synthesizeResult: []byte{1, 2, 3}}
	
	vad := NewRMSVAD(0.1, 100*time.Millisecond)

	orch := NewWithVAD(stt, llm, tts, vad, DefaultConfig())
	session := NewConversationSession("test")

	stream := orch.NewManagedStream(context.Background(), session)
	defer stream.Close()

	
	stream.mu.Lock()
	stream.lastAudioSentAt = time.Now()
	stream.mu.Unlock()

	
	
	loudChunk := make([]byte, 100)
	for i := 0; i < 100; i += 2 {
		
		val := int16(32768.0 * 0.25)
		loudChunk[i] = byte(val & 0xFF)
		loudChunk[i+1] = byte(val >> 8)
	}

	
	for i := 0; i < 20; i++ {
		stream.Write(loudChunk)
	}

	
	select {
	case ev := <-stream.Events():
		if ev.Type == UserSpeaking {
			t.Errorf("Echo Guard FAILED: Detected UserSpeaking for audio below echo threshold")
		}
	case <-time.After(100 * time.Millisecond):
		
	}

	
	stream.mu.Lock()
	stream.lastAudioSentAt = time.Now().Add(-5 * time.Second)
	stream.mu.Unlock()

	
	for i := 0; i < 20; i++ {
		stream.Write(loudChunk)
	}

	select {
	case ev := <-stream.Events():
		if ev.Type != UserSpeaking {
			t.Errorf("Expected USER_SPEAKING after danger zone, got %v", ev.Type)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("Timed out waiting for USER_SPEAKING after danger zone")
	}
}
