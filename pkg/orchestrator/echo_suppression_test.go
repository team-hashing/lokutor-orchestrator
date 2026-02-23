package orchestrator

import (
	"fmt"
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

// helper to convert float64 samples back to 16-bit bytes (little-endian)
func samplesToBytes(samples []float64) []byte {
	buf := make([]byte, len(samples)*2)
	for i, v := range samples {
		s := int16(v * 32767)
		buf[2*i] = byte(s)
		buf[2*i+1] = byte(s >> 8)
	}
	return buf
}

// run a PostProcess scenario with the provided rates, generating a simple
// played tone and a different user tone. Returns the processed output for
// further inspection.
func runPostProcessScenario(t *testing.T, playRate, inputRate int) {
	// create sine waves at the respective sample rates
	played := generateSine(440, 500, playRate, 0.8)
	user := generateSine(1200, 300, inputRate, 0.8)

	// build mic input: 100ms silence + attenuated played (resampled if needed) + user + replayed echo
	var echoAtt []byte
	if playRate == inputRate {
		echoAtt = make([]byte, len(played))
		for i := 0; i < len(played); i += 2 {
			// attenuate by 0.25
			s := int16(played[i]) | (int16(played[i+1]) << 8)
			s = int16(float64(s) * 0.25)
			echoAtt[i] = byte(s)
			echoAtt[i+1] = byte(s >> 8)
		}
	} else {
		// resample played audio to input rate before attenuating
		ps := bytesToSamples(played)
		rs := resample(ps, playRate, inputRate)
		echoAtt = samplesToBytes(rs)
		for i := 0; i < len(echoAtt); i += 2 {
			s := int16(echoAtt[i]) | (int16(echoAtt[i+1]) << 8)
			s = int16(float64(s) * 0.25)
			echoAtt[i] = byte(s)
			echoAtt[i+1] = byte(s >> 8)
		}
	}

	silence := make([]byte, inputRate*100/1000*2)
	mic := append([]byte{}, silence...)
	mic = append(mic, echoAtt...)
	mic = append(mic, user...)
	mic = append(mic, echoAtt...)

	// configure suppressor
	es := NewEchoSuppressorWithRates(playRate, inputRate)
	es.RecordPlayedAudio(played)
	es.lastTTSTime = time.Now()

	out := es.PostProcess(mic)

	// evaluate energies
	offEcho1 := len(silence)
	offUser := offEcho1 + len(echoAtt)
	offEcho2 := offUser + len(user)

	eEcho1Before := pcmEnergy(mic[offEcho1 : offEcho1+len(echoAtt)])
	eEcho1After := pcmEnergy(out[offEcho1 : offEcho1+len(echoAtt)])
	eUserBefore := pcmEnergy(mic[offUser : offUser+len(user)])
	eUserAfter := pcmEnergy(out[offUser : offUser+len(user)])
	eEcho2Before := pcmEnergy(mic[offEcho2 : offEcho2+len(echoAtt)])
	eEcho2After := pcmEnergy(out[offEcho2 : offEcho2+len(echoAtt)])

	if eEcho1After > eEcho1Before*0.2 {
		t.Fatalf("echo1 not sufficiently suppressed at rates play=%d input=%d: before=%v after=%v", playRate, inputRate, eEcho1Before, eEcho1After)
	}
	if eEcho2After > eEcho2Before*0.2 {
		t.Fatalf("echo2 not sufficiently suppressed at rates play=%d input=%d: before=%v after=%v", playRate, inputRate, eEcho2Before, eEcho2After)
	}
	if math.Abs(eUserAfter-eUserBefore) > eUserBefore*0.05 {
		t.Fatalf("user audio altered unexpectedly at rates play=%d input=%d: before=%v after=%v", playRate, inputRate, eUserBefore, eUserAfter)
	}

	// optionally drop files for manual inspection (quiet unless verbose)
	if testing.Verbose() {
		tmp := os.TempDir()
		inPath := filepath.Join(tmp, fmt.Sprintf("echo_test_input_%d_%d.wav", playRate, inputRate))
		outPath := filepath.Join(tmp, fmt.Sprintf("echo_test_output_%d_%d.wav", playRate, inputRate))
		_ = os.WriteFile(inPath, audio.NewWavBuffer(mic, inputRate), 0644)
		_ = os.WriteFile(outPath, audio.NewWavBuffer(out, inputRate), 0644)
		t.Logf("wrote test files: %s, %s", inPath, outPath)
	}
}

func TestEchoSuppressor_PostProcess(t *testing.T) {
	t.Run("same-rate", func(t *testing.T) {
		runPostProcessScenario(t, 44100, 44100)
	})
	t.Run("mismatch-44k-to-16k", func(t *testing.T) {
		runPostProcessScenario(t, 44100, 16000)
	})
}

func TestEchoSuppressor_IsEchoCorrelation(t *testing.T) {
	scenarios := []struct {
		name      string
		playRate  int
		inputRate int
	}{
		{"same-rate", 44100, 44100},
		{"mismatch-44k-to-16k", 44100, 16000},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			// sanity-check maxCorrelationSamples + IsEcho with configured rates
			es := NewEchoSuppressorWithRates(sc.playRate, sc.inputRate)
			played := generateSine(440, 200, sc.playRate, 0.8)
			es.RecordPlayedAudio(played)
			es.lastTTSTime = time.Now()

			frame := played[len(played)-1764:]
			if sc.playRate != sc.inputRate {
				// resample to input rate before passing to IsEcho()
				samp := bytesToSamples(frame)
				frame = samplesToBytes(resample(samp, sc.playRate, sc.inputRate))
			}

			ref := es.getRecentSamples(0)
			threshold := 0.80

			// compute correlation against reference; resample input back up if
			// necessary so that the two arrays are comparable.
			inp := bytesToSamples(frame)
			if sc.playRate != sc.inputRate {
				inp = resample(inp, sc.inputRate, sc.playRate)
			}
			corr := es.maxCorrelationSamples(inp, ref)
			if corr <= threshold {
				t.Fatalf("expected high correlation for identical frame; corr=%v threshold=%v", corr, threshold)
			}
			if !es.IsEcho(frame) {
				t.Fatalf("IsEcho returned false despite corr=%v", corr)
			}

			different := generateSine(880, 200, sc.playRate, 0.8)
			frame2 := different[:1764]
			if sc.playRate != sc.inputRate {
				samp := bytesToSamples(frame2)
				frame2 = samplesToBytes(resample(samp, sc.playRate, sc.inputRate))
			}
			inp2 := bytesToSamples(frame2)
			if sc.playRate != sc.inputRate {
				inp2 = resample(inp2, sc.inputRate, sc.playRate)
			}
			corr2 := es.maxCorrelationSamples(inp2, ref)
			if corr2 > threshold {
				t.Fatalf("unexpectedly high correlation for different signal; corr=%v", corr2)
			}
			if es.IsEcho(frame2) {
				t.Fatal("unexpected echo detection for different signal")
			}
		})
	}
}

func TestEchoSuppressor_SetSampleRates(t *testing.T) {
	es := NewEchoSuppressor()
	if es.playbackSampleRate != 44100 || es.inputSampleRate != 44100 {
		t.Fatalf("default rates incorrect: got %d/%d", es.playbackSampleRate, es.inputSampleRate)
	}
	es.SetSampleRates(48000, 16000)
	if es.playbackSampleRate != 48000 || es.inputSampleRate != 16000 {
		t.Fatalf("SetSampleRates did not update fields: got %d/%d", es.playbackSampleRate, es.inputSampleRate)
	}
}
