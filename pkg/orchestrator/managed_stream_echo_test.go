package orchestrator

import (
	"context"
	"testing"
	"time"
)

// Ensure that when actual playback samples are recorded by the echo suppressor,
// subsequent mic chunks that match played audio are treated as echo and do
// not cause VAD->UserSpeaking.
func TestManagedStream_PlaybackAlignedEchoDetection(t *testing.T) {
	orch := New(nil, nil, nil, Config{})
	sess := NewConversationSession("test")
	ms := NewManagedStream(context.Background(), orch, sess)

	// install VAD with low threshold so speech detection is easy
	ms.vad = NewRMSVAD(0.02, 50*time.Millisecond)

	// simulate playback: small tone that will be "heard" by mic
	played := make([]byte, 4410*2) // 100ms
	for i := 0; i < len(played)-1; i += 2 {
		val := int16(8000)
		played[i] = byte(val)
		played[i+1] = byte(val >> 8)
	}

	// Tell echo suppressor what was played (as if output thread called RecordPlayedOutput)
	ms.RecordPlayedOutput(played)

	// simulate mic receiving that same tone (echo) — feed via Write
	err := ms.Write(played)
	if err != nil {
		t.Fatal(err)
	}

	// process VAD on the same data — the VAD should see speech but echo suppressor
	// should cause ms to ignore it (i.e., not set ms.isSpeaking)
	// Trigger VADSpeechStart event by writing a small chunk that crosses threshold
	chunk := make([]byte, 1024)
	for i := 0; i < len(chunk)-1; i += 2 {
		val := int16(8000)
		chunk[i] = byte(val)
		chunk[i+1] = byte(val >> 8)
	}

	err = ms.Write(chunk)
	if err != nil {
		t.Fatal(err)
	}

	// after Write where echo detection should have run, the VAD must NOT mark
	// the stream as speaking (we treat it as echo)
	if ms.IsUserSpeaking() {
		t.Fatal("expected echo to be suppressed and not mark user as speaking")
	}
}
