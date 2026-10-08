package acp

import (
	"math"
	"os"
	"testing"
	"testing/synctest"
	"time"
)

// speedMatching is a declared simulation controller, not a reconstruction of HP firmware.
// Its input is actual drive-buffer admission; it cannot manufacture bytes or suppress starvation.
type speedMatching struct {
	last       time.Time
	accepted   int64
	feed, rate float64
}

type speedSample struct {
	elapsed            time.Duration
	buffer             int64
	feed, target, rate float64
}

func (m *ltfsModel) matchSpeed(now time.Time) int64 {
	// Fixed-speed controls exercise the same physical byte ledger without adaptation.
	if !m.config.adaptive {
		return m.config.streamRate
	}
	m.lock.Lock()
	defer m.lock.Unlock()
	s := &m.matching
	dt := now.Sub(s.last)
	if dt < m.config.controlPeriod {
		return int64(s.rate)
	}

	// Smooth actual native-byte arrivals independently of file, hash, Write and Close boundaries.
	incoming := float64(m.driverAccepted-s.accepted) / dt.Seconds()
	weight := dt.Seconds() / (m.config.smoothing.Seconds() + dt.Seconds())
	s.feed += (incoming - s.feed) * weight
	target := s.feed + (float64(m.drivePending)-m.config.targetFill*float64(m.config.driveBufferBytes))/m.config.correction.Seconds()
	target = min(float64(m.config.bytesPerSecond), max(float64(m.config.minimumRate), target))

	// Bound acceleration rather than instantly mirroring a host burst; the physical consumer remains independent.
	change := float64(m.config.slewPerSecond) * dt.Seconds()
	if math.Abs(target-s.rate) >= 1_000_000 {
		s.rate += min(change, max(-change, target-s.rate))
	}
	s.rate = min(float64(m.config.bytesPerSecond), max(float64(m.config.minimumRate), s.rate))
	s.last, s.accepted = now, m.driverAccepted
	m.controls = append(m.controls, speedSample{time.Since(m.started), m.drivePending, s.feed, target, s.rate})
	return int64(s.rate)
}

func TestMockTapeAdaptiveSpeedTracksActualBufferSupply(t *testing.T) {
	// A full-size drive buffer must support changing in-range supply without synthetic throughput-based stops.
	synctest.Test(t, func(t *testing.T) {
		m := newLTFSModel(tapeProfile(true))
		out := &ltfsOutput{model: m, path: "adaptive"}
		chunk := make([]byte, 1<<20)
		for _, offered := range []int64{120_000_000, 135_000_000, 110_000_000} {
			for range 1536 {
				if _, err := out.Write(chunk); err != nil {
					t.Fatal(err)
				}
				time.Sleep(time.Duration(int64(len(chunk)) * int64(time.Second) / offered))
			}

			// The end of each sustained phase must track its input, not merely change speed during startup.
			m.lock.Lock()
			cutoff := time.Since(m.started) - 3*time.Second
			var count int
			var sum float64
			for _, sample := range m.controls {
				if sample.elapsed >= cutoff {
					sum += sample.rate
					count++
				}
			}
			m.lock.Unlock()
			if count < 10 || math.Abs(sum/float64(count)-float64(offered)) > 5_000_000 {
				t.Errorf("speed did not settle near supply: offered=%d samples=%d mean=%f", offered, count, sum/float64(count))
			}
		}
		if err := out.Close(); err != nil {
			t.Fatal(err)
		}
		if err := m.validate(); err != nil {
			t.Fatal(err)
		}
		m.shutdown()

		// The controller really changes speed, remains bounded and preserves continuous tape motion.
		if m.backhitches != 0 {
			t.Fatalf("in-range supply caused %d backhitches", m.backhitches)
		}
		low, high := float64(140_000_000), float64(100_000_000)
		for _, s := range m.controls {
			if s.rate < 100_000_000 || s.rate > 140_000_000 {
				t.Fatalf("speed outside stress range: %+v", s)
			}
			low, high = min(low, s.rate), max(high, s.rate)
		}
		if high-low < 15_000_000 {
			t.Fatalf("speed did not adapt meaningfully: min=%f max=%f", low, high)
		}

		// Opt-in traces expose the same deterministic controller checked above.
		if directory := os.Getenv("ACP_MOCK_REPORT_DIR"); directory != "" {
			writeTapeModelReport(t, directory, m)
		}
	})
}

func TestMockTapeAdaptiveSpeedStillStarvesBelowMinimum(t *testing.T) {
	// Adaptive control can reduce speed to 100 MB/s but cannot compensate for a persistent 80 MB/s feed.
	synctest.Test(t, func(t *testing.T) {
		m := newLTFSModel(tapeProfile(true))
		out := &ltfsOutput{model: m, path: "underfed"}
		chunk := make([]byte, 1<<20)
		for range 2048 {
			if _, err := out.Write(chunk); err != nil {
				t.Fatal(err)
			}
			time.Sleep(time.Duration(int64(len(chunk)) * int64(time.Second) / 80_000_000))
		}
		if err := out.Close(); err != nil {
			t.Fatal(err)
		}
		if err := m.validate(); err != nil {
			t.Fatal(err)
		}
		m.shutdown()
		if m.backhitches == 0 {
			t.Fatal("adaptive controller hid actual drive-buffer exhaustion")
		}
	})
}
