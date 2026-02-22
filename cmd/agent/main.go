package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/gen2brain/malgo"
	"github.com/joho/godotenv"
	"github.com/lokutor-ai/lokutor-orchestrator/pkg/audio"
	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
	llmProvider "github.com/lokutor-ai/lokutor-orchestrator/pkg/providers/llm"
	sttProvider "github.com/lokutor-ai/lokutor-orchestrator/pkg/providers/stt"
	ttsProvider "github.com/lokutor-ai/lokutor-orchestrator/pkg/providers/tts"
)

const (
	SampleRate = 44100
	Channels   = 1
)

func main() {

	if err := godotenv.Load(); err != nil {
		log.Println("Note: No .env file found, using system environment variables")
	}

	groqKey := os.Getenv("GROQ_API_KEY")
	openaiKey := os.Getenv("OPENAI_API_KEY")
	anthropicKey := os.Getenv("ANTHROPIC_API_KEY")
	googleKey := os.Getenv("GOOGLE_API_KEY")
	deepgramKey := os.Getenv("DEEPGRAM_API_KEY")
	assemblyKey := os.Getenv("ASSEMBLYAI_API_KEY")
	lokutorKey := os.Getenv("LOKUTOR_API_KEY")

	sttProviderName := os.Getenv("STT_PROVIDER")
	if sttProviderName == "" {
		sttProviderName = "groq"
	}
	llmProviderName := os.Getenv("LLM_PROVIDER")
	if llmProviderName == "" {
		llmProviderName = "groq"
	}

	lang := orchestrator.Language(os.Getenv("AGENT_LANGUAGE"))
	if lang == "" {
		lang = orchestrator.LanguageEs
	}

	if lokutorKey == "" {
		log.Fatal("Error: LOKUTOR_API_KEY must be set.")
	}

	var stt orchestrator.STTProvider
	switch sttProviderName {
	case "openai":
		if openaiKey == "" {
			log.Fatal("Error: OPENAI_API_KEY must be set for openai STT")
		}
		stt = sttProvider.NewOpenAISTT(openaiKey, "whisper-1")
	case "deepgram":
		if deepgramKey == "" {
			log.Fatal("Error: DEEPGRAM_API_KEY must be set for deepgram STT")
		}
		stt = sttProvider.NewDeepgramSTT(deepgramKey)
	case "assemblyai":
		if assemblyKey == "" {
			log.Fatal("Error: ASSEMBLYAI_API_KEY must be set for assemblyai STT")
		}
		stt = sttProvider.NewAssemblyAISTT(assemblyKey)
	case "groq":
		fallthrough
	default:
		if groqKey == "" {
			log.Fatal("Error: GROQ_API_KEY must be set for groq STT")
		}
		groqModel := os.Getenv("GROQ_STT_MODEL")
		if groqModel == "" {
			groqModel = "whisper-large-v3"
		}
		stt = sttProvider.NewGroqSTT(groqKey, groqModel)
	}

	if s, ok := stt.(interface{ SetSampleRate(int) }); ok {
		s.SetSampleRate(SampleRate)
	}

	var llm orchestrator.LLMProvider
	switch llmProviderName {
	case "openai":
		if openaiKey == "" {
			log.Fatal("Error: OPENAI_API_KEY must be set for openai LLM")
		}
		llm = llmProvider.NewOpenAILLM(openaiKey, "gpt-4o")
	case "anthropic":
		if anthropicKey == "" {
			log.Fatal("Error: ANTHROPIC_API_KEY must be set for anthropic LLM")
		}
		llm = llmProvider.NewAnthropicLLM(anthropicKey, "claude-3-5-sonnet-20241022")
	case "google":
		if googleKey == "" {
			log.Fatal("Error: GOOGLE_API_KEY must be set for google LLM")
		}
		llm = llmProvider.NewGoogleLLM(googleKey, "gemini-1.5-flash")
	case "groq":
		fallthrough
	default:
		if groqKey == "" {
			log.Fatal("Error: GROQ_API_KEY must be set for groq LLM")
		}
		llm = llmProvider.NewGroqLLM(groqKey, "llama-3.3-70b-versatile")
	}

	fmt.Printf("Configured: STT=%s | LLM=%s | TTS=Lokutor\n", sttProviderName, llmProviderName)
	fmt.Printf("VAD Threshold: %.3f | Sample Rate: %dHz | Language: %s\n", 0.02, SampleRate, lang)
	fmt.Println("Voice Agent Started! Listening to microphone...")
	fmt.Println("Press Ctrl+C to exit")

	tts := ttsProvider.NewLokutorTTS(lokutorKey)

	// Create VAD with aggressive settings for fast interrupt detection
	// Lower threshold (0.015) catches speech earlier, shorter confirmation (1 frame ~23ms) for quick response
	vad := orchestrator.NewRMSVAD(0.015, 300*time.Millisecond)
	// Minimal confirmation frames for maximum responsiveness during interrupt
	vad.SetMinConfirmed(1)
	config := orchestrator.DefaultConfig()
	config.Language = lang
	orch := orchestrator.NewWithVAD(stt, llm, tts, vad, config)

	session := orch.NewSessionWithDefaults("user_123")

	systemPrompt := "You are a helpful and concise voice assistant. Use short sentences suitable for speech."
	if lang == orchestrator.LanguageEs {
		systemPrompt = "Eres un asistente de voz útil y conciso. Usa frases cortas adecuadas para el habla."
	}
	orch.SetSystemPrompt(session, systemPrompt)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream := orch.NewManagedStream(ctx, session)
	defer stream.Close()

	mctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer mctx.Uninit()

	var playbackMu sync.Mutex
	var playbackBytes []byte
	var e2eLogged bool

	// Mic capture decoupled from the audio callback thread!
	// This completely prevents stream.Write (which does heavy VAD/STT correlation)
	// from blocking the speaker playback, eliminating audio gaps.
	inputChan := make(chan []byte, 1024)
	go func() {
		for chunk := range inputChan {
			_ = stream.Write(chunk)
		}
	}()

	onSamples := func(pOutput, pInput []byte, frameCount uint32) {
		// CAPTURE: Copy input and send to channel asynchronously
		if pInput != nil {
			inCopy := make([]byte, len(pInput))
			copy(inCopy, pInput)
			select {
			case inputChan <- inCopy:
			default:
				// Drop if the pipeline falls too far behind to keep real-time
			}
		}

		// PLAYBACK: Minimize lock hold time to prevent audio artifacts
		if pOutput != nil {
			playbackMu.Lock()
			audioLen := len(playbackBytes)
			playbackMu.Unlock()

			// Only wait for buffer if we have absolutely nothing queued
			// Otherwise, start playback immediately and gracefully handle underruns
			if audioLen == 0 {
				// Complete underrun - output silence
				for i := range pOutput {
					pOutput[i] = 0
				}
				return
			}

			// Determine how much to copy
			bytesToCopy := len(pOutput)
			if audioLen < bytesToCopy {
				bytesToCopy = audioLen
			}
			bytesToCopy -= bytesToCopy % 2 // Keep samples aligned

			// Copy data (hold lock only for this operation)
			playbackMu.Lock()
			n := copy(pOutput[:bytesToCopy], playbackBytes[:bytesToCopy])
			playbackBytes = playbackBytes[n:]
			playbackMu.Unlock()

			// Notify after lock is released
			if n > 0 {
				// record exact samples being played so echo suppressor has accurate reference
				stream.RecordPlayedOutput(pOutput[:n])
				stream.NotifyAudioPlayed()
				// Log end-to-end (user -> actual audio playback) once per turn
				if !e2eLogged {
					bd := stream.GetLatencyBreakdown()
					// only print when we have meaningful data
					if bd.UserToPlay > 0 || bd.UserToTTSFirstByte > 0 || bd.UserToLLM > 0 || bd.UserToSTT > 0 {
						fmt.Printf("\r\033[K⏱️ [LATENCY] user→stt=%dms stt=%dms user→llm=%dms llm=%dms user→tts_first=%dms llm→tts_first=%dms tts_total=%dms user→play=%dms\n",
							bd.UserToSTT, bd.STT, bd.UserToLLM, bd.LLM, bd.UserToTTSFirstByte, bd.LLMToTTSFirstByte, bd.TTSTotal, bd.UserToPlay)
						e2eLogged = true
					}
				}
			}

			// Pad remaining output with silence (graceful underrun)
			if n < len(pOutput) {
				for i := n; i < len(pOutput); i++ {
					pOutput[i] = 0
				}
			}
		}
	}

	deviceConfig := malgo.DefaultDeviceConfig(malgo.Duplex)
	deviceConfig.Capture.Format = malgo.FormatS16
	deviceConfig.Capture.Channels = 1
	deviceConfig.Playback.Format = malgo.FormatS16
	deviceConfig.Playback.Channels = 1
	deviceConfig.SampleRate = SampleRate
	deviceConfig.Alsa.NoMMap = 1

	device, err := malgo.InitDevice(mctx.Context, deviceConfig, malgo.DeviceCallbacks{
		Data: onSamples,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer device.Uninit()

	if err := device.Start(); err != nil {
		log.Fatal(err)
	}

	// Event loop: handle orchestrator events and update playback buffer / device
	go func() {
		for event := range stream.Events() {
			switch event.Type {
			case orchestrator.UserSpeaking:
				fmt.Printf("\r\033[K🎤 [USER] Speaking...\n")
			case orchestrator.UserStopped:
				fmt.Printf("\r\033[K⌛ [STT] Processing...\n")
			case orchestrator.TranscriptFinal:
				fmt.Printf("\r\033[K📝 [TRANSCRIPT] %s\n", event.Data.(string))
				// export last captured user audio (raw + post-processed) for debugging
				raw, proc := stream.ExportLastUserAudio()
				if raw != nil {
					ts := time.Now().Format("20060102-150405")
					rawPath := fmt.Sprintf("/tmp/lokutor_user_raw_%s.wav", ts)
					procPath := fmt.Sprintf("/tmp/lokutor_user_processed_%s.wav", ts)
					_ = os.WriteFile(rawPath, audio.NewWavBuffer(raw, SampleRate), 0644)
					_ = os.WriteFile(procPath, audio.NewWavBuffer(proc, SampleRate), 0644)
					fmt.Printf("\r\033[K💾 Saved user audio: %s (raw), %s (processed)\n", rawPath, procPath)
				}

			case orchestrator.BotThinking:
				fmt.Printf("\r\033[K🧠 [LLM] Thinking...\n")
			case orchestrator.BotResponse:
				// print the assistant's textual response when available
				if resp, ok := event.Data.(string); ok {
					fmt.Printf("\r\033[K💬 [AGENT] %s\n", resp)
				}
			case orchestrator.BotSpeaking:
				latency := stream.GetLatency()
				if latency > 0 {
					fmt.Printf("\r\033[K🔊 [TTS] Speaking... (latency: %dms)\n", latency)
				} else {
					fmt.Printf("\r\033[K🔊 [TTS] Speaking...\n")
				}
			case orchestrator.AudioChunk:
				chunk := event.Data.([]byte)
				playbackMu.Lock()
				playbackBytes = append(playbackBytes, chunk...)
				playbackMu.Unlock()

			case orchestrator.Interrupted:
				fmt.Printf("\r\033[K🛑 [INTERRUPTED] User started talking.\n")
				// Fast interruption: just clear the playback buffer instead of stopping device
				// This is much more responsive than device.Stop()/Start() which can add latency
				playbackMu.Lock()
				playbackBytes = nil
				playbackMu.Unlock()
			case orchestrator.ErrorEvent:
				fmt.Printf("\r\033[K❌ [ERROR] %v\n", event.Data)
			}
		}
	}()

	// Wait for SIGINT / SIGTERM and perform best-effort cleanup so the
	// audio thread and streams are unblocked before exit.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	sigReceived := <-sig
	fmt.Printf("\nShutting down (signal=%v)...\n", sigReceived)

	// stop audio device to ensure audio callback returns promptly
	fmt.Println("debug: calling device.Stop()")
	_ = device.Stop()
	fmt.Println("debug: device.Stop() returned")

	// explicitly close stream to cancel pipelines
	fmt.Println("debug: calling stream.Close()")
	stream.Close()
	fmt.Println("debug: stream.Close() returned")

	// give a small grace period for cleanup
	time.Sleep(50 * time.Millisecond)
	fmt.Println("debug: exiting main")
}
