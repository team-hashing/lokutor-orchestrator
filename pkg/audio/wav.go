package audio

import (
	"bytes"
	"encoding/binary"
)


func NewWavBuffer(pcm []byte, sampleRate int) []byte {
	buf := new(bytes.Buffer)

	
	buf.WriteString("RIFF")
	binary.Write(buf, binary.LittleEndian, uint32(36+len(pcm)))
	buf.WriteString("WAVE")

	
	buf.WriteString("fmt ")
	binary.Write(buf, binary.LittleEndian, uint32(16))           
	binary.Write(buf, binary.LittleEndian, uint16(1))            
	binary.Write(buf, binary.LittleEndian, uint16(1))            
	binary.Write(buf, binary.LittleEndian, uint32(sampleRate))   
	binary.Write(buf, binary.LittleEndian, uint32(sampleRate*2)) 
	binary.Write(buf, binary.LittleEndian, uint16(2))            
	binary.Write(buf, binary.LittleEndian, uint16(16))           

	
	buf.WriteString("data")
	binary.Write(buf, binary.LittleEndian, uint32(len(pcm)))
	buf.Write(pcm)

	return buf.Bytes()
}
