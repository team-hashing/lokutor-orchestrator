package audio

import (
	"bytes"
	"testing"
)

func TestNewWavBuffer(t *testing.T) {
	pcm := []byte{0x01, 0x02, 0x03, 0x04}
	sampleRate := 44100
	wav := NewWavBuffer(pcm, sampleRate)

	if !bytes.HasPrefix(wav, []byte("RIFF")) {
		t.Errorf("Expected RIFF prefix")
	}

	if !bytes.Contains(wav, []byte("WAVE")) {
		t.Errorf("Expected WAVE format identifier")
	}

	expectedLen := 44 + len(pcm)
	if len(wav) != expectedLen {
		t.Errorf("Expected length %d, got %d", expectedLen, len(wav))
	}
}
