package orchestrator

import (
	"context"
	"sync"
)

type Logger interface {
	Debug(msg string, args ...interface{})

	Info(msg string, args ...interface{})

	Warn(msg string, args ...interface{})

	Error(msg string, args ...interface{})
}

type NoOpLogger struct{}

func (n *NoOpLogger) Debug(msg string, args ...interface{}) {}
func (n *NoOpLogger) Info(msg string, args ...interface{})  {}
func (n *NoOpLogger) Warn(msg string, args ...interface{})  {}
func (n *NoOpLogger) Error(msg string, args ...interface{}) {}

type STTProvider interface {
	Transcribe(ctx context.Context, audio []byte, lang Language) (string, error)
	Name() string
}

type StreamingSTTProvider interface {
	STTProvider
	StreamTranscribe(ctx context.Context, lang Language, onTranscript func(transcript string, isFinal bool) error) (chan<- []byte, error)
}

type LLMProvider interface {
	Complete(ctx context.Context, messages []Message) (string, error)
	Name() string
}

type TTSProvider interface {
	Synthesize(ctx context.Context, text string, voice Voice, lang Language) ([]byte, error)
	StreamSynthesize(ctx context.Context, text string, voice Voice, lang Language, onChunk func([]byte) error) error
	// Abort forces any in-progress synthesis to stop immediately.
	// Implementations should try to unblock StreamSynthesize / Synthesize
	// as quickly as possible (closing connections, cancelling streams, etc.).
	Abort() error
	Name() string
}

type VADProvider interface {
	Process(chunk []byte) (*VADEvent, error)
	Reset()
	Clone() VADProvider
	Name() string
}

type VADEventType string

const (
	VADSpeechStart VADEventType = "SPEECH_START"
	VADSpeechEnd   VADEventType = "SPEECH_END"
	VADSilence     VADEventType = "SILENCE"
)

type VADEvent struct {
	Type      VADEventType
	Timestamp int64
}

type EventType string

const (
	UserSpeaking      EventType = "USER_SPEAKING"
	UserStopped       EventType = "USER_STOPPED"
	TranscriptPartial EventType = "TRANSCRIPT_PARTIAL"
	TranscriptFinal   EventType = "TRANSCRIPT_FINAL"
	BotThinking       EventType = "BOT_THINKING"
	// BotResponse carries the assistant's textual response (payload is string)
	BotResponse EventType = "BOT_RESPONSE"
	BotSpeaking EventType = "BOT_SPEAKING"
	Interrupted EventType = "INTERRUPTED"
	AudioChunk  EventType = "AUDIO_CHUNK"
	ErrorEvent  EventType = "ERROR"
)

type OrchestratorEvent struct {
	Type      EventType   `json:"type"`
	SessionID string      `json:"session_id"`
	Data      interface{} `json:"data,omitempty"`
}

type Voice string

const (
	VoiceF1 Voice = "F1"
	VoiceF2 Voice = "F2"
	VoiceF3 Voice = "F3"
	VoiceF4 Voice = "F4"
	VoiceF5 Voice = "F5"
	VoiceM1 Voice = "M1"
	VoiceM2 Voice = "M2"
	VoiceM3 Voice = "M3"
	VoiceM4 Voice = "M4"
	VoiceM5 Voice = "M5"
)

type Language string

const (
	LanguageEn Language = "en"
	LanguageEs Language = "es"
	LanguageFr Language = "fr"
	LanguageDe Language = "de"
	LanguageIt Language = "it"
	LanguagePt Language = "pt"
	LanguageJa Language = "ja"
	LanguageZh Language = "zh"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Config struct {
	SampleRate         int
	Channels           int
	BytesPerSamp       int
	MaxContextMessages int
	VoiceStyle         Voice
	// Minimum number of spoken words required for a user to interrupt the bot
	// while the bot is mid-speech. When >1, interruptions are decided using
	// STT transcripts (partial/final) instead of immediate VAD-based barge-in.
	MinWordsToInterrupt int
	Language            Language
	STTTimeout          uint
	LLMTimeout          uint
	TTSTimeout          uint
}

func DefaultConfig() Config {
	return Config{
		SampleRate:          44100,
		Channels:            1,
		BytesPerSamp:        2,
		MaxContextMessages:  20,
		VoiceStyle:          VoiceF1,
		MinWordsToInterrupt: 1, // default: immediate interruption on any user speech
		Language:            LanguageEn,
		STTTimeout:          30,
		LLMTimeout:          60,
		TTSTimeout:          30,
	}
}

type ConversationSession struct {
	mu              sync.RWMutex
	ID              string
	Context         []Message
	LastUser        string
	LastAssistant   string
	MaxMessages     int
	CurrentVoice    Voice
	CurrentLanguage Language
}

func NewConversationSession(userID string) *ConversationSession {
	return &ConversationSession{
		ID:              userID,
		Context:         []Message{},
		MaxMessages:     20,
		CurrentVoice:    VoiceF1,
		CurrentLanguage: LanguageEn,
	}
}

func (s *ConversationSession) AddMessage(role, content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Context = append(s.Context, Message{Role: role, Content: content})
	if len(s.Context) > s.MaxMessages {
		s.Context = s.Context[len(s.Context)-s.MaxMessages:]
	}
	if role == "user" {
		s.LastUser = content
	} else if role == "assistant" {
		s.LastAssistant = content
	}
}

func (s *ConversationSession) ClearContext() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Context = []Message{}
	s.LastUser = ""
	s.LastAssistant = ""
}

func (s *ConversationSession) GetContextCopy() []Message {
	s.mu.RLock()
	defer s.mu.RUnlock()
	contextCopy := make([]Message, len(s.Context))
	copy(contextCopy, s.Context)
	return contextCopy
}

func (s *ConversationSession) GetCurrentVoice() Voice {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.CurrentVoice
}

func (s *ConversationSession) GetCurrentLanguage() Language {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.CurrentLanguage
}
