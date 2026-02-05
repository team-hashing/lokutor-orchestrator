package audio

import (
	"bytes"
	"encoding/binary"
)

// NewWavBuffer adds a RIFF WAV header to raw 16-bit mono PCM data
func NewWavBuffer(pcm []byte, sampleRate int) []byte {
	buf := new(bytes.Buffer)

	// RIFF header
	buf.WriteString("RIFF")
	binary.Write(buf, binary.LittleEndian, uint32(36+len(pcm)))
	buf.WriteString("WAVE")

	// fmt sub-chunk
	buf.WriteString("fmt ")
	binary.Write(buf, binary.LittleEndian, uint32(16))           // Subchunk1Size
	binary.Write(buf, binary.LittleEndian, uint16(1))            // AudioFormat (1 = PCM)
	binary.Write(buf, binary.LittleEndian, uint16(1))            // NumChannels (1 = Mono)
	binary.Write(buf, binary.LittleEndian, uint32(sampleRate))   // SampleRate
	binary.Write(buf, binary.LittleEndian, uint32(sampleRate*2)) // ByteRate
	binary.Write(buf, binary.LittleEndian, uint16(2))            // BlockAlign
	binary.Write(buf, binary.LittleEndian, uint16(16))           // BitsPerSample

	// data sub-chunk
	buf.WriteString("data")
	binary.Write(buf, binary.LittleEndian, uint32(len(pcm)))
	buf.Write(pcm)

	return buf.Bytes()
}
