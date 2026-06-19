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

package netbudget

import (
	"math"
	"sync"
	"testing"
	"time"
)

func TestNewTracker_defaults(t *testing.T) {
	tk := NewTracker(0, 0, 0, 0)
	if tk.targetDuration != DefaultTargetDuration {
		t.Errorf("targetDuration = %v, want %v", tk.targetDuration, DefaultTargetDuration)
	}
	if tk.minBytes != DefaultMinBytes {
		t.Errorf("minBytes = %d, want %d", tk.minBytes, DefaultMinBytes)
	}
	if tk.maxBytes != DefaultMaxBytes {
		t.Errorf("maxBytes = %d, want %d", tk.maxBytes, DefaultMaxBytes)
	}
	if tk.bytesPerSec != DefaultInitialBytesPerSec {
		t.Errorf("bytesPerSec = %v, want %v", tk.bytesPerSec, DefaultInitialBytesPerSec)
	}
}

func TestBudget_initialIsAroundExpected(t *testing.T) {
	tk := NewDefaultTracker()
	// 10 MiB/s * 1.5 s = 15 MiB.
	const want = int64(15 * 1024 * 1024)
	got := tk.Budget()
	// Allow ±1 MiB.
	if math.Abs(float64(got-want)) > float64(1024*1024) {
		t.Errorf("initial budget = %d, want around %d", got, want)
	}
}

func TestBudget_clampedToMin(t *testing.T) {
	// Force an artificially low throughput estimate via Observe of a
	// slow fetch.  The Min cap must prevent the budget from collapsing.
	tk := NewTracker(time.Second, 4*1024*1024, 64*1024*1024, 100) // 100 B/s initial — absurdly slow
	got := tk.Budget()
	if got != 4*1024*1024 {
		t.Errorf("budget = %d, want clamped to MinBytes = %d", got, 4*1024*1024)
	}
}

func TestBudget_clampedToMax(t *testing.T) {
	// 1 GiB/s * 1.5 s = 1.5 GiB — must clamp to Max.
	tk := NewTracker(1500*time.Millisecond, 4*1024*1024, 128*1024*1024, 1024*1024*1024)
	got := tk.Budget()
	if got != 128*1024*1024 {
		t.Errorf("budget = %d, want clamped to MaxBytes = %d", got, 128*1024*1024)
	}
}

func TestObserve_increasesBudgetForFastFetch(t *testing.T) {
	tk := NewDefaultTracker()
	before := tk.Budget()
	// Observe a fetch that completed at 100 MiB/s.  Sustained over
	// many fetches the EWMA should drift up.
	for i := 0; i < 30; i++ {
		tk.Observe(50*1024*1024, 500*time.Millisecond)
	}
	after := tk.Budget()
	if !(after > before) {
		t.Errorf("budget did not increase after fast fetches: before=%d after=%d", before, after)
	}
}

func TestObserve_decreasesBudgetForSlowFetch(t *testing.T) {
	tk := NewTracker(time.Second, 1024, 256*1024*1024, 100*1024*1024) // start at 100 MiB/s
	before := tk.Budget()
	// Observe slow fetches: 1 MiB in 2 s = 500 KiB/s.
	for i := 0; i < 30; i++ {
		tk.Observe(1024*1024, 2*time.Second)
	}
	after := tk.Budget()
	if !(after < before) {
		t.Errorf("budget did not decrease after slow fetches: before=%d after=%d", before, after)
	}
}

func TestObserve_ignoresZeroOrNegativeBytes(t *testing.T) {
	tk := NewDefaultTracker()
	before := tk.BytesPerSec()
	tk.Observe(0, time.Second)
	tk.Observe(-1, time.Second)
	after := tk.BytesPerSec()
	if before != after {
		t.Errorf("Observe with bad bytes mutated state: before=%v after=%v", before, after)
	}
}

func TestObserve_ignoresNegativeDuration(t *testing.T) {
	tk := NewDefaultTracker()
	before := tk.BytesPerSec()
	tk.Observe(1024*1024, -time.Second)
	after := tk.BytesPerSec()
	if before != after {
		t.Errorf("Observe with negative duration mutated state")
	}
}

func TestObserve_subMillisecondDurationClamped(t *testing.T) {
	tk := NewTracker(time.Second, 4*1024*1024, 1024*1024*1024, 10*1024*1024)
	// A 1 MiB fetch in 1 ns would naively be ~10 PiB/s — the clamp
	// to 1 ms keeps the rate at 1 GiB/s, the bounded ceiling.
	tk.Observe(1024*1024, 0)
	got := tk.BytesPerSec()
	const want = float64(1024 * 1024 * 1000) // 1 MiB / 1 ms
	// EWMA: 0.3 * 1024MB/s + 0.7 * 10MiB/s ≈ ~314 MiB/s.  We don't
	// assert the exact value, only that it's finite and positive.
	if math.IsInf(got, 0) || math.IsNaN(got) || got <= 0 || got > want {
		t.Errorf("EWMA after sub-ms observe is unbounded: %v", got)
	}
}

func TestSnapshot_atomicWithBudget(t *testing.T) {
	tk := NewDefaultTracker()
	b1, bps, td := tk.Snapshot()
	b2 := tk.Budget()
	if b1 != b2 {
		t.Errorf("Snapshot.budget=%d Budget()=%d (should match for an idle tracker)", b1, b2)
	}
	if bps <= 0 {
		t.Errorf("Snapshot.bytesPerSec = %v, want positive", bps)
	}
	if td != DefaultTargetDuration {
		t.Errorf("Snapshot.targetDuration = %v, want %v", td, DefaultTargetDuration)
	}
}

func TestConcurrent_observeAndBudget(t *testing.T) {
	// Race-detector test: many goroutines hammering Observe + Budget.
	tk := NewDefaultTracker()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				tk.Observe(int64(j+1)*1024, 10*time.Millisecond)
				_ = tk.Budget()
				_ = tk.BytesPerSec()
				_, _, _ = tk.Snapshot()
			}
		}()
	}
	wg.Wait()
}

func TestMaxLessThanMin_isClampedUp(t *testing.T) {
	// Constructing with maxBytes < minBytes is a misconfiguration;
	// the constructor must repair it (max := default; if still <
	// min, force max = min).
	tk := NewTracker(time.Second, 100*1024*1024, 1*1024*1024, 50*1024*1024)
	if tk.maxBytes < tk.minBytes {
		t.Errorf("maxBytes=%d < minBytes=%d after construction", tk.maxBytes, tk.minBytes)
	}
}
