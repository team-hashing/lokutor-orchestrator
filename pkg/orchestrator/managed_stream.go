package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

type ManagedStream struct {
	orch    *Orchestrator
	session *ConversationSession
	ctx     context.Context
	cancel  context.CancelFunc
	events  chan OrchestratorEvent
	vad     VADProvider

	audioBuf *bytes.Buffer
	mu       sync.Mutex

	pipelineCtx       context.Context
	pipelineCancel    context.CancelFunc
	sttChan           chan<- []byte
	sttGeneration     int // Version number to detect stale STT callbacks
	isSpeaking        bool
	isThinking        bool
	lastInterruptedAt time.Time
	lastAudioSentAt   time.Time
	userSpeechEndTime time.Time // When user stopped speaking (VADSpeechEnd)
	botSpeakStartTime time.Time // When bot started TTS playback

	// Last captured user turn audio (raw PCM). Filled when STT starts or during
	// streaming STT so the CLI can export raw + postprocessed audio for debugging.
	lastUserAudio []byte

	// Per-turn instrumentation timestamps (set/cleared each user turn)
	sttStartTime      time.Time // when STT started (batch or streaming)
	sttEndTime        time.Time // when final transcript was produced
	llmStartTime      time.Time // when LLM generation started
	llmEndTime        time.Time // when LLM generation finished
	ttsStartTime      time.Time // when TTS synthesis began
	ttsFirstChunkTime time.Time // when first audio chunk was emitted by TTS
	ttsEndTime        time.Time // when TTS finished

	responseCancel     context.CancelFunc
	ttsCancel          context.CancelFunc // Track TTS context for fast abort
	userInterrupting   bool               // Flag to block audio emission during user barge-in
	echoSuppressor     *EchoSuppressor    // Echo detection and suppression
	lastAudioEmittedAt time.Time          // When the very last audio chunk was sent to speaker
	closeOnce          sync.Once
}

// NewManagedStream creates a stream that handles the audio & event pipeline
// for a conversation.  The internal echo suppressor is created with the
// default 44.1 kHz rate; callers who need custom rates can adjust it after
// construction (see SetEchoSampleRates) or create their own suppressor and
// assign it to `ms.echoSuppressor`.
func NewManagedStream(ctx context.Context, o *Orchestrator, session *ConversationSession) *ManagedStream {
	mCtx, mCancel := context.WithCancel(ctx)

	var streamVAD VADProvider
	if o.vad != nil {
		streamVAD = o.vad.Clone()
	}

	ms := &ManagedStream{
		orch:           o,
		session:        session,
		ctx:            mCtx,
		cancel:         mCancel,
		events:         make(chan OrchestratorEvent, 1024),
		audioBuf:       new(bytes.Buffer),
		vad:            streamVAD,
		echoSuppressor: NewEchoSuppressor(),
	}

	return ms
}

// LastRMS returns the last RMS value computed by the stream's internal VAD
// (returns 0.0 when unavailable).
func (ms *ManagedStream) LastRMS() float64 {
	if ms.vad == nil {
		return 0.0
	}
	if rmsVAD, ok := ms.vad.(*RMSVAD); ok {
		return rmsVAD.LastRMS()
	}
	return 0.0
}

// IsUserSpeaking reports the internal VAD speaking state for this stream.
func (ms *ManagedStream) IsUserSpeaking() bool {
	if ms.vad == nil {
		return false
	}
	if rmsVAD, ok := ms.vad.(*RMSVAD); ok {
		return rmsVAD.IsSpeaking()
	}
	return false
}

// SetEchoSampleRates allows callers to configure the playback/input rates used
// by the stream's echo suppressor.  It is safe to call at any time; the
// suppressor will resize buffers and update frame sizing as needed.
func (ms *ManagedStream) SetEchoSampleRates(playbackRate, inputRate int) {
	if ms.echoSuppressor != nil {
		ms.echoSuppressor.SetSampleRates(playbackRate, inputRate)
	}
}

// Interrupt immediately stops the bot from speaking. This is an explicit way to
// interrupt regardless of VAD state - useful for UI buttons or external signals.
// It clears audio playback, cancels TTS/LLM, and emits an Interrupted event.
func (ms *ManagedStream) Interrupt() {
	ms.mu.Lock()
	ms.userInterrupting = true
	ms.mu.Unlock()
	ms.internalInterrupt()
}

// countWords returns the number of whitespace-separated words in s.
func countWords(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	return len(strings.Fields(s))
}

const speechEndHold = 150 * time.Millisecond

func (ms *ManagedStream) Write(chunk []byte) error {
	if ms.vad == nil {
		return fmt.Errorf("VAD not configured for this stream")
	}

	// Write processes an incoming audio chunk from the microphone

	// Temporarily adjust VAD threshold when recent audio was played.
	if rmsVAD, ok := ms.vad.(*RMSVAD); ok {
		originalThreshold := rmsVAD.Threshold()
		originalMinConfirmed := rmsVAD.MinConfirmed()

		ms.mu.Lock()
		speaking := ms.isSpeaking
		isThinking := ms.isThinking
		ms.mu.Unlock()

		// During active speaking, thinking, or the immediate audio trail, we want to be
		// sensitive to barge-in (2 frames) but keep a safe threshold (0.10)
		// to avoid echo-triggering.
		lastEmitted := ms.lastAudioEmittedAt
		inTrail := time.Since(lastEmitted) < 1500*time.Millisecond
		if speaking || isThinking || inTrail {
			if originalMinConfirmed > 2 {
				rmsVAD.SetMinConfirmed(2)
			}
			rmsVAD.SetAdaptiveMode(false)
			rmsVAD.SetThreshold(0.04)
		}

		defer func() {
			rmsVAD.SetThreshold(originalThreshold)
			rmsVAD.SetMinConfirmed(originalMinConfirmed)
			rmsVAD.SetAdaptiveMode(true)
		}()
	}

	if ms.echoSuppressor != nil {
		chunk = ms.echoSuppressor.RemoveEchoRealtime(chunk)
	}

	event, err := ms.vad.Process(chunk)
	if err != nil {
		return err
	}

	if event != nil && event.Type != VADSilence {
		switch event.Type {
		case VADSpeechStart:
			ms.mu.Lock()
			lead := ms.audioBuf.Bytes()
			speaking := ms.isSpeaking
			isThinking := ms.isThinking
			lastEmitted := ms.lastAudioEmittedAt
			ms.mu.Unlock()

			leadBytes := 8820 // ~100ms @44.1kHz
			if len(lead) > leadBytes {
				lead = lead[len(lead)-leadBytes:]
			}
			checkBuf := make([]byte, 0, len(lead)+len(chunk))
			checkBuf = append(checkBuf, lead...)
			checkBuf = append(checkBuf, chunk...)

			// Check if this speech start is likely just an echo.
			isEcho := false
			if ms.echoSuppressor != nil && ms.echoSuppressor.IsEchoFast(checkBuf) {
				isEcho = true
			}

			// Barge-in if speaking, thinking, or recently finished speaking (trail).
			canBargeIn := (speaking || isThinking || time.Since(lastEmitted) < 1500*time.Millisecond)

			// If it's a valid barge-in (not echo), kill current turn and start STT
			if canBargeIn && !isEcho {
				ms.mu.Lock()
				ms.userInterrupting = true
				ms.sttGeneration++
				pipelineCancel := ms.pipelineCancel
				ms.pipelineCancel = nil
				ms.sttChan = nil
				ms.mu.Unlock()

				if pipelineCancel != nil {
					pipelineCancel()
				}

				ms.emit(UserSpeaking, nil)
				ms.internalInterrupt()

				if sProvider, ok := ms.orch.stt.(StreamingSTTProvider); ok {
					ms.startStreamingSTT(sProvider)
				}
				break
			}

			// Normal turn start (if not already barge-in)
			if !isEcho {
				ms.emit(UserSpeaking, nil)
				ms.mu.Lock()
				ms.sttStartTime = time.Now()
				ms.sttEndTime = time.Time{}
				ms.llmStartTime = time.Time{}
				ms.llmEndTime = time.Time{}
				ms.ttsStartTime = time.Time{}
				ms.ttsFirstChunkTime = time.Time{}
				ms.ttsEndTime = time.Time{}
				ms.lastUserAudio = nil
				ms.mu.Unlock()

				ms.internalInterrupt()

				if sProvider, ok := ms.orch.stt.(StreamingSTTProvider); ok {
					ms.startStreamingSTT(sProvider)
				}
			}

		case VADSpeechEnd:
			ms.mu.Lock()
			ms.userSpeechEndTime = time.Now()
			ms.mu.Unlock()
			ms.emit(UserStopped, nil)

			ms.mu.Lock()
			sttChan := ms.sttChan
			if sttChan != nil {
				ms.sttChan = nil
				ms.mu.Unlock()
			} else {
				audioData := make([]byte, ms.audioBuf.Len())
				copy(audioData, ms.audioBuf.Bytes())
				ms.audioBuf.Reset()
				ms.mu.Unlock()

				go func(buf []byte) {
					t := time.NewTimer(speechEndHold)
					defer t.Stop()

					select {
					case <-t.C:
						if rmsVAD, ok := ms.vad.(*RMSVAD); ok {
							if rmsVAD.IsSpeaking() {
								ms.mu.Lock()
								ms.audioBuf.Write(buf)
								ms.mu.Unlock()
								return
							}
						}
						ms.runBatchPipeline(buf)
					case <-ms.ctx.Done():
						return
					}
				}(audioData)
			}

		case VADSilence:
			// no-op
		}
	}

	isUserSpeaking := false
	if rmsVAD, ok := ms.vad.(*RMSVAD); ok {
		isUserSpeaking = rmsVAD.IsSpeaking()
	}

	ms.mu.Lock()
	ms.audioBuf.Write(chunk)
	if !isUserSpeaking && ms.audioBuf.Len() > 176400 {
		data := ms.audioBuf.Bytes()
		leadIn := data[len(data)-132300:]
		ms.audioBuf.Reset()
		ms.audioBuf.Write(leadIn)
	}
	ms.mu.Unlock()

	ms.mu.Lock()
	sttChan := ms.sttChan
	if sttChan != nil {
		ms.lastUserAudio = append(ms.lastUserAudio, chunk...)
	}
	ms.mu.Unlock()

	if sttChan != nil {
		select {
		case sttChan <- chunk:
		default:
		}
	}

	return nil
}

// isLikelyNoise uses a density-based heuristic to filter out STT hallucinations.
// It compares the duration of the audio segment to the number of words produced.
// High duration with very low word count is a classic sign of STT artifacts
// triggered by background noise (fans, hums, etc).
func isLikelyNoise(transcript string, audioDuration time.Duration) bool {
	t := strings.TrimSpace(transcript)
	if t == "" {
		return true
	}

	// Filter out very short transient noises (clicks, coughs)
	// that are too short to be meaningful speech (< 100ms).
	if audioDuration < 100*time.Millisecond {
		return true
	}

	wordCount := len(strings.Fields(t))

	// Density check: If we have a long audio segment but very few words,
	// it's likely the STT 'hallucinating' on background noise.
	// 1 word from > 1.2 seconds of audio is often noise (Whisper artifact).
	if wordCount == 1 && audioDuration > 1200*time.Millisecond {
		return true
	}

	// 2 words from > 3 seconds of audio is also highly suspicious.
	if wordCount <= 2 && audioDuration > 3000*time.Millisecond {
		return true
	}

	return false
}

func (ms *ManagedStream) startStreamingSTT(provider StreamingSTTProvider) {

	ctx, cancel := context.WithCancel(ms.ctx)

	// Capture current generation to detect stale callbacks from previous sessions
	ms.mu.Lock()
	currentGeneration := ms.sttGeneration
	ms.mu.Unlock()

	sttChan, err := provider.StreamTranscribe(ctx, ms.session.GetCurrentLanguage(), func(transcript string, isFinal bool) error {
		ms.mu.Lock()
		speaking := ms.isSpeaking
		thinking := ms.isThinking
		// Ignore callbacks from stale STT sessions (happens when interrupted)
		isStale := ms.sttGeneration != currentGeneration
		ms.mu.Unlock()

		// Ignore this callback if we've already moved to a new STT session
		if isStale {
			return nil
		}

		// When bot is actively speaking, apply word threshold to prevent short utterances
		// from interrupting. When bot is thinking/generating response, interrupt immediately
		// on any detected speech.
		if speaking {
			minWords := 1
			if ms.orch != nil {
				minWords = ms.orch.GetConfig().MinWordsToInterrupt
			}

			if minWords > 1 {
				wc := countWords(transcript)
				if wc < minWords {
					// keep partial transcripts visible, but suppress final user turn
					if !isFinal {
						ms.emit(TranscriptPartial, transcript)
					}
					return nil
				}
				// reached threshold -> interrupt assistant
				ms.mu.Lock()
				duration := time.Since(ms.sttStartTime)
				ms.mu.Unlock()
				if !isLikelyNoise(transcript, duration) {
					ms.internalInterrupt()
				}
			} else {
				// minWords == 1 while assistant is speaking -> any transcript
				// (including partial) should trigger an interrupt (barge-in).
				ms.mu.Lock()
				duration := time.Since(ms.sttStartTime)
				ms.mu.Unlock()
				if strings.TrimSpace(transcript) != "" && !isLikelyNoise(transcript, duration) {
					ms.internalInterrupt()
				}
			}
		} else if thinking && strings.TrimSpace(transcript) != "" {
			ms.mu.Lock()
			duration := time.Since(ms.sttStartTime)
			ms.mu.Unlock()
			if !isLikelyNoise(transcript, duration) {
				// Bot is thinking (generating response) - interrupt immediately on any speech
				ms.internalInterrupt()
			}
		}

		if isFinal {
			// record STT final timestamp for instrumentation
			ms.mu.Lock()
			ms.sttEndTime = time.Now()
			duration := time.Since(ms.sttStartTime)
			ms.mu.Unlock()

			// Don't respond to hallucinations as final user turn
			if isLikelyNoise(transcript, duration) {
				return nil
			}

			ms.emit(TranscriptFinal, transcript)
			ms.session.AddMessage("user", transcript)
		} else {
			ms.emit(TranscriptPartial, transcript)
		}
		return nil
	})

	if err != nil {
		ms.emit(ErrorEvent, fmt.Sprintf("failed to start streaming STT: %v", err))
		cancel()
		return
	}

	ms.pipelineCtx = ctx
	ms.pipelineCancel = cancel
	ms.sttChan = sttChan
	// mark streaming STT start time for instrumentation
	ms.sttStartTime = time.Now()

	if ms.audioBuf.Len() > 0 {
		data := make([]byte, ms.audioBuf.Len())
		copy(data, ms.audioBuf.Bytes())
		// Save copy as lastUserAudio for CLI export/debug
		ms.lastUserAudio = make([]byte, len(data))
		copy(ms.lastUserAudio, data)
		// Clear the buffer after copying - fresh audio will accumulate from now on
		ms.audioBuf.Reset()
		select {
		case sttChan <- data:
		default:
		}
	}
}

func (ms *ManagedStream) runBatchPipeline(audioData []byte) {
	// Interrupt pending operations FIRST (outside lock for now)
	ms.internalInterrupt()

	ms.mu.Lock()
	ctx, cancel := context.WithCancel(ms.ctx)
	ms.pipelineCtx = ctx
	ms.pipelineCancel = cancel
	// instrumentation: mark STT start for batch pipeline
	ms.sttStartTime = time.Now()
	// capture the audio used for this STT call
	ms.lastUserAudio = make([]byte, len(audioData))
	copy(ms.lastUserAudio, audioData)
	ms.mu.Unlock()
	defer cancel()

	ms.emit(BotThinking, nil)

	transcript, err := ms.orch.Transcribe(ctx, audioData, ms.session.GetCurrentLanguage())
	// instrumentation: mark STT end immediately after Transcribe returns
	ms.mu.Lock()
	if err == nil {
		ms.sttEndTime = time.Now()
	}
	ms.mu.Unlock()

	if err != nil {
		if ctx.Err() == nil {
			ms.emit(ErrorEvent, fmt.Sprintf("transcription error: %v", err))
		}
		return
	}

	// Calculate duration for batch pipeline (44.1kHz, 16-bit mono)
	audioDuration := time.Duration(len(audioData)/2) * time.Second / 44100

	if transcript == "" || isLikelyNoise(transcript, audioDuration) {
		return
	}

	// When assistant is currently speaking and a minimum-word interrupt
	// threshold is configured, suppress short user utterances (backchannels)
	// and only interrupt when the transcript meets the threshold.
	ms.mu.Lock()
	speaking := ms.isSpeaking
	ms.mu.Unlock()
	if speaking && ms.orch != nil && ms.orch.GetConfig().MinWordsToInterrupt > 1 {
		if countWords(transcript) < ms.orch.GetConfig().MinWordsToInterrupt {
			// discard short user utterance
			return
		}
		// otherwise interrupt the assistant before processing
		ms.internalInterrupt()
	}

	ms.emit(TranscriptFinal, transcript)
	ms.session.AddMessage("user", transcript)

	ms.runLLMAndTTS(ctx, transcript)
}

func (ms *ManagedStream) runLLMAndTTS(ctx context.Context, transcript string) {
	ms.mu.Lock()

	if ms.responseCancel != nil {
		ms.responseCancel()
	}
	if ms.ttsCancel != nil {
		ms.ttsCancel()
	}

	rCtx, rCancel := context.WithCancel(ctx)
	ms.responseCancel = rCancel
	ms.isThinking = true
	ms.mu.Unlock()

	defer rCancel()

	ms.emit(BotThinking, nil)

	// instrumentation: mark LLM start
	ms.mu.Lock()
	ms.llmStartTime = time.Now()
	ms.mu.Unlock()

	response, err := ms.orch.GenerateResponse(rCtx, ms.session)
	// instrumentation: mark LLM end
	ms.mu.Lock()
	if err == nil {
		ms.llmEndTime = time.Now()
	}
	ms.mu.Unlock()

	if err != nil {
		if rCtx.Err() == nil {
			ms.emit(ErrorEvent, fmt.Sprintf("LLM error: %v", err))
		}
		return
	}

	ms.session.AddMessage("assistant", response)
	// Emit the assistant text so callers (CLI, tests) can display the
	// agent's textual response prior to/while TTS is synthesized.
	ms.emit(BotResponse, response)

	ms.mu.Lock()
	ms.isThinking = false
	ms.isSpeaking = true

	if ms.vad != nil {
		ms.vad.Reset()
	}

	// Create separate TTS context for fast abort on barge-in
	ttsCtx, ttsCancel := context.WithCancel(rCtx)
	ms.ttsCancel = ttsCancel
	ms.mu.Unlock()

	defer ttsCancel()

	ms.mu.Lock()
	ms.botSpeakStartTime = time.Now()
	// instrumentation: mark TTS synthesis start
	ms.ttsStartTime = ms.botSpeakStartTime
	ms.mu.Unlock()
	ms.emit(BotSpeaking, nil)

	err = ms.orch.SynthesizeStream(ttsCtx, response, ms.session.GetCurrentVoice(), ms.session.GetCurrentLanguage(), func(chunk []byte) error {
		select {
		case <-ttsCtx.Done():
			return ttsCtx.Err()
		default:
			ms.mu.Lock()
			ms.lastAudioSentAt = time.Now()
			ms.lastAudioEmittedAt = ms.lastAudioSentAt
			// record first-chunk timestamp for instrumentation
			if ms.ttsFirstChunkTime.IsZero() {
				ms.ttsFirstChunkTime = time.Now()
			}
			ms.mu.Unlock()

			// Record this audio chunk for echo detection
			ms.echoSuppressor.RecordPlayedAudio(chunk)

			ms.emit(AudioChunk, chunk)
			return nil
		}
	})

	// instrumentation: mark TTS end
	ms.mu.Lock()
	if !ms.ttsStartTime.IsZero() {
		ms.ttsEndTime = time.Now()
	}
	ms.mu.Unlock()

	if err != nil && ttsCtx.Err() == nil {
		ms.emit(ErrorEvent, fmt.Sprintf("TTS error: %v", err))
	}

	ms.mu.Lock()
	ms.isSpeaking = false
	ms.ttsCancel = nil
	ms.mu.Unlock()
}

func (ms *ManagedStream) NotifyAudioPlayed() {
	ms.mu.Lock()
	ms.lastAudioSentAt = time.Now()
	ms.lastAudioEmittedAt = ms.lastAudioSentAt
	ms.mu.Unlock()
}

// RecordPlayedOutput should be called by the audio playback thread with the
// actual samples being sent to the speaker. This ensures the echo suppressor's
// reference buffer matches what the microphone may pick up.
func (ms *ManagedStream) RecordPlayedOutput(chunk []byte) {
	if ms.echoSuppressor == nil || len(chunk) == 0 {
		return
	}
	ms.echoSuppressor.RecordPlayedAudio(chunk)
}

// GetLatency returns the time in milliseconds from when user stopped speaking
// to when bot started playing audio (0 if not applicable)
func (ms *ManagedStream) GetLatency() int64 {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	if ms.userSpeechEndTime.IsZero() || ms.botSpeakStartTime.IsZero() {
		return 0
	}

	if ms.botSpeakStartTime.Before(ms.userSpeechEndTime) {
		return 0
	}

	latency := ms.botSpeakStartTime.Sub(ms.userSpeechEndTime)
	return latency.Milliseconds()
}

// LatencyBreakdown holds per-stage timings (all values in milliseconds).
type LatencyBreakdown struct {
	UserToSTT          int64 // user stop -> STT final
	STT                int64 // STT duration (start→end)
	UserToLLM          int64 // user stop -> LLM end
	LLM                int64 // LLM duration (start→end)
	UserToTTSFirstByte int64 // user stop -> first TTS chunk
	LLMToTTSFirstByte  int64 // LLM end -> first TTS chunk
	TTSTotal           int64 // TTS total duration (ttsStart→ttsEnd)
	BotStartLatency    int64 // user stop -> botSpeakStart
	UserToPlay         int64 // user stop -> actual audio played (lastAudioSentAt)
}

// GetEndToEndLatency returns the time in milliseconds from when the user
// stopped speaking to when the first audio sample was actually played by the
// audio device (0 if not available).
func (ms *ManagedStream) GetEndToEndLatency() int64 {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	if ms.userSpeechEndTime.IsZero() || ms.lastAudioSentAt.IsZero() {
		return 0
	}

	if ms.lastAudioSentAt.Before(ms.userSpeechEndTime) {
		return 0
	}

	latency := ms.lastAudioSentAt.Sub(ms.userSpeechEndTime)
	return latency.Milliseconds()
}

// GetLatencyBreakdown returns measured timings for STT, LLM and TTS stages.
func (ms *ManagedStream) GetLatencyBreakdown() LatencyBreakdown {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	var bd LatencyBreakdown
	if ms.userSpeechEndTime.IsZero() {
		return bd
	}

	// STT
	if !ms.sttEndTime.IsZero() {
		bd.UserToSTT = ms.sttEndTime.Sub(ms.userSpeechEndTime).Milliseconds()
	}
	if !ms.sttStartTime.IsZero() && !ms.sttEndTime.IsZero() {
		bd.STT = ms.sttEndTime.Sub(ms.sttStartTime).Milliseconds()
	}

	// LLM
	if !ms.llmEndTime.IsZero() {
		bd.UserToLLM = ms.llmEndTime.Sub(ms.userSpeechEndTime).Milliseconds()
	}
	if !ms.llmStartTime.IsZero() && !ms.llmEndTime.IsZero() {
		bd.LLM = ms.llmEndTime.Sub(ms.llmStartTime).Milliseconds()
	}

	// TTS first byte
	if !ms.ttsFirstChunkTime.IsZero() {
		bd.UserToTTSFirstByte = ms.ttsFirstChunkTime.Sub(ms.userSpeechEndTime).Milliseconds()
	}
	if !ms.llmEndTime.IsZero() && !ms.ttsFirstChunkTime.IsZero() {
		bd.LLMToTTSFirstByte = ms.ttsFirstChunkTime.Sub(ms.llmEndTime).Milliseconds()
	}

	// TTS total
	if !ms.ttsStartTime.IsZero() && !ms.ttsEndTime.IsZero() {
		bd.TTSTotal = ms.ttsEndTime.Sub(ms.ttsStartTime).Milliseconds()
	}

	// Bot start and playback
	if !ms.botSpeakStartTime.IsZero() {
		bd.BotStartLatency = ms.botSpeakStartTime.Sub(ms.userSpeechEndTime).Milliseconds()
	}
	if !ms.lastAudioSentAt.IsZero() {
		bd.UserToPlay = ms.lastAudioSentAt.Sub(ms.userSpeechEndTime).Milliseconds()
	}

	return bd
}

// ExportLastUserAudio returns a copy of the last captured user-turn audio (raw)
// and a post-processed version (echo-suppressed) suitable for debugging.
// Both slices are raw 16-bit little-endian PCM. Caller may be nil-checked.
func (ms *ManagedStream) ExportLastUserAudio() (raw []byte, processed []byte) {
	ms.mu.Lock()
	if len(ms.lastUserAudio) == 0 {
		ms.mu.Unlock()
		return nil, nil
	}
	rawCopy := make([]byte, len(ms.lastUserAudio))
	copy(rawCopy, ms.lastUserAudio)
	ms.mu.Unlock()

	if ms.echoSuppressor != nil {
		processed = ms.echoSuppressor.PostProcess(rawCopy)
	} else {
		processed = rawCopy
	}
	return rawCopy, processed
}

func (ms *ManagedStream) Events() <-chan OrchestratorEvent {
	return ms.events
}

func (ms *ManagedStream) Close() {
	// idempotent close to avoid panic if Close is called multiple times
	ms.closeOnce.Do(func() {
		// First interrupt to stop all active operations
		ms.interrupt()

		// Clean up resources under lock
		ms.mu.Lock()
		ms.audioBuf.Reset()
		ms.mu.Unlock()

		// Clear echo buffer
		ms.echoSuppressor.ClearEchoBuffer()

		// Then cancel the context to signal all goroutines to exit
		ms.cancel()

		// Give goroutines a moment to exit cleanly
		time.Sleep(10 * time.Millisecond)

		// Finally close the events channel
		close(ms.events)
	})
}

func (ms *ManagedStream) emit(eventType EventType, data interface{}) {
	// Silently drop events if context is cancelled (shutdown in progress)
	select {
	case <-ms.ctx.Done():
		return
	default:
	}

	if eventType == AudioChunk {
		ms.mu.Lock()
		speaking := ms.isSpeaking
		userInterrupting := ms.userInterrupting
		ms.mu.Unlock()
		// Don't emit audio chunks if not speaking OR if user is interrupting (barge-in)
		if !speaking || userInterrupting {
			return
		}
	}

	event := OrchestratorEvent{
		Type:      eventType,
		SessionID: ms.session.ID,
		Data:      data,
	}

	// Use non-blocking send with panic recovery in case channel is closed
	defer func() {
		if r := recover(); r != nil {
			// Channel closed, stream shutting down - safe to ignore
		}
	}()

	select {
	case ms.events <- event:
	case <-ms.ctx.Done():
		// Context cancelled, give up
	default:
		// Channel full, drop event non-blocking
	}
}

func (ms *ManagedStream) interrupt() {
	ms.internalInterrupt()
}

func (ms *ManagedStream) internalInterrupt() {
	// Acquire lock FIRST before reading any protected fields
	// (fixes race condition that caused deadlocks)
	ms.mu.Lock()

	// Check if there's anything to interrupt.
	// We also allow interruption if we *recently* finished speaking (1500ms trail)
	// to ensure the user can clear any local buffers at the end of a sentence.
	recentActivity := time.Since(ms.lastAudioEmittedAt) < 1500*time.Millisecond
	if ms.pipelineCancel == nil && ms.responseCancel == nil && ms.ttsCancel == nil && !ms.isSpeaking && !ms.isThinking && !ms.userInterrupting && !recentActivity {
		ms.mu.Unlock()
		return
	}

	// Retrieve all cancellable contexts under lock - NEVER close channels, let context cancellation handle it
	pipelineCancel := ms.pipelineCancel
	responseCancel := ms.responseCancel
	ttsCancel := ms.ttsCancel

	ms.pipelineCancel = nil
	ms.responseCancel = nil
	ms.ttsCancel = nil
	ms.sttChan = nil
	ms.sttGeneration++ // Invalidate all concurrent STT callbacks

	// NOTE: Don't clear audio buffer here - it contains important audio that might include user speech!
	// The buffer is managed by the Write() function and cleared when we're truly done (Close or other cleanup)

	ms.isSpeaking = false
	ms.isThinking = false
	ms.userInterrupting = false
	ms.mu.Unlock()

	// Clear echo buffer when interrupting - we want to detect new user speech
	ms.echoSuppressor.ClearEchoBuffer()

	// Cancel all contexts OUTSIDE the lock to prevent deadlocks
	// Context cancellation will cause the STT/TTS goroutines to exit cleanly
	if pipelineCancel != nil {
		pipelineCancel()
	}
	if responseCancel != nil {
		responseCancel()
	}
	if ttsCancel != nil {
		ttsCancel()
	}

	// Try to forcibly abort provider-level synthesis
	if ms.orch != nil && ms.orch.tts != nil {
		if err := ms.orch.tts.Abort(); err != nil {
			ms.orch.logger.Warn("tts abort failed", "sessionID", ms.session.ID, "error", err)
		}
	}

	ms.lastInterruptedAt = time.Now()
	// Emit the Interrupted event as soon as possible so the client/server
	// can clear its playback buffer.
	ms.emit(Interrupted, nil)
	// Then drain any remaining audio chunks in the channel to prevent stale audio
	// from being processed.
	ms.drainAudioChunks()
}

func (ms *ManagedStream) drainAudioChunks() {
	// Non-blocking drain: remove audio chunks, keep control events
	// Use timeout to avoid blocking if channel reader is slow
	deadline := time.Now().Add(100 * time.Millisecond)
	var controlEvents []OrchestratorEvent

	for {
		select {
		case ev := <-ms.events:
			if ev.Type != AudioChunk {
				controlEvents = append(controlEvents, ev)
			}
		default:
			// No more events to drain
			goto DrainDone
		}

		// Safety timeout to prevent infinite blocking
		if time.Now().After(deadline) {
			goto DrainDone
		}
	}

DrainDone:
	// Re-emit control events (don't hold lock, events channel might be full)
	for _, ev := range controlEvents {
		select {
		case ms.events <- ev:
		default:
			// Channel full, drop event
		}
	}
}
