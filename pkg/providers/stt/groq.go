package stt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/audio"
	"github.com/lokutor-ai/lokutor-orchestrator/pkg/orchestrator"
)

type GroqSTT struct {
	apiKey     string
	url        string
	model      string
	sampleRate int
}

func NewGroqSTT(apiKey string, model string) *GroqSTT {
	if model == "" {
		model = "whisper-large-v3-turbo"
	}
	return &GroqSTT{
		apiKey:     apiKey,
		url:        "https://api.groq.com/openai/v1/audio/transcriptions",
		model:      model,
		sampleRate: 44100,
	}
}

func (s *GroqSTT) SetSampleRate(rate int) {
	s.sampleRate = rate
}

func (s *GroqSTT) Transcribe(ctx context.Context, audioPCM []byte, lang orchestrator.Language) (string, error) {
	wavData := audio.NewWavBuffer(audioPCM, s.sampleRate)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	if err := writer.WriteField("model", s.model); err != nil {
		return "", err
	}

	if lang != "" {
		if err := writer.WriteField("language", string(lang)); err != nil {
			return "", err
		}
	}

	part, err := writer.CreateFormFile("file", "audio.wav")
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(part, bytes.NewReader(wavData)); err != nil {
		return "", err
	}

	if err := writer.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", s.url, body)
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+s.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errResp interface{}
		json.NewDecoder(resp.Body).Decode(&errResp)
		return "", fmt.Errorf("groq stt error (status %d): %v", resp.StatusCode, errResp)
	}

	var result struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	return result.Text, nil
}

func (s *GroqSTT) Name() string {
	return "groq-stt"
}
