package orchestrator

import (
	"bytes"
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
func TestManagedStream_EndToEndLatency(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ms := &ManagedStream{
		events:  make(chan OrchestratorEvent, 10),
		session: &ConversationSession{ID: "test"},
		ctx:     ctx,
	}

	base := time.Now()
	start := base
	played := base.Add(250 * time.Millisecond)

	ms.mu.Lock()
	ms.userSpeechEndTime = start
	ms.lastAudioSentAt = played
	ms.mu.Unlock()

	if got := ms.GetEndToEndLatency(); got != int64(250) {
		t.Fatalf("expected 250ms, got %dms", got)
	}
}

func TestManagedStream_LatencyBreakdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ms := &ManagedStream{
		events:  make(chan OrchestratorEvent, 10),
		session: &ConversationSession{ID: "test"},
		ctx:     ctx,
	}

	base := time.Now()
	ms.mu.Lock()
	ms.userSpeechEndTime = base
	ms.sttStartTime = base.Add(10 * time.Millisecond)
	ms.sttEndTime = base.Add(110 * time.Millisecond) // STT = 100ms
	ms.llmStartTime = base.Add(130 * time.Millisecond)
	ms.llmEndTime = base.Add(380 * time.Millisecond) // LLM = 250ms
	ms.ttsStartTime = base.Add(400 * time.Millisecond)
	ms.ttsFirstChunkTime = base.Add(520 * time.Millisecond) // first TTS = 120ms after ttsStart
	ms.ttsEndTime = base.Add(900 * time.Millisecond)        // TTS total = 500ms
	ms.botSpeakStartTime = base.Add(395 * time.Millisecond)
	ms.lastAudioSentAt = base.Add(525 * time.Millisecond)
	ms.mu.Unlock()

	bd := ms.GetLatencyBreakdown()

	if bd.UserToSTT != int64(110) {
		t.Fatalf("expected UserToSTT 110ms, got %d", bd.UserToSTT)
	}
	if bd.STT != int64(100) {
		t.Fatalf("expected STT 100ms, got %d", bd.STT)
	}
	if bd.UserToLLM != int64(380) {
		t.Fatalf("expected UserToLLM 380ms, got %d", bd.UserToLLM)
	}
	if bd.LLM != int64(250) {
		t.Fatalf("expected LLM 250ms, got %d", bd.LLM)
	}
	if bd.UserToTTSFirstByte != int64(520) {
		t.Fatalf("expected UserToTTSFirstByte 520ms, got %d", bd.UserToTTSFirstByte)
	}
	if bd.LLMToTTSFirstByte != int64(140) {
		t.Fatalf("expected LLMToTTSFirstByte 140ms, got %d", bd.LLMToTTSFirstByte)
	}
	if bd.TTSTotal != int64(500) {
		t.Fatalf("expected TTSTotal 500ms, got %d", bd.TTSTotal)
	}
	if bd.BotStartLatency != int64(395) {
		t.Fatalf("expected BotStartLatency 395ms, got %d", bd.BotStartLatency)
	}
	if bd.UserToPlay != int64(525) {
		t.Fatalf("expected UserToPlay 525ms, got %d", bd.UserToPlay)
	}
}

func TestManagedStream_ExportLastUserAudio(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ms := &ManagedStream{
		events:  make(chan OrchestratorEvent, 10),
		session: &ConversationSession{ID: "test"},
		ctx:     ctx,
	}

	// prepare played tone and mic (attenuated echo + user)
	played := make([]byte, 44100/10*2)
	for i := 0; i < len(played)-1; i += 2 {
		val := int16(10000)
		played[i] = byte(val)
		played[i+1] = byte(val >> 8)
	}

	atten := make([]byte, len(played))
	for i := 0; i < len(played)-1; i += 2 {
		s := int16(played[i]) | (int16(played[i+1]) << 8)
		s = int16(float64(s) * 0.25)
		atten[i] = byte(s)
		atten[i+1] = byte(s >> 8)
	}

	user := make([]byte, 44100/20*2)
	for i := 0; i < len(user)-1; i += 2 {
		user[i] = 0x40
		user[i+1] = 0x00
	}

	mic := append([]byte{}, atten...)
	mic = append(mic, user...)

	ms.echoSuppressor = NewEchoSuppressor()
	ms.echoSuppressor.RecordPlayedAudio(played)
	ms.mu.Lock()
	ms.lastUserAudio = make([]byte, len(mic))
	copy(ms.lastUserAudio, mic)
	ms.mu.Unlock()

	raw, processed := ms.ExportLastUserAudio()
	if raw == nil || processed == nil {
		t.Fatal("expected non-nil raw and processed")
	}
	if len(raw) != len(mic) {
		t.Fatalf("raw len mismatch: %d vs %d", len(raw), len(mic))
	}

	before := pcmEnergy(raw[:len(played)])
	after := pcmEnergy(processed[:len(played)])
	if after > before*0.5 {
		t.Fatalf("expected echo reduced by >50%%; before=%v after=%v", before, after)
	}
}

func TestManagedStream_DropsEchoBeforeSTT(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ms := &ManagedStream{
		events:         make(chan OrchestratorEvent, 10),
		session:        &ConversationSession{ID: "test"},
		ctx:            ctx,
		echoSuppressor: NewEchoSuppressor(),
		audioBuf:       new(bytes.Buffer),
	}
	ms.vad = NewRMSVAD(0.02, 50*time.Millisecond)

	// Simulate playback then mic echo
	played := make([]byte, 4410*2) // 100ms
	for i := 0; i < len(played)-1; i += 2 {
		val := int16(8000)
		played[i] = byte(val)
		played[i+1] = byte(val >> 8)
	}

	ms.RecordPlayedOutput(played)

	// create a buffered sttChan to observe forwarded audio
	ch := make(chan []byte, 4)
	ms.mu.Lock()
	ms.sttChan = ch
	ms.mu.Unlock()

	// write a chunk that is playback-echo — should be dropped and NOT sent to sttChan
	err := ms.Write(played)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-ch:
		t.Fatal("expected no data forwarded to STT for echo chunk")
	default:
		// OK
	}

	// lastUserAudio should not include the echo
	ms.mu.Lock()
	if len(ms.lastUserAudio) != 0 {
		ts := len(ms.lastUserAudio)
		ms.mu.Unlock()
		t.Fatalf("expected lastUserAudio to be empty, got %d bytes", ts)
	}
	ms.mu.Unlock()
}
