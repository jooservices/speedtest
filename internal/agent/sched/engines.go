package sched

import "time"

// SetEngines atomically replaces the independently timed schedules. Run still
// owns the single execution worker, including manual requests.
func (s *Scheduler) SetEngines(schedules []Schedule) {
	s.mu.Lock()
	s.engineEpoch = s.clock.Now()
	s.engines = append([]Schedule(nil), schedules...)
	s.mu.Unlock()
	s.signalWake()
}

func (s *Scheduler) nextSchedule(consumed map[string]time.Time) Schedule {
	s.currentAfterWake()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.engines) == 0 {
		return s.schedule
	}
	if s.paused {
		return Schedule{}
	}
	var chosen Schedule
	var earliest time.Time
	for _, sch := range s.engines {
		if !sch.Enabled {
			continue
		}
		next := engineSlot(sch, s.clock.Now(), consumed[sch.Engine], s.engineEpoch)
		if !next.IsZero() && (earliest.IsZero() || next.Before(earliest)) {
			chosen, earliest = sch, next
		}
	}
	return chosen
}

func (s *Scheduler) hasSchedule(sch Schedule) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.engines) == 0 {
		return s.schedule == sch
	}
	if s.paused {
		return false
	}
	for _, current := range s.engines {
		if current == sch {
			return true
		}
	}
	return false
}

func (s *Scheduler) engineSlot(sch Schedule, now, consumed time.Time) time.Time {
	s.mu.Lock()
	epoch := s.engineEpoch
	s.mu.Unlock()
	return engineSlot(sch, now, consumed, epoch)
}

func engineSlot(sch Schedule, now, consumed, epoch time.Time) time.Time {
	if sch.Engine == "" {
		return nextUnconsumed(sch, now, consumed)
	}
	if consumed.IsZero() {
		return sch.nextAllowed(epoch)
	}
	return sch.nextAllowed(consumed.Add(time.Nanosecond))
}
