package orchestrator

import (
	"context"
	"testing"
	"time"
)

func TestManagedStream_InterruptionLogic(t *testing.T) {
	orch := New(nil, nil, nil, Config{})
	session := NewConversationSession("test")
	ms := NewManagedStream(context.Background(), orch, session)

	
	ms.vad = NewRMSVAD(0.1, 100*time.Millisecond)

	
	ms.mu.Lock()
	ms.isThinking = true
	ms.mu.Unlock()

	
	ms.mu.Lock()
	ms.internalInterrupt()
	ms.mu.Unlock()

	if ms.isThinking {
		t.Error("isThinking should be false after interruption")
	}
	if ms.isSpeaking {
		t.Error("isSpeaking should be false after interruption")
	}

	
	select {
	case ev := <-ms.events:
		if ev.Type != Interrupted {
			t.Errorf("expected Interrupted event, got %v", ev.Type)
		}
	default:
		t.Error("expected Interrupted event in channel")
	}
}

func TestManagedStream_EchoGuard(t *testing.T) {
	orch := New(nil, nil, nil, Config{})
	session := NewConversationSession("test")
	ms := NewManagedStream(context.Background(), orch, session)

	
	vad := NewRMSVAD(0.02, 100*time.Millisecond)
	ms.vad = vad

	
	if vad.Threshold() != 0.02 {
		t.Errorf("expected threshold 0.02, got %f", vad.Threshold())
	}

	
	ms.NotifyAudioPlayed()

	
	
	
	

	
	chunk := make([]byte, 200)
	for i := 0; i < len(chunk); i += 2 {
		
		val := int16(3276)
		chunk[i] = byte(val)
		chunk[i+1] = byte(val >> 8)
	}

	err := ms.Write(chunk)
	if err != nil {
		t.Fatal(err)
	}

	if ms.isSpeaking {
		t.Error("should NOT be speaking due to Echo Guard threshold (0.25)")
	}

	
	ms.mu.Lock()
	ms.lastAudioSentAt = time.Now().Add(-500 * time.Millisecond)
	ms.mu.Unlock()

	err = ms.Write(chunk)
	if err != nil {
		t.Fatal(err)
	}

	
	if vad.IsSpeaking() {
		
	} else {
		
		
		
	}
}

func TestManagedStream_StaleAudioDiscard(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ms := &ManagedStream{
		events:  make(chan OrchestratorEvent, 10),
		session: &ConversationSession{ID: "test"},
		ctx:     ctx,
	}

	
	ms.isSpeaking = false
	ms.emit(AudioChunk, []byte("stale"))

	select {
	case <-ms.events:
		t.Error("should have discarded audio chunk when not speaking")
	default:
		
	}

	
	ms.isSpeaking = true
	ms.emit(AudioChunk, []byte("fresh"))

	select {
	case ev := <-ms.events:
		if ev.Type != AudioChunk {
			t.Error("expected AudioChunk")
		}
	default:
		t.Error("should have emitted audio chunk when speaking")
	}
}
