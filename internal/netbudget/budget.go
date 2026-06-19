/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

// Package netbudget computes an adaptive per-fetch byte budget from an
// EWMA estimate of recently observed network throughput.
//
// The motivation: rather than fixing a single chunk size at image-build
// time (and forcing operators to pick large chunks for fast links and
// small chunks for slow links), the lazy-loading cache issues fetches
// sized to take a configurable wall-clock time per request — typically
// 1–2 seconds.  Faster networks naturally request more bytes per fetch
// (saturating their pipe with one in-flight Range request); slower or
// higher-latency networks shrink the budget so that no single fetch
// hangs the foreground path for many seconds.
//
// The tracker observes (bytes, duration) tuples from completed fetches
// and produces a budget = bytes_per_sec × target_duration.  Both are
// clamped to a (Min, Max) range so a single anomalous fetch can't push
// the next batch to 0 bytes or to gigabytes.
//
// Concurrency: all methods are safe for concurrent use.  The tracker
// keeps a single bytes-per-second EWMA shared across all observers.
// This is intentional: when multiple workers fetch in parallel, each
// observes the actual goodput of its connection, not the aggregate
// link bandwidth.  In our cache layer the warmer runs at concurrency=1
// (one big batch saturates the link), so per-tracker contention is
// uninteresting.
package netbudget

import (
	"sync"
	"time"
)

// Defaults reflect typical container-image lazy-pull conditions:
//
//   - TargetDuration = 1.5 s: fast enough that foreground (fanotify)
//     latency is bounded by ~one batch worst-case; slow enough that
//     HTTP framing overhead doesn't dominate.
//   - MinBytes = 4 MiB: at typical compressed chunk sizes (~4.5 MiB
//     for the +zstd encoder's default frame), this guarantees at least
//     one chunk per fetch.  A smaller minimum on a very slow link
//     would defeat the optimisation — too few chunks per request.
//   - MaxBytes = 128 MiB: protects the cache against a single fetch
//     buffering hundreds of MiB at concurrency=1 in pathological
//     misobservations (e.g. an early hot-cache transfer mis-estimating
//     the link as gigabit).
//   - InitialBytesPerSec ≈ 10 MiB/s: yields an initial budget of
//     ~16 MiB.  Conservatively assumes a slow-ish home link to avoid
//     blowing the first fetch up.
const (
	DefaultTargetDuration    = 1500 * time.Millisecond
	DefaultMinBytes          = int64(4 * 1024 * 1024)
	DefaultMaxBytes          = int64(128 * 1024 * 1024)
	DefaultInitialBytesPerSec = float64(10 * 1024 * 1024)

	// alpha is the EWMA smoothing factor.  Higher = more responsive,
	// noisier; lower = smoother but slow to adapt.  0.3 weights the
	// most recent observation at 30% and history at 70%, which
	// reaches a steady state in roughly 5–10 fetches — fast enough
	// to learn the link during a single image pull but not so fast
	// that one slow chunk halves the budget.
	alpha = 0.3
)

// Tracker holds an EWMA estimate of bytes-per-second and produces an
// adaptive byte budget for the next fetch.
//
// Construct with NewTracker.  Use Observe to feed completed-fetch
// (bytes, duration) measurements and Budget to query the current
// per-fetch byte budget.
type Tracker struct {
	mu             sync.Mutex
	bytesPerSec    float64
	targetDuration time.Duration
	minBytes       int64
	maxBytes       int64
}

// NewTracker returns a Tracker with the given parameters.  Pass
// negative or zero values to fall back to defaults.
func NewTracker(targetDuration time.Duration, minBytes, maxBytes int64, initialBytesPerSec float64) *Tracker {
	t := &Tracker{
		targetDuration: targetDuration,
		minBytes:       minBytes,
		maxBytes:       maxBytes,
		bytesPerSec:    initialBytesPerSec,
	}
	if t.targetDuration <= 0 {
		t.targetDuration = DefaultTargetDuration
	}
	if t.minBytes <= 0 {
		t.minBytes = DefaultMinBytes
	}
	if t.maxBytes <= 0 || t.maxBytes < t.minBytes {
		t.maxBytes = DefaultMaxBytes
		if t.maxBytes < t.minBytes {
			t.maxBytes = t.minBytes
		}
	}
	if t.bytesPerSec <= 0 {
		t.bytesPerSec = DefaultInitialBytesPerSec
	}
	return t
}

// NewDefaultTracker returns a Tracker constructed with the package
// defaults — the form you almost always want for the cache layer.
func NewDefaultTracker() *Tracker {
	return NewTracker(DefaultTargetDuration, DefaultMinBytes, DefaultMaxBytes, DefaultInitialBytesPerSec)
}

// Budget returns the current byte budget for the next fetch: the EWMA
// throughput multiplied by the target duration, clamped to [Min, Max].
//
// Callers should treat this as a SOFT TARGET: the actual fetch may
// fall short (no more chunks to coalesce) or slightly over (a single
// large chunk pushes past the budget).  The tracker doesn't penalise
// either case — Observe just feeds back the measured rate.
func (t *Tracker) Budget() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	b := int64(t.bytesPerSec * t.targetDuration.Seconds())
	if b < t.minBytes {
		b = t.minBytes
	}
	if b > t.maxBytes {
		b = t.maxBytes
	}
	return b
}

// Observe records the byte count and wall-clock duration of a
// completed fetch and updates the EWMA throughput estimate.
//
// Pathological inputs (bytes ≤ 0, duration ≤ 0) are silently ignored
// to keep callers free of conditional logic.  Very small (< 1 ms)
// durations are treated as 1 ms to avoid division spikes that would
// otherwise push the estimate to absurd values.
func (t *Tracker) Observe(bytes int64, duration time.Duration) {
	if bytes <= 0 || duration < 0 {
		return
	}
	if duration < time.Millisecond {
		duration = time.Millisecond
	}
	rate := float64(bytes) / duration.Seconds()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.bytesPerSec = alpha*rate + (1.0-alpha)*t.bytesPerSec
}

// BytesPerSec returns the current EWMA throughput estimate.  Exposed
// for diagnostics / logs; the cache layer does not consume it.
func (t *Tracker) BytesPerSec() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.bytesPerSec
}

// Snapshot returns a stable trio (budget, bytesPerSec, targetDuration)
// for log lines that want to print everything atomically.  Avoids the
// race where Budget() and BytesPerSec() are sampled at different times.
func (t *Tracker) Snapshot() (budget int64, bytesPerSec float64, targetDuration time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	b := int64(t.bytesPerSec * t.targetDuration.Seconds())
	if b < t.minBytes {
		b = t.minBytes
	}
	if b > t.maxBytes {
		b = t.maxBytes
	}
	return b, t.bytesPerSec, t.targetDuration
}
