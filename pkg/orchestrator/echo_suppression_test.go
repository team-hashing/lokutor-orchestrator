package orchestrator

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lokutor-ai/lokutor-orchestrator/pkg/audio"
)

// helper: generate a sine wave (16-bit LE PCM)
func generateSine(freq float64, durationMs int, sampleRate int, amp float64) []byte {
	n := sampleRate * durationMs / 1000
	buf := make([]byte, n*2)
	for i := 0; i < n; i++ {
		t := float64(i) / float64(sampleRate)
		v := amp * math.Sin(2*math.Pi*freq*t)
		s := int16(v * 32767)
		buf[2*i] = byte(s)
		buf[2*i+1] = byte(s >> 8)
	}
	return buf
}

// energy of a PCM slice (sum of squared samples)
func pcmEnergy(b []byte) float64 {
	if len(b) < 2 {
		return 0
	}
	sum := 0.0
	for i := 0; i < len(b)-1; i += 2 {
		s := int16(b[i]) | (int16(b[i+1]) << 8)
		f := float64(s) / 32768.0
		sum += f * f
	}
	return sum
}

func TestEchoSuppressor_PostProcess(t *testing.T) {
	// synthesize: played audio (A), user audio (B). Mic contains attenuated A (echo)
	// followed by B. PostProcess should mute segments matching A but keep B.
	sr := 44100
	played := generateSine(440, 500, sr, 0.8) // 0.5s
	user := generateSine(1200, 300, sr, 0.8)  // 0.3s (different freq)

	// mic: silence (100ms) + echo(played attenuated) + user + echo
	silence := make([]byte, sr*100/1000*2)
	echoAtt := make([]byte, len(played))
	for i := 0; i < len(played); i += 2 {
		// attenuate by 0.25
		s := int16(played[i]) | (int16(played[i+1]) << 8)
		s = int16(float64(s) * 0.25)
		echoAtt[i] = byte(s)
		echoAtt[i+1] = byte(s >> 8)
	}

	mic := append([]byte{}, silence...)
	mic = append(mic, echoAtt...)
	mic = append(mic, user...)
	mic = append(mic, echoAtt...)

	es := NewEchoSuppressor()
	// feed reference (what was played to speaker)
	es.RecordPlayedAudio(played)
	// ensure postprocess uses reference even if lastTTSTime check would block
	es.lastTTSTime = time.Now()

	out := es.PostProcess(mic)

	// measure energies around known offsets
	offEcho1 := len(silence)
	offUser := offEcho1 + len(echoAtt)
	offEcho2 := offUser + len(user)

	eEcho1Before := pcmEnergy(mic[offEcho1 : offEcho1+len(echoAtt)])
	eEcho1After := pcmEnergy(out[offEcho1 : offEcho1+len(echoAtt)])
	eUserBefore := pcmEnergy(mic[offUser : offUser+len(user)])
	eUserAfter := pcmEnergy(out[offUser : offUser+len(user)])
	// second echo
	eEcho2Before := pcmEnergy(mic[offEcho2 : offEcho2+len(echoAtt)])
	eEcho2After := pcmEnergy(out[offEcho2 : offEcho2+len(echoAtt)])

	// Expect echo energy reduced by large factor (>90%) while user energy preserved
	if eEcho1After > eEcho1Before*0.2 {
		t.Fatalf("echo1 not sufficiently suppressed: before=%v after=%v", eEcho1Before, eEcho1After)
	}
	if eEcho2After > eEcho2Before*0.2 {
		t.Fatalf("echo2 not sufficiently suppressed: before=%v after=%v", eEcho2Before, eEcho2After)
	}
	if math.Abs(eUserAfter-eUserBefore) > eUserBefore*0.05 {
		t.Fatalf("user audio altered unexpectedly: before=%v after=%v", eUserBefore, eUserAfter)
	}

	// write WAV files to /tmp for manual inspection
	tmp := os.TempDir()
	inPath := filepath.Join(tmp, "echo_test_input.wav")
	outPath := filepath.Join(tmp, "echo_test_output.wav")
	_ = os.WriteFile(inPath, audio.NewWavBuffer(mic, sr), 0644)
	_ = os.WriteFile(outPath, audio.NewWavBuffer(out, sr), 0644)

	// brief helpful log for developer
	t.Logf("wrote test files: %s, %s (inspect manually)", inPath, outPath)
}

func TestEchoSuppressor_IsEchoCorrelation(t *testing.T) {
	// Sanity-check calculateCorrelation + IsEcho
	es := NewEchoSuppressor()
	played := generateSine(440, 200, 44100, 0.8)
	es.RecordPlayedAudio(played)
	es.lastTTSTime = time.Now()

	// identical frame (use tail to match refCompare behavior) should be detected as echo
	frame := played[len(played)-1764:]
	corr := es.calculateCorrelation(frame, es.playedAudioBuf.Bytes())
	if corr <= es.echoThreshold {
		t.Fatalf("expected high correlation for identical frame; corr=%v threshold=%v", corr, es.echoThreshold)
	}
	if !es.IsEcho(frame) {
		t.Fatalf("IsEcho returned false despite corr=%v", corr)
	}

	// different frequency should NOT be detected
	different := generateSine(880, 200, 44100, 0.8)
	frame2 := different[:1764]
	corr2 := es.calculateCorrelation(frame2, es.playedAudioBuf.Bytes())
	if corr2 > es.echoThreshold {
		t.Fatalf("unexpectedly high correlation for different signal; corr=%v", corr2)
	}
	if es.IsEcho(frame2) {
		t.Fatal("unexpected echo detection for different signal")
	}
}
