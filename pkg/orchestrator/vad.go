package orchestrator

import (
	"math"
	"time"
)

type RMSVAD struct {
	threshold    float64
	silenceLimit time.Duration
	isSpeaking   bool
	silenceStart time.Time

	adaptiveMode bool
	noiseFloor   float64
	alpha        float64

	consecutiveFrames int
	minConfirmed      int
	lastRMS           float64
}

func NewRMSVAD(threshold float64, silenceLimit time.Duration) *RMSVAD {
	return &RMSVAD{
		threshold:    threshold,
		silenceLimit: silenceLimit,
		minConfirmed: 7,
		adaptiveMode: true,
		noiseFloor:   0.005,
		alpha:        0.05,
	}
}

func (v *RMSVAD) SetAdaptiveMode(enabled bool) {
	v.adaptiveMode = enabled
}

func (v *RMSVAD) SetMinConfirmed(count int) {
	v.minConfirmed = count
}

func (v *RMSVAD) MinConfirmed() int {
	return v.minConfirmed
}

func (v *RMSVAD) SetThreshold(threshold float64) {
	v.threshold = threshold
}

func (v *RMSVAD) Threshold() float64 {
	return v.threshold
}

func (v *RMSVAD) LastRMS() float64 {
	return v.lastRMS
}

func (v *RMSVAD) IsSpeaking() bool {
	return v.isSpeaking
}

func (v *RMSVAD) Process(chunk []byte) (*VADEvent, error) {
	rms := v.calculateRMS(chunk)
	v.lastRMS = rms
	now := time.Now()

	effectiveThreshold := v.threshold
	if v.adaptiveMode {

		if rms < v.noiseFloor {
			v.noiseFloor = rms
		} else if !v.isSpeaking && rms < v.threshold*2 {

			v.noiseFloor = (1-v.alpha)*v.noiseFloor + v.alpha*rms
		}

		adaptiveThreshold := v.noiseFloor * 2.0
		if adaptiveThreshold > effectiveThreshold {
			effectiveThreshold = adaptiveThreshold
		}

		if effectiveThreshold > 0.3 {
			effectiveThreshold = 0.3
		}
	}

	if rms > effectiveThreshold {
		v.consecutiveFrames++
		if !v.isSpeaking {

			if v.consecutiveFrames >= v.minConfirmed {
				v.isSpeaking = true
				return &VADEvent{Type: VADSpeechStart, Timestamp: now.UnixMilli()}, nil
			}
			return nil, nil
		}
		v.silenceStart = time.Time{}
		return nil, nil
	}

	v.consecutiveFrames = 0

	if v.isSpeaking {
		if v.silenceStart.IsZero() {
			v.silenceStart = now
		}

		if now.Sub(v.silenceStart) >= v.silenceLimit {
			v.isSpeaking = false
			v.silenceStart = time.Time{}
			return &VADEvent{Type: VADSpeechEnd, Timestamp: now.UnixMilli()}, nil
		}
	}

	return &VADEvent{Type: VADSilence, Timestamp: now.UnixMilli()}, nil
}

func (v *RMSVAD) Name() string {
	return "rms_vad"
}

func (v *RMSVAD) Reset() {
	v.isSpeaking = false
	v.silenceStart = time.Time{}
	v.consecutiveFrames = 0
}

func (v *RMSVAD) Clone() VADProvider {
	return &RMSVAD{
		threshold:    v.threshold,
		silenceLimit: v.silenceLimit,
		minConfirmed: v.minConfirmed,
	}
}

func (v *RMSVAD) calculateRMS(chunk []byte) float64 {
	if len(chunk) == 0 {
		return 0
	}

	var sum float64

	for i := 0; i < len(chunk)-1; i += 2 {
		sample := int16(chunk[i]) | (int16(chunk[i+1]) << 8)
		f := float64(sample) / 32768.0
		sum += f * f
	}

	return math.Sqrt(sum / float64(len(chunk)/2))
}
