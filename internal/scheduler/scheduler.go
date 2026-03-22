package scheduler

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

type EntryID int64

type entry struct {
	id   EntryID
	spec scheduleSpec
	job  func()
}

type Scheduler struct {
	mu      sync.Mutex
	nextID  EntryID
	entries map[EntryID]entry
	cancels map[EntryID]context.CancelFunc
	wg      sync.WaitGroup
	ctx     context.Context
	cancel  context.CancelFunc
	started bool
}

func New() *Scheduler {
	return &Scheduler{
		entries: make(map[EntryID]entry),
		cancels: make(map[EntryID]context.CancelFunc),
	}
}

func (s *Scheduler) AddFunc(expr string, job func()) (EntryID, error) {
	spec, err := parse(expr)
	if err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextID++
	id := s.nextID
	s.entries[id] = entry{id: id, spec: spec, job: job}

	if s.started {
		s.startEntry(id, s.entries[id])
	}

	return id, nil
}

func (s *Scheduler) Remove(id EntryID) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.entries, id)
	if cancel, ok := s.cancels[id]; ok {
		cancel()
		delete(s.cancels, id)
	}
}

func (s *Scheduler) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return
	}

	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.started = true

	for id, e := range s.entries {
		s.startEntry(id, e)
	}
}

func (s *Scheduler) Stop() context.Context {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		doneCtx, cancel := context.WithCancel(context.Background())
		cancel()
		return doneCtx
	}

	cancel := s.cancel
	s.started = false
	s.cancel = nil
	s.ctx = nil
	s.cancels = make(map[EntryID]context.CancelFunc)
	s.mu.Unlock()

	cancel()

	doneCtx, doneCancel := context.WithCancel(context.Background())
	go func() {
		s.wg.Wait()
		doneCancel()
	}()

	return doneCtx
}

func (s *Scheduler) startEntry(id EntryID, e entry) {
	entryCtx, entryCancel := context.WithCancel(s.ctx)
	s.cancels[id] = entryCancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.cancels, id)
			s.mu.Unlock()
		}()

		for {
			next, ok := e.spec.Next(time.Now())
			if !ok {
				return
			}

			timer := time.NewTimer(time.Until(next))
			select {
			case <-entryCtx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return
			case <-timer.C:
				e.job()
			}
		}
	}()
}

type scheduleSpec struct {
	seconds  fieldMatcher
	minutes  fieldMatcher
	hours    fieldMatcher
	days     fieldMatcher
	months   fieldMatcher
	weekdays fieldMatcher
}

func (s scheduleSpec) Next(after time.Time) (time.Time, bool) {
	candidate := after.Truncate(time.Second).Add(time.Second)
	limit := candidate.AddDate(1, 0, 0)

	for !candidate.After(limit) {
		if s.matches(candidate) {
			return candidate, true
		}
		candidate = candidate.Add(time.Second)
	}

	return time.Time{}, false
}

func (s scheduleSpec) matches(t time.Time) bool {
	return s.seconds.Match(t.Second()) &&
		s.minutes.Match(t.Minute()) &&
		s.hours.Match(t.Hour()) &&
		s.days.Match(t.Day()) &&
		s.months.Match(int(t.Month())) &&
		s.weekdays.Match(int(t.Weekday()))
}

type fieldMatcher struct {
	any    bool
	values map[int]struct{}
}

func (m fieldMatcher) Match(v int) bool {
	if m.any {
		return true
	}
	_, ok := m.values[v]
	return ok
}

func parse(expr string) (scheduleSpec, error) {
	parts := strings.Fields(expr)
	switch len(parts) {
	case 5:
		parts = append([]string{"0"}, parts...)
	case 6:
	default:
		return scheduleSpec{}, fmt.Errorf("cron expression must contain 5 or 6 fields, got %d", len(parts))
	}

	seconds, err := parseField(parts[0], 0, 59)
	if err != nil {
		return scheduleSpec{}, fmt.Errorf("parse seconds: %w", err)
	}
	minutes, err := parseField(parts[1], 0, 59)
	if err != nil {
		return scheduleSpec{}, fmt.Errorf("parse minutes: %w", err)
	}
	hours, err := parseField(parts[2], 0, 23)
	if err != nil {
		return scheduleSpec{}, fmt.Errorf("parse hours: %w", err)
	}
	days, err := parseField(parts[3], 1, 31)
	if err != nil {
		return scheduleSpec{}, fmt.Errorf("parse day-of-month: %w", err)
	}
	months, err := parseField(parts[4], 1, 12)
	if err != nil {
		return scheduleSpec{}, fmt.Errorf("parse month: %w", err)
	}
	weekdays, err := parseField(parts[5], 0, 6)
	if err != nil {
		return scheduleSpec{}, fmt.Errorf("parse day-of-week: %w", err)
	}

	return scheduleSpec{
		seconds:  seconds,
		minutes:  minutes,
		hours:    hours,
		days:     days,
		months:   months,
		weekdays: weekdays,
	}, nil
}

func parseField(raw string, min int, max int) (fieldMatcher, error) {
	if raw == "*" {
		return fieldMatcher{any: true}, nil
	}

	values := make(map[int]struct{})
	for _, segment := range strings.Split(raw, ",") {
		switch {
		case strings.HasPrefix(segment, "*/"):
			step, err := strconv.Atoi(strings.TrimPrefix(segment, "*/"))
			if err != nil || step <= 0 {
				return fieldMatcher{}, fmt.Errorf("invalid step %q", segment)
			}
			for v := min; v <= max; v += step {
				values[v] = struct{}{}
			}
		case strings.Contains(segment, "-"):
			bounds := strings.SplitN(segment, "-", 2)
			if len(bounds) != 2 {
				return fieldMatcher{}, fmt.Errorf("invalid range %q", segment)
			}
			start, err := strconv.Atoi(bounds[0])
			if err != nil {
				return fieldMatcher{}, fmt.Errorf("invalid range start %q", bounds[0])
			}
			end, err := strconv.Atoi(bounds[1])
			if err != nil {
				return fieldMatcher{}, fmt.Errorf("invalid range end %q", bounds[1])
			}
			if start > end || start < min || end > max {
				return fieldMatcher{}, fmt.Errorf("range %q out of bounds", segment)
			}
			for v := start; v <= end; v++ {
				values[v] = struct{}{}
			}
		default:
			value, err := strconv.Atoi(segment)
			if err != nil {
				return fieldMatcher{}, fmt.Errorf("invalid value %q", segment)
			}
			if value < min || value > max {
				return fieldMatcher{}, fmt.Errorf("value %d out of bounds [%d,%d]", value, min, max)
			}
			values[value] = struct{}{}
		}
	}

	if len(values) == 0 {
		return fieldMatcher{}, fmt.Errorf("empty field %q", raw)
	}

	return fieldMatcher{values: values}, nil
}
