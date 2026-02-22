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

	responseCancel   context.CancelFunc
	ttsCancel        context.CancelFunc // Track TTS context for fast abort
	userInterrupting bool               // Flag to block audio emission during user barge-in
	echoSuppressor   *EchoSuppressor    // Echo detection and suppression
	closeOnce        sync.Once
}

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

const speechEndHold = 300 * time.Millisecond

func (ms *ManagedStream) Write(chunk []byte) error {
	// Avoid holding ms.mu for the entire function — callers (and
	// startStreamingSTT) also need to acquire ms.mu and that caused a
	// re-entrancy deadlock in practice.

	if ms.vad == nil {
		return fmt.Errorf("VAD not configured for this stream")
	}

	// Temporarily adjust VAD threshold when recent audio was played. This
	// prevents immediate echo from freshly-played audio from being mistaken
	// for user speech — but it MUST NOT prevent legitimate user barge-in.
	// Only apply the aggressive "echo guard" when we are *not* currently
	// speaking (i.e. playback leftover), so active TTS remains interruptible.
	if rmsVAD, ok := ms.vad.(*RMSVAD); ok {
		originalThreshold := rmsVAD.Threshold()
		originalMinConfirmed := rmsVAD.MinConfirmed()

		ms.mu.Lock()
		speaking := ms.isSpeaking
		lastSent := ms.lastAudioSentAt
		ms.mu.Unlock()

		if speaking {
			// Require more sustained sound to interrupt the bot (e.g., 3 frames ~ 70ms)
			// to avoid transient noises or small echo slips causing false interruptions,
			// but keeping it low enough so the user can still barge in easily.
			if originalMinConfirmed < 3 {
				rmsVAD.SetMinConfirmed(3)
			}
		} else if time.Since(lastSent) < 250*time.Millisecond {
			// Only apply aggressive "echo guard" when we recently finished speaking
			rmsVAD.SetAdaptiveMode(false)
			rmsVAD.SetThreshold(0.25)
		}

		defer func() {
			rmsVAD.SetThreshold(originalThreshold)
			rmsVAD.SetMinConfirmed(originalMinConfirmed)
			rmsVAD.SetAdaptiveMode(true)
		}()
	}

	// apply realtime echo removal to the incoming mic chunk BEFORE VAD/STT
	isLikelyEchoByEnergy := false
	if ms.echoSuppressor != nil {
		// keep original energy for a relative check
		origSamples := bytesToSamples(chunk)
		origEnergy := calculateEnergy(origSamples)

		cleaned := ms.echoSuppressor.RemoveEchoRealtime(chunk)

		cleanedEnergy := calculateEnergy(bytesToSamples(cleaned))
		// if cleaned energy is both very small OR a small fraction of original,
		// it's almost certainly echo and we should treat it as such.
		if cleanedEnergy < 1e-8 || (origEnergy > 0 && cleanedEnergy/origEnergy < 0.02) {
			isLikelyEchoByEnergy = true
			// use cleaned (near-zero) so VAD sees silence
			chunk = cleaned
		} else {
			// otherwise pass the cleaned audio through
			chunk = cleaned
		}
	}
	event, err := ms.vad.Process(chunk)
	if err != nil {
		return err
	}

	if event != nil && event.Type != VADSilence {
		switch event.Type {
		case VADSpeechStart:
			// Check if this is echo from speakers before treating as speech
			// Build a short buffer combining recent captured mic (lead-in) + current chunk
			ms.mu.Lock()
			lead := ms.audioBuf.Bytes()
			ms.mu.Unlock()

			// keep only last ~100ms of lead audio to improve match stability
			leadBytes := 8820 // ~100ms @44.1kHz * 2 bytes
			if len(lead) > leadBytes {
				lead = lead[len(lead)-leadBytes:]
			}
			checkBuf := make([]byte, 0, len(lead)+len(chunk))
			checkBuf = append(checkBuf, lead...)
			checkBuf = append(checkBuf, chunk...)

			if ms.echoSuppressor.IsEcho(checkBuf) {
				// This audio is primarily echo from our speaker output - ignore it
				break
			}

			// If we're currently playing TTS and the mic input arrives
			// immediately after an audio chunk, it's likely our own
			// playback being captured — ignore short-lived echoes to avoid
			// self-interruption.
			ms.mu.Lock()
			speaking := ms.isSpeaking
			lastSent := ms.lastAudioSentAt
			ms.mu.Unlock()

			if speaking && time.Since(lastSent) < 120*time.Millisecond {
				// treat as silence/ignore this VAD event
				break
			}

			// If assistant is currently speaking, treat this as an IMMEDIATE user barge-in:
			// 1. Set userInterrupting flag to block new audio chunks
			// 2. Cancel streaming STT context to stop processing
			// 3. Keep audio buffer - we need it for the new STT session!
			// 4. Cancel all pending responses
			// 5. Restart streaming STT for fresh user input
			if speaking {
				ms.mu.Lock()
				ms.userInterrupting = true
				ms.sttGeneration++ // Invalidate old STT callbacks
				// Cancel pipeline context to stop any in-flight STT (don't close the channel)
				pipelineCancel := ms.pipelineCancel
				ms.pipelineCancel = nil
				ms.sttChan = nil
				// NOTE: Don't clear audio buffer here - we need it for the new STT!
				ms.mu.Unlock()

				// Cancel context outside the lock to avoid deadlocks
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

			// not speaking: normal user turn — emit and interrupt pending response
			ms.emit(UserSpeaking, nil)
			// reset per-turn instrumentation timestamps
			ms.mu.Lock()
			ms.sttStartTime = time.Time{}
			ms.sttEndTime = time.Time{}
			ms.llmStartTime = time.Time{}
			ms.llmEndTime = time.Time{}
			ms.ttsStartTime = time.Time{}
			ms.ttsFirstChunkTime = time.Time{}
			ms.ttsEndTime = time.Time{}
			ms.lastUserAudio = nil
			ms.mu.Unlock()

			ms.internalInterrupt()

			// start streaming STT without holding ms.mu to avoid deadlock
			if sProvider, ok := ms.orch.stt.(StreamingSTTProvider); ok {
				ms.startStreamingSTT(sProvider)
			}

		case VADSpeechEnd:
			ms.mu.Lock()
			ms.userSpeechEndTime = time.Now()
			ms.mu.Unlock()
			ms.emit(UserStopped, nil)

			// Capture current audio buffer under lock and schedule a short
			// hold before finalizing the user's turn. If speech resumes during
			// the hold, re-insert the captured audio back into the buffer and
			// don't transcribe yet. This prevents premature truncation of
			// user utterances caused by brief pauses.
			ms.mu.Lock()
			sttChan := ms.sttChan
			if sttChan != nil {
				ms.sttChan = nil // Stop sending new audio to STT provider
				ms.mu.Unlock()
				// DO NOT cancel the context - let STT provider finish processing audio it has
				// The context will be cancelled later when speech resumes or timeout occurs
			} else {
				audioData := make([]byte, ms.audioBuf.Len())
				copy(audioData, ms.audioBuf.Bytes())
				ms.audioBuf.Reset()
				ms.mu.Unlock()

				go func(buf []byte) {
					// short grace period to allow resumption of speech
					t := time.NewTimer(speechEndHold)
					defer t.Stop()

					select {
					case <-t.C:
						// if VAD now reports speaking, reinsert buffer and abort
						if rmsVAD, ok := ms.vad.(*RMSVAD); ok {
							if rmsVAD.IsSpeaking() {
								ms.mu.Lock()
								ms.audioBuf.Write(buf)
								ms.mu.Unlock()
								return
							}
						}
						// otherwise proceed with batch transcription
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

	// forward chunk to streaming STT if present (read sttChan under lock,
	// perform non-blocking send outside the lock)
	// First, check whether this chunk appears to be echo of our own playback.
	isEcho := false
	if ms.echoSuppressor != nil {
		// build a small context buffer (tail of audioBuf + current chunk) to
		// improve correlation stability
		ms.mu.Lock()
		lead := ms.audioBuf.Bytes()
		ms.mu.Unlock()

		leadBytes := 8820 // ~100ms @44.1kHz * 2 bytes
		if len(lead) > leadBytes {
			lead = lead[len(lead)-leadBytes:]
		}
		check := make([]byte, 0, len(lead)+len(chunk))
		check = append(check, lead...)
		check = append(check, chunk...)
		if ms.echoSuppressor.IsEcho(check) {
			isEcho = true
		}
	}

	// also respect the earlier energy-based decision made during realtime removal
	if isLikelyEchoByEnergy {
		isEcho = true
	}
	ms.mu.Lock()
	sttChan := ms.sttChan
	// Only accumulate user audio and forward to STT when this chunk is NOT echo
	if sttChan != nil && !isEcho {
		ms.lastUserAudio = append(ms.lastUserAudio, chunk...)
	}
	ms.mu.Unlock()

	if sttChan != nil && !isEcho {
		select {
		case sttChan <- chunk:
		default:
		}
	}

	// append to audio buffer under lock
	isUserSpeaking := false
	if rmsVAD, ok := ms.vad.(*RMSVAD); ok {
		isUserSpeaking = rmsVAD.IsSpeaking()
	}

	ms.mu.Lock()
	// If this chunk was detected as echo earlier, don't add it to the rolling
	// buffer that we later feed into STT — prevents self-transcription.
	if !isEcho {
		ms.audioBuf.Write(chunk)
		// Keep a rolling buffer of ~2 seconds of audio pre-speech detection
		// At 44100 Hz, 16-bit mono: 2 seconds = 44100 * 2 * 2 bytes = 176,400 bytes
		// This ensures we capture the full beginning of user speech for accurate transcription
		if !isUserSpeaking && ms.audioBuf.Len() > 176400 {
			data := ms.audioBuf.Bytes()
			// Keep only the last 1.5 seconds (132,300 bytes)
			leadIn := data[len(data)-132300:]
			ms.audioBuf.Reset()
			ms.audioBuf.Write(leadIn)
		}
	}
	ms.mu.Unlock()

	return nil
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
				ms.internalInterrupt()
			} else {
				// minWords == 1 while assistant is speaking -> any transcript
				// (including partial) should trigger an interrupt (barge-in).
				if strings.TrimSpace(transcript) != "" {
					ms.internalInterrupt()
				}
			}
		} else if thinking && strings.TrimSpace(transcript) != "" {
			// Bot is thinking (generating response) - interrupt immediately on any speech
			ms.internalInterrupt()
		}

		if isFinal {
			// record STT final timestamp for instrumentation
			ms.mu.Lock()
			ms.sttEndTime = time.Now()
			ms.mu.Unlock()

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

	if transcript == "" {
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

	// Check if there's anything to interrupt
	if ms.pipelineCancel == nil && ms.responseCancel == nil && ms.ttsCancel == nil && !ms.isSpeaking && !ms.isThinking && !ms.userInterrupting {
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
	ms.drainAudioChunks()
	ms.emit(Interrupted, nil)
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
