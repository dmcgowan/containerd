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

package scheduler

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/containerd/containerd/gc"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/plugin"
	"github.com/pkg/errors"
)

// config configures the garbage collection policies.
type config struct {
	// PauseThreshold represents the maximum amount of time garbage
	// collection should be scheduled based on the average pause time.
	// For example, a value of 0.02 means that scheduled garbage collection
	// pauses should present at most 2% of real time,
	// or 20ms of every second.
	//
	// A maximum value of .5 is enforced to prevent over scheduling of the
	// garbage collector, trigger options are available to run in a more
	// predictable time frame after mutation.
	//
	// Default is 0.02
	PauseThreshold float64 `toml:"pause_threshold"`

	// DeletionThreshold is used to guarantee that a garbage collection is
	// scheduled after configured number of deletions have occurred
	// since the previous garbage collection. A value of 0 indicates that
	// garbage collection will not be triggered by deletion count.
	//
	// Default 0
	DeletionThreshold int `toml:"deletion_threshold"`

	// MutationThreshold is used to guarantee that a garbage collection is
	// run after a configured number of database mutations have occurred
	// since the previous garbage collection. A value of 0 indicates that
	// garbage collection will only be run after a manual trigger or
	// deletion. Unlike the deletion threshold, the mutation threshold does
	// not cause rescheduling of a garbage collection, but ensures GC is run
	// at the next scheduled GC.
	//
	// Default 100
	MutationThreshold int `toml:"mutation_threshold"`

	// ScheduleDelay is the duration in the future to schedule a garbage
	// collection triggered manually or by exceeding the configured
	// threshold for deletion or mutation. A zero value will immediately
	// schedule. Use suffix "ms" for millisecond and "s" for second.
	//
	// Default is "0ms"
	ScheduleDelay duration `toml:"schedule_delay"`

	// StartupDelay is the delay duration to do an initial garbage
	// collection after startup. The initial garbage collection is used to
	// set the base for pause threshold and should be scheduled in the
	// future to avoid slowing down other startup processes. Use suffix
	// "ms" for millisecond and "s" for second.
	//
	// Default is "100ms"
	StartupDelay duration `toml:"startup_delay"`

	// MaxDelay represents the maximum time to schedule garbage collection.
	// When garbage collection is not triggered manually or by mutations,
	// the schedule interval will increase to this delay. The max delay
	// should be seen as the maximum time delay between any mutation and
	// a scheduled garbage collection. Use the threshold configurations to
	// tune the frequency and rescheduling of garbage collection.
	// Use "s" for second.
	//
	// A minimum value of 10 seconds is enfored to prevent over scheduling
	// of the garbage collector.
	//
	// Default "300s"
	MaxDelay duration `toml:"max_interval"`
}

type duration time.Duration

func (d *duration) UnmarshalText(text []byte) error {
	ed, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	*d = duration(ed)
	return nil
}

func (d duration) MarshalText() (text []byte, err error) {
	return []byte(time.Duration(d).String()), nil
}

func init() {
	plugin.Register(&plugin.Registration{
		Type: plugin.GCPlugin,
		ID:   "scheduler",
		Requires: []plugin.Type{
			plugin.MetadataPlugin,
		},
		Config: &config{
			PauseThreshold:    0.02,
			DeletionThreshold: 0,
			MutationThreshold: 100,
			ScheduleDelay:     duration(0),
			StartupDelay:      duration(100 * time.Millisecond),
			MaxDelay:          duration(300 * time.Second),
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			md, err := ic.Get(plugin.MetadataPlugin)
			if err != nil {
				return nil, err
			}

			mdCollector, ok := md.(collector)
			if !ok {
				return nil, errors.Errorf("%s %T must implement collector", plugin.MetadataPlugin, md)
			}

			m := newScheduler(mdCollector, ic.Config.(*config))

			ic.Meta.Exports = map[string]string{
				"PauseThreshold":    fmt.Sprint(m.pauseThreshold),
				"DeletionThreshold": fmt.Sprint(m.deletionThreshold),
				"MutationThreshold": fmt.Sprint(m.mutationThreshold),
				"ScheduleDelay":     fmt.Sprint(m.scheduleDelay),
				"MaxDelay":          fmt.Sprint(m.maxDelay),
			}

			go m.run(ic.Context)

			return m, nil
		},
	})
}

type mutationEvent struct {
	ts       time.Time
	mutation bool
	dirty    bool
}

type collector interface {
	RegisterMutationCallback(func(bool))
	GarbageCollect(context.Context) (gc.Stats, error)
}

type gcScheduler struct {
	c collector

	eventC chan mutationEvent

	waiterL sync.Mutex
	waiters []chan gc.Stats

	pauseThreshold    float64
	deletionThreshold int
	mutationThreshold int
	scheduleDelay     time.Duration
	startupDelay      time.Duration
	maxDelay          time.Duration
}

func newScheduler(c collector, cfg *config) *gcScheduler {
	eventC := make(chan mutationEvent)

	s := &gcScheduler{
		c:                 c,
		eventC:            eventC,
		pauseThreshold:    cfg.PauseThreshold,
		deletionThreshold: cfg.DeletionThreshold,
		mutationThreshold: cfg.MutationThreshold,
		scheduleDelay:     time.Duration(cfg.ScheduleDelay),
		startupDelay:      time.Duration(cfg.StartupDelay),
		maxDelay:          time.Duration(cfg.MaxDelay),
	}

	if s.pauseThreshold < 0.0 {
		s.pauseThreshold = 0.0
	}
	if s.pauseThreshold > 0.5 {
		s.pauseThreshold = 0.5
	}
	if s.mutationThreshold < 0 {
		s.mutationThreshold = 0
	}
	if s.maxDelay < 10*time.Second {
		s.maxDelay = 10 * time.Second
	}
	if s.scheduleDelay < 0 {
		s.scheduleDelay = 0
	}
	if s.startupDelay < 0 {
		s.startupDelay = 0
	}

	c.RegisterMutationCallback(s.mutationCallback)

	return s
}

func (s *gcScheduler) ScheduleAndWait(ctx context.Context) (gc.Stats, error) {
	return s.wait(ctx, true)
}

func (s *gcScheduler) wait(ctx context.Context, trigger bool) (gc.Stats, error) {
	wc := make(chan gc.Stats, 1)
	s.waiterL.Lock()
	s.waiters = append(s.waiters, wc)
	s.waiterL.Unlock()

	if trigger {
		e := mutationEvent{
			ts: time.Now(),
		}
		go func() {
			s.eventC <- e
		}()
	}

	var gcStats gc.Stats
	select {
	case stats, ok := <-wc:
		if !ok {
			return gcStats, errors.New("gc failed")
		}
		gcStats = stats
	case <-ctx.Done():
		return gcStats, ctx.Err()
	}

	return gcStats, nil
}

func (s *gcScheduler) mutationCallback(dirty bool) {
	e := mutationEvent{
		ts:       time.Now(),
		mutation: true,
		dirty:    dirty,
	}
	go func() {
		s.eventC <- e
	}()
}

func schedule(d time.Duration) (<-chan time.Time, time.Time) {
	next := time.Now().Add(d)
	return time.After(d), next
}

func (s *gcScheduler) run(ctx context.Context) {
	var (
		schedC <-chan time.Time

		lastCollection time.Time
		nextCollection time.Time

		interval    = s.maxDelay
		gcTime      time.Duration
		collections int
		// TODO(dmcg): expose collection stats as metrics

		triggered = true // Always trigger on first event or interval

		deletions int
		mutations int
	)

	if s.startupDelay > 0 {
		interval = s.startupDelay
	}
	schedC, nextCollection = schedule(interval)
	for {
		select {
		case <-schedC:
			// Check if garbage collection can be skipped because
			// it is not needed or was not requested and reschedule
			// it to attempt again after another time interval.
			if !triggered && deletions == 0 &&
				(s.mutationThreshold == 0 || mutations < s.mutationThreshold) {
				// Increase interval to avoid checking while idle
				interval = interval * 2
				if interval > s.maxDelay {
					interval = s.maxDelay
				}
				schedC, nextCollection = schedule(interval)
				continue
			}
		case e := <-s.eventC:
			if lastCollection.After(e.ts) {
				continue
			}
			if e.dirty {
				deletions++
			}
			if e.mutation {
				mutations++
			} else {
				triggered = true
			}

			// Check whether should reschedule the garbage collection sooner
			// if condition should cause immediate collection
			//  and not already scheduled before delay threshold
			if (triggered || (s.deletionThreshold > 0 && deletions >= s.deletionThreshold)) &&
				nextCollection.After(time.Now().Add(s.scheduleDelay)) {
				// TODO(dmcg): track re-schedules for tuning schedule config
				schedC, nextCollection = schedule(s.scheduleDelay)
			}

			continue
		case <-ctx.Done():
			return
		}

		s.waiterL.Lock()

		stats, err := s.c.GarbageCollect(ctx)
		if err != nil {
			log.G(ctx).WithError(err).Error("garbage collection failed")

			interval = interval * 2
			if interval > s.maxDelay {
				interval = s.maxDelay
			}
			schedC, nextCollection = schedule(interval)

			for _, w := range s.waiters {
				close(w)
			}
			s.waiters = nil
			s.waiterL.Unlock()
			continue
		}

		log.G(ctx).WithField("d", stats.Elapsed()).Debug("garbage collected")

		gcTime += stats.Elapsed()
		collections++
		triggered = false
		deletions = 0
		mutations = 0

		// Calculate new interval with updated times
		if s.pauseThreshold > 0.0 {
			// Set interval to average gc time divided by the pause threshold
			// This algorithm ensures that a gc is scheduled to allow enough
			// runtime in between gc to reach the pause threshold.
			// Pause threshold is always 0.0 < threshold <= 0.5
			avg := float64(gcTime) / float64(collections)
			interval = time.Duration(avg/s.pauseThreshold - avg)
		} else {
			interval = s.maxDelay
		}

		lastCollection = time.Now()
		schedC, nextCollection = schedule(interval)

		for _, w := range s.waiters {
			w <- stats
		}
		s.waiters = nil
		s.waiterL.Unlock()
	}
}
