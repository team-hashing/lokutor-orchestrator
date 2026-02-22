package orchestrator

import (
	"bytes"
	"math"
	"sync"
	"time"
)

// EchoSuppressor detects and filters out speaker echo from microphone input.
// It uses correlation-based analysis to detect when input audio matches recently played audio.
type EchoSuppressor struct {
	mu             sync.Mutex
	playedAudioBuf *bytes.Buffer // Rolling buffer of played audio
	maxBufSize     int           // Max size of played audio buffer
	echoThreshold  float64       // Correlation threshold above which we consider audio to be echo
	echoSilenceMS  int           // How long to suppress echoes after TTS stops (ms)
	lastTTSTime    time.Time     // When we last played audio
	enabled        bool
	// For real-time detection we also keep a short recent-playback duration to
	// tolerate playback-to-mic latency (ms).
	recentPlaybackWindowMS int
}

// NewEchoSuppressor creates a new echo suppressor
func NewEchoSuppressor() *EchoSuppressor {
	return &EchoSuppressor{
		playedAudioBuf:         new(bytes.Buffer),
		maxBufSize:             176400, // ~2 seconds at 44.1kHz, 16-bit mono
		echoThreshold:          0.55,   // slightly more sensitive by default
		echoSilenceMS:          1200,   // cover longer playback→mic delays
		recentPlaybackWindowMS: 1200,
		enabled:                true,
	}
}

// RecordPlayedAudio records audio that was just sent to speakers
func (es *EchoSuppressor) RecordPlayedAudio(chunk []byte) {
	if !es.enabled || len(chunk) == 0 {
		return
	}

	es.mu.Lock()
	defer es.mu.Unlock()

	es.playedAudioBuf.Write(chunk)
	es.lastTTSTime = time.Now()

	// Keep buffer size bounded
	if es.playedAudioBuf.Len() > es.maxBufSize {
		data := es.playedAudioBuf.Bytes()
		trim := data[len(data)-es.maxBufSize:]
		es.playedAudioBuf.Reset()
		es.playedAudioBuf.Write(trim)
	}
}

// IsEcho checks if input audio is primarily echo from speakers
func (es *EchoSuppressor) IsEcho(inputChunk []byte) bool {
	if !es.enabled || len(inputChunk) == 0 {
		return false
	}

	es.mu.Lock()
	defer es.mu.Unlock()

	// If we haven't played audio recently, no echo possible
	if time.Since(es.lastTTSTime) > time.Duration(es.echoSilenceMS)*time.Millisecond {
		return false
	}

	playedData := es.playedAudioBuf.Bytes()
	if len(playedData) == 0 {
		return false
	}

	// Calculate correlation between input and played audio
	correlation := es.calculateCorrelation(inputChunk, playedData)

	// If correlation is high, it's echo
	if correlation > es.echoThreshold {
		return true
	}

	// Fallback to envelope correlation for 'S' sounds
	envCorr := maxEnvelopeCorrelation(bytesToSamples(inputChunk), bytesToSamples(playedData), 8)
	return envCorr > es.echoThreshold+0.05
}

// calculateCorrelation computes the normalized cross-correlation between input and reference
// Returns a value between 0 and 1, where 1 means perfect correlation
func (es *EchoSuppressor) calculateCorrelation(input, reference []byte) float64 {
	if len(input) == 0 || len(reference) == 0 {
		return 0
	}

	// Convert bytes to float64 samples
	inputSamples := bytesToSamples(input)
	refSamples := bytesToSamples(reference)

	if len(inputSamples) == 0 || len(refSamples) == 0 {
		return 0
	}

	// For efficiency, only compare the last part of reference with input
	// This accounts for speaker latency
	compareLen := len(inputSamples)
	if compareLen > len(refSamples) {
		compareLen = len(refSamples)
	}

	refStart := len(refSamples) - compareLen
	refCompare := refSamples[refStart:]

	// Calculate energy of both signals (use refCompare energy, not whole ref)
	inputEnergy := calculateEnergy(inputSamples)
	refCompareEnergy := calculateEnergy(refCompare)

	if inputEnergy == 0 || refCompareEnergy == 0 {
		return 0
	}

	// Calculate cross-correlation
	correlation := 0.0
	for i := 0; i < len(inputSamples) && i < len(refCompare); i++ {
		correlation += inputSamples[i] * refCompare[i]
	}

	// Normalize by the geometric mean of energies
	normFactor := math.Sqrt(inputEnergy * refCompareEnergy)
	if normFactor == 0 {
		return 0
	}

	normalizedCorr := correlation / normFactor

	// Clamp to [0, 1]
	if normalizedCorr < 0 {
		normalizedCorr = 0
	} else if normalizedCorr > 1 {
		normalizedCorr = 1
	}

	return normalizedCorr
}

// bytesToSamples converts byte array (16-bit little-endian) to float64 samples in [-1, 1]
func bytesToSamples(data []byte) []float64 {
	samples := make([]float64, 0, len(data)/2)

	for i := 0; i < len(data)-1; i += 2 {
		sample := int16(data[i]) | (int16(data[i+1]) << 8)
		normalized := float64(sample) / 32768.0
		samples = append(samples, normalized)
	}

	return samples
}

// calculateEnergy computes the sum of squared samples
func calculateEnergy(samples []float64) float64 {
	energy := 0.0
	for _, s := range samples {
		energy += s * s
	}
	return energy
}

// ClearEchoBuffer clears the played audio buffer (call when stopping TTS or interrupting)
func (es *EchoSuppressor) ClearEchoBuffer() {
	es.mu.Lock()
	defer es.mu.Unlock()
	es.playedAudioBuf.Reset()
}

// PostProcess runs offline echo removal on `input` PCM (16-bit little-endian,
// mono). It inspects fixed-size frames and mutes frames that correlate highly
// with the stored `playedAudioBuf` (uses same correlation logic as
// IsEcho). Returns a new []byte with echo frames zeroed.
//
// Note: this is conservative — it mutes entire frames classified as echo. Use
// for debugging / manual inspection. Frame duration is 20ms at 44.1kHz.
func (es *EchoSuppressor) PostProcess(input []byte) []byte {
	if !es.enabled || len(input) == 0 {
		out := make([]byte, len(input))
		copy(out, input)
		return out
	}

	const sampleRate = 44100
	const frameMs = 20
	frameBytes := (sampleRate * 2 * frameMs) / 1000 // 2 bytes per sample

	es.mu.Lock()
	ref := make([]byte, es.playedAudioBuf.Len())
	copy(ref, es.playedAudioBuf.Bytes())
	threshold := es.echoThreshold
	es.mu.Unlock()

	out := make([]byte, len(input))
	copy(out, input)

	for off := 0; off < len(input); off += frameBytes {
		end := off + frameBytes
		if end > len(input) {
			end = len(input)
		}
		frame := input[off:end]

		// compute best correlation against the reference buffer (search)
		corr := es.maxCorrelationAgainstReference(frame, ref)
		if corr > threshold {
			// mute this frame (conservative)
			for i := off; i < end; i++ {
				out[i] = 0
			}
		}
	}

	return out
}

// RemoveEchoRealtime attempts to subtract a scaled, aligned segment of the
// recently-played audio from the incoming `input` chunk in real time.
// If a good match is found (correlation > threshold) the function returns a
// cleaned copy; otherwise it returns the original input. This is a lightweight
// time-domain cancellation (single-scale subtraction), not a full AEC.
func (es *EchoSuppressor) RemoveEchoRealtime(input []byte) []byte {
	if !es.enabled || len(input) == 0 {
		out := make([]byte, len(input))
		copy(out, input)
		return out
	}

	es.mu.Lock()
	if time.Since(es.lastTTSTime) > time.Duration(es.echoSilenceMS)*time.Millisecond {
		es.mu.Unlock()
		out := make([]byte, len(input))
		copy(out, input)
		return out
	}
	ref := make([]byte, es.playedAudioBuf.Len())
	copy(ref, es.playedAudioBuf.Bytes())
	threshold := es.echoThreshold
	es.mu.Unlock()

	if len(ref) == 0 {
		out := make([]byte, len(input))
		copy(out, input)
		return out
	}

	inSamples := bytesToSamples(input)
	refSamples := bytesToSamples(ref)
	if len(inSamples) == 0 || len(refSamples) == 0 {
		out := make([]byte, len(input))
		copy(out, input)
		return out
	}

	compareLen := len(inSamples)
	if compareLen > len(refSamples) {
		compareLen = len(refSamples)
	}

	inSeg := inSamples[:compareLen]
	inEnergy := calculateEnergy(inSeg)
	if inEnergy == 0 {
		out := make([]byte, len(input))
		copy(out, input)
		return out
	}

	// search for best alignment within the reference (bounded sliding search)
	maxCorr := 0.0
	// Use a much larger stride to avoid massive CPU overhead in the tight realtime audio thread!
	// This fixes the 'slowed down and lots of gaps' issue.
	stride := compareLen / 4
	if stride < 8 {
		stride = 8 // ensure at least some minimum stride
	}

	searchRange := len(refSamples) - compareLen + 1
	for pos := 0; pos < searchRange; pos += stride {
		seg := refSamples[pos : pos+compareLen]
		segEnergy := calculateEnergy(seg)
		if segEnergy == 0 {
			continue
		}
		dot := 0.0
		for i := 0; i < compareLen; i++ {
			dot += inSeg[i] * seg[i]
		}
		corr := dot / math.Sqrt(inEnergy*segEnergy)
		if corr > maxCorr {
			maxCorr = corr
			if maxCorr >= 0.999 {
				break
			}
		}
	}

	if maxCorr < threshold {
		// fallback to envelope correlation to catch phase-shifted 'S' sounds
		// we use threshold + 0.05 for envelope since it runs slightly higher inherently
		envCorr := maxEnvelopeCorrelation(inSeg, refSamples, 8)
		if envCorr < threshold+0.05 {
			out := make([]byte, len(input))
			copy(out, input)
			return out
		}
	}

	// completely mute the segment instead of subtracting
	// outBytes is initialized to all zeros
	outBytes := make([]byte, len(input))
	// if input is longer than compareLen, copy remaining bytes unchanged
	if len(outBytes) > compareLen*2 {
		copy(outBytes[compareLen*2:], input[compareLen*2:])
	}

	return outBytes
}

// maxCorrelationAgainstReference performs a (bounded) sliding-window search of
// `reference` to find the maximum normalized correlation with `input`.
// This is intentionally expensive and used only for offline/postprocess use.
func (es *EchoSuppressor) maxCorrelationAgainstReference(input, reference []byte) float64 {
	inputSamples := bytesToSamples(input)
	refSamples := bytesToSamples(reference)

	if len(inputSamples) == 0 || len(refSamples) == 0 {
		return 0
	}

	compareLen := len(inputSamples)
	if compareLen > len(refSamples) {
		compareLen = len(refSamples)
	}

	// energies for input fixed
	inputEnergy := calculateEnergy(inputSamples[:compareLen])
	if inputEnergy == 0 {
		return 0
	}

	maxCorr := 0.0
	// choose a stride to limit CPU (small frames -> stride 8; larger -> coarser)
	stride := compareLen / 4
	if stride < 8 {
		stride = 8
	}

	searchRange := len(refSamples) - compareLen + 1
	for pos := 0; pos < searchRange; pos += stride {
		seg := refSamples[pos : pos+compareLen]
		segEnergy := calculateEnergy(seg)
		if segEnergy == 0 {
			continue
		}
		// dot product
		dot := 0.0
		for i := 0; i < compareLen; i++ {
			dot += inputSamples[i] * seg[i]
		}
		corr := dot / math.Sqrt(inputEnergy*segEnergy)
		if corr > maxCorr {
			maxCorr = corr
			if maxCorr >= 0.999 {
				return maxCorr
			}
		}
	}

	// clamp
	if maxCorr < 0 {
		maxCorr = 0
	} else if maxCorr > 1 {
		maxCorr = 1
	}

	return maxCorr
}

// maxEnvelopeCorrelation finds the maximum correlation by comparing the absolute value
// energy envelope (downsampled) of the signals. This perfectly matches 'S' sounds and high
// frequencies that decorators would otherwise scramble with room phase shifts.
func maxEnvelopeCorrelation(inSamples, refSamples []float64, decimation int) float64 {
	if len(inSamples) == 0 || len(refSamples) == 0 {
		return 0
	}
	// Create envelopes
	inEnv := make([]float64, len(inSamples)/decimation)
	for i := 0; i < len(inEnv); i++ {
		sum := 0.0
		for j := 0; j < decimation; j++ {
			sum += math.Abs(inSamples[i*decimation+j])
		}
		inEnv[i] = sum
	}

	refEnv := make([]float64, len(refSamples)/decimation)
	for i := 0; i < len(refEnv); i++ {
		sum := 0.0
		for j := 0; j < decimation; j++ {
			sum += math.Abs(refSamples[i*decimation+j])
		}
		refEnv[i] = sum
	}

	compareLen := len(inEnv)
	if compareLen > len(refEnv) {
		compareLen = len(refEnv)
	}
	if compareLen == 0 {
		return 0
	}

	inMean := 0.0
	for i := 0; i < compareLen; i++ {
		inMean += inEnv[i]
	}
	inMean /= float64(compareLen)

	inVar := 0.0
	for i := 0; i < compareLen; i++ {
		inEnv[i] -= inMean
		inVar += inEnv[i] * inEnv[i]
	}

	if inVar <= 0 {
		return 0
	}

	maxCorr := 0.0
	stride := compareLen / 4
	if stride < 2 {
		stride = 2
	}

	searchRange := len(refEnv) - compareLen + 1

	for pos := 0; pos < searchRange; pos += stride {
		refMean := 0.0
		for i := 0; i < compareLen; i++ {
			refMean += refEnv[pos+i]
		}
		refMean /= float64(compareLen)

		dot := 0.0
		refVar := 0.0
		for i := 0; i < compareLen; i++ {
			r := refEnv[pos+i] - refMean
			dot += inEnv[i] * r
			refVar += r * r
		}

		if refVar > 0 {
			corr := dot / math.Sqrt(inVar*refVar)
			if corr > maxCorr {
				maxCorr = corr
			}
		}
	}

	return maxCorr
}

// SetThreshold adjusts the echo detection sensitivity (0-1, higher = more sensitive)
func (es *EchoSuppressor) SetThreshold(threshold float64) {
	es.mu.Lock()
	defer es.mu.Unlock()
	if threshold >= 0 && threshold <= 1 {
		es.echoThreshold = threshold
	}
}

// SetEnabled enables or disables echo suppression
func (es *EchoSuppressor) SetEnabled(enabled bool) {
	es.mu.Lock()
	defer es.mu.Unlock()
	es.enabled = enabled
}
