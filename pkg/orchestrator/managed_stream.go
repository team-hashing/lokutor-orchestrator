package orchestrator

import (
	"bytes"
	"context"
	"fmt"
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
	isSpeaking        bool
	isThinking        bool
	lastInterruptedAt time.Time
	lastAudioSentAt   time.Time

	
	responseCancel context.CancelFunc
}


func NewManagedStream(ctx context.Context, o *Orchestrator, session *ConversationSession) *ManagedStream {
	mCtx, mCancel := context.WithCancel(ctx)

	var streamVAD VADProvider
	if o.vad != nil {
		streamVAD = o.vad.Clone()
	}

	ms := &ManagedStream{
		orch:     o,
		session:  session,
		ctx:      mCtx,
		cancel:   mCancel,
		events:   make(chan OrchestratorEvent, 1024),
		audioBuf: new(bytes.Buffer),
		vad:      streamVAD,
	}

	return ms
}


func (ms *ManagedStream) Write(chunk []byte) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	if ms.vad == nil {
		return fmt.Errorf("VAD not configured for this stream")
	}

	
	if rmsVAD, ok := ms.vad.(*RMSVAD); ok {
		originalThreshold := rmsVAD.Threshold()
		if time.Since(ms.lastAudioSentAt) < 250*time.Millisecond {
			
			
			rmsVAD.SetAdaptiveMode(false)
			rmsVAD.SetThreshold(0.25) 
			defer func() {
				rmsVAD.SetThreshold(originalThreshold)
				rmsVAD.SetAdaptiveMode(true)
			}()
		}
	}

	
	event, err := ms.vad.Process(chunk)
	if err != nil {
		return err
	}

	if event != nil && event.Type != VADSilence {
		switch event.Type {
		case VADSpeechStart:
			ms.emit(UserSpeaking, nil)
			
			ms.internalInterrupt()

			
			if sProvider, ok := ms.orch.stt.(StreamingSTTProvider); ok {
				ms.startStreamingSTT(sProvider)
			}

		case VADSpeechEnd:
			ms.emit(UserStopped, nil)

			if ms.sttChan != nil {
				close(ms.sttChan)
				ms.sttChan = nil
			} else {
				
				audioData := make([]byte, ms.audioBuf.Len())
				copy(audioData, ms.audioBuf.Bytes())
				ms.audioBuf.Reset()
				go ms.runBatchPipeline(audioData)
			}

		case VADSilence:
			
		}
	}

	
	if ms.sttChan != nil {
		select {
		case ms.sttChan <- chunk:
		default:
			
		}
	}

	isUserSpeaking := false
	if rmsVAD, ok := ms.vad.(*RMSVAD); ok {
		isUserSpeaking = rmsVAD.IsSpeaking()
	}

	
	
	
	ms.audioBuf.Write(chunk)
	if !isUserSpeaking && ms.audioBuf.Len() > 50000 {
		
		data := ms.audioBuf.Bytes()
		leadIn := data[len(data)-44100:]
		ms.audioBuf.Reset()
		ms.audioBuf.Write(leadIn)
	}

	return nil
}

func (ms *ManagedStream) startStreamingSTT(provider StreamingSTTProvider) {
	
	ctx, cancel := context.WithCancel(ms.ctx)

	sttChan, err := provider.StreamTranscribe(ctx, ms.session.GetCurrentLanguage(), func(transcript string, isFinal bool) error {
		if isFinal {
			ms.emit(TranscriptFinal, transcript)
			ms.session.AddMessage("user", transcript)
			
			go ms.runLLMAndTTS(ctx, transcript)
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

	if ms.audioBuf.Len() > 0 {
		data := make([]byte, ms.audioBuf.Len())
		copy(data, ms.audioBuf.Bytes())
		select {
		case sttChan <- data:
		default:
		}
	}
}

func (ms *ManagedStream) runBatchPipeline(audioData []byte) {
	ms.mu.Lock()
	ms.internalInterrupt()
	ctx, cancel := context.WithCancel(ms.ctx)
	ms.pipelineCtx = ctx
	ms.pipelineCancel = cancel
	ms.mu.Unlock()
	defer cancel()

	ms.emit(BotThinking, nil)

	transcript, err := ms.orch.Transcribe(ctx, audioData, ms.session.GetCurrentLanguage())
	if err != nil {
		if ctx.Err() == nil {
			ms.emit(ErrorEvent, fmt.Sprintf("transcription error: %v", err))
		}
		return
	}

	if transcript == "" {
		return
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
	
	rCtx, rCancel := context.WithCancel(ctx)
	ms.responseCancel = rCancel
	ms.isThinking = true
	ms.mu.Unlock()

	defer rCancel()

	ms.emit(BotThinking, nil)

	response, err := ms.orch.GenerateResponse(rCtx, ms.session)
	if err != nil {
		if rCtx.Err() == nil {
			ms.emit(ErrorEvent, fmt.Sprintf("LLM error: %v", err))
		}
		return
	}

	ms.session.AddMessage("assistant", response)

	ms.mu.Lock()
	ms.isThinking = false
	ms.isSpeaking = true
	
	if ms.vad != nil {
		ms.vad.Reset()
	}
	ms.mu.Unlock()

	ms.emit(BotSpeaking, nil)

	err = ms.orch.SynthesizeStream(rCtx, response, ms.session.GetCurrentVoice(), ms.session.GetCurrentLanguage(), func(chunk []byte) error {
		select {
		case <-rCtx.Done():
			return rCtx.Err()
		default:
			ms.mu.Lock()
			ms.lastAudioSentAt = time.Now()
			ms.mu.Unlock()
			ms.emit(AudioChunk, chunk)
			return nil
		}
	})

	if err != nil && rCtx.Err() == nil {
		ms.emit(ErrorEvent, fmt.Sprintf("TTS error: %v", err))
	}

	ms.mu.Lock()
	ms.isSpeaking = false
	
	
	ms.mu.Unlock()
}




func (ms *ManagedStream) NotifyAudioPlayed() {
	ms.mu.Lock()
	ms.lastAudioSentAt = time.Now()
	ms.mu.Unlock()
}


func (ms *ManagedStream) Events() <-chan OrchestratorEvent {
	return ms.events
}


func (ms *ManagedStream) Close() {
	ms.cancel()
	ms.interrupt()
	close(ms.events)
}

func (ms *ManagedStream) emit(eventType EventType, data interface{}) {
	
	
	
	if eventType == AudioChunk {
		ms.mu.Lock()
		speaking := ms.isSpeaking
		ms.mu.Unlock()
		if !speaking {
			return
		}
	}

	event := OrchestratorEvent{
		Type:      eventType,
		SessionID: ms.session.ID,
		Data:      data,
	}

	
	
	isControl := eventType != AudioChunk

	if isControl {
		select {
		case ms.events <- event:
		case <-ms.ctx.Done():
		}
		return
	}

	
	
	select {
	case ms.events <- event:
	case <-ms.ctx.Done():
		
	}
}

func (ms *ManagedStream) interrupt() {
	ms.mu.Lock()
	ms.internalInterrupt()
	ms.mu.Unlock()
}


func (ms *ManagedStream) internalInterrupt() {
	
	if !ms.isSpeaking && !ms.isThinking {
		return
	}

	if ms.pipelineCancel != nil {
		ms.pipelineCancel()
		ms.pipelineCancel = nil
	}
	if ms.responseCancel != nil {
		ms.responseCancel()
		ms.responseCancel = nil
	}

	ms.lastInterruptedAt = time.Now()
	ms.isSpeaking = false
	ms.isThinking = false

	
	
	ms.drainAudioChunks()

	ms.emit(Interrupted, nil)

	if ms.sttChan != nil {
		ms.sttChan = nil
	}
}


func (ms *ManagedStream) drainAudioChunks() {
	var controlEvents []OrchestratorEvent
DrainLoop:
	for {
		select {
		case ev := <-ms.events:
			if ev.Type != AudioChunk {
				controlEvents = append(controlEvents, ev)
			}
		default:
			break DrainLoop
		}
	}
	
	for _, ev := range controlEvents {
		select {
		case ms.events <- ev:
		default:
			
			
		}
	}
}