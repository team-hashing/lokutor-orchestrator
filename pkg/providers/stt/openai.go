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

type OpenAISTT struct {
	apiKey     string
	url        string
	model      string
	sampleRate int
}

func NewOpenAISTT(apiKey string, model string) *OpenAISTT {
	if model == "" {
		model = "whisper-1"
	}
	return &OpenAISTT{
		apiKey:     apiKey,
		url:        "https://api.openai.com/v1/audio/transcriptions",
		model:      model,
		sampleRate: 44100,
	}
}

func (s *OpenAISTT) SetSampleRate(rate int) {
	s.sampleRate = rate
}

func (s *OpenAISTT) Name() string {
	return "openai_stt"
}

func (s *OpenAISTT) Transcribe(ctx context.Context, audioPCM []byte, lang orchestrator.Language) (string, error) {
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
	if _, err := part.Write(wavData); err != nil {
		return "", err
	}
	writer.Close()

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
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("openai error: %s (status %d)", string(respBody), resp.StatusCode)
	}

	var result struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	return result.Text, nil
}
