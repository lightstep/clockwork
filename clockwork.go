package clockwork

import (
	"sync"
	"sync/atomic"
	"time"
)

// Clock provides an interface that packages can use instead of directly
// using the time module, so that chronology-related behavior can be tested
type Clock interface {
	After(d time.Duration) <-chan time.Time
	Sleep(d time.Duration)
	Now() time.Time
	Since(t time.Time) time.Duration

	NewTicker(d time.Duration) Ticker

	NewTimer(d time.Duration) Timer
	AfterFunc(d time.Duration, f func()) Timer
}

// FakeClock provides an interface for a clock which can be
// manually advanced through time
type FakeClock interface {
	Clock
	// Advance advances the FakeClock to a new point in time, ensuring any existing
	// sleepers are notified appropriately before returning
	Advance(d time.Duration)
	// BlockUntil will block until the FakeClock has the given number of
	// sleepers (callers of Sleep or After)
	BlockUntil(n int)
}

// NewRealClock returns a Clock which simply delegates calls to the actual time
// package; it should be used by packages in production.
func NewRealClock() Clock {
	return &realClock{}
}

// NewFakeClock returns a FakeClock implementation which can be
// manually advanced through time for testing. The initial time of the
// FakeClock will be an arbitrary non-zero time.
func NewFakeClock() FakeClock {
	// use a fixture that does not fulfill Time.IsZero()
	return NewFakeClockAt(time.Date(1984, time.April, 4, 0, 0, 0, 0, time.UTC))
}

// NewFakeClockAt returns a FakeClock initialised at the given time.Time.
func NewFakeClockAt(t time.Time) FakeClock {
	return &fakeClock{
		time: t,
	}
}

type realClock struct{}

func (rc *realClock) After(d time.Duration) <-chan time.Time {
	return time.After(d)
}

func (rc *realClock) Sleep(d time.Duration) {
	time.Sleep(d)
}

func (rc *realClock) Now() time.Time {
	return time.Now()
}

func (rc *realClock) Since(t time.Time) time.Duration {
	return rc.Now().Sub(t)
}

func (rc *realClock) NewTicker(d time.Duration) Ticker {
	return &realTicker{time.NewTicker(d)}
}

func (rc *realClock) NewTimer(d time.Duration) Timer {
	return &realTimer{time.NewTimer(d)}
}

func (rc *realClock) AfterFunc(d time.Duration, f func()) Timer {
	return &realTimer{time.AfterFunc(d, f)}
}

type fakeClock struct {
	sleepers []*sleeper
	blockers []*blocker
	time     time.Time

	l sync.RWMutex
}

// sleeper represents a waiting timer from NewTimer, Sleep, After, etc.
type sleeper struct {
	until    time.Time
	callback func(interface{}, time.Time)
	arg      interface{}

	c     chan time.Time
	done  uint32
	clock *fakeClock // needed for Reset()
}

// blocker represents a caller of BlockUntil
type blocker struct {
	count int
	ch    chan struct{}
}

func (s *sleeper) awaken(now time.Time) {
	if atomic.CompareAndSwapUint32(&s.done, 0, 1) {
		s.callback(s.arg, now)
	}
}

func (s *sleeper) Chan() <-chan time.Time { return s.c }

func (s *sleeper) Reset(d time.Duration) bool {
	active := s.Stop()
	s.until = s.clock.Now().Add(d)
	defer s.clock.addTimer(s)
	defer atomic.StoreUint32(&s.done, 0)
	return active
}

func (s *sleeper) Stop() bool {
	stopped := atomic.CompareAndSwapUint32(&s.done, 0, 1)
	if stopped {
		// Expire the timer and notify blockers
		s.until = s.clock.Now()
		s.clock.Advance(0)
	}
	return stopped
}

func (fc *fakeClock) addTimer(s *sleeper) {
	fc.l.Lock()
	defer fc.l.Unlock()

	now := fc.time
	if now.Sub(s.until) >= 0 {
		// special case - trigger immediately
		s.awaken(now)
	} else {
		// otherwise, add to the set of sleepers
		fc.sleepers = append(fc.sleepers, s)
		// and notify any blockers
		fc.blockers = notifyBlockers(fc.blockers, len(fc.sleepers))
	}
}

// After mimics time.After; it waits for the given duration to elapse on the
// fakeClock, then sends the current time on the returned channel.
func (fc *fakeClock) After(d time.Duration) <-chan time.Time {
	return fc.NewTimer(d).Chan()
}

// notifyBlockers notifies all the blockers waiting until the
// given number of sleepers are waiting on the fakeClock. It
// returns an updated slice of blockers (i.e. those still waiting)
func notifyBlockers(blockers []*blocker, count int) (newBlockers []*blocker) {
	for _, b := range blockers {
		if b.count == count {
			close(b.ch)
		} else {
			newBlockers = append(newBlockers, b)
		}
	}
	return
}

// Sleep blocks until the given duration has passed on the fakeClock
func (fc *fakeClock) Sleep(d time.Duration) {
	<-fc.After(d)
}

// Time returns the current time of the fakeClock
func (fc *fakeClock) Now() time.Time {
	fc.l.RLock()
	t := fc.time
	fc.l.RUnlock()
	return t
}

// Since returns the duration that has passed since the given time on the fakeClock
func (fc *fakeClock) Since(t time.Time) time.Duration {
	return fc.Now().Sub(t)
}

func (fc *fakeClock) NewTicker(d time.Duration) Ticker {
	ft := &fakeTicker{
		c:      make(chan time.Time, 1),
		stop:   make(chan bool, 1),
		clock:  fc,
		period: d,
	}
	go ft.tick()
	return ft
}

// NewTimer creates a new Timer that will send the current time on its channel
// after the given duration elapses on the fake clock.
func (fc *fakeClock) NewTimer(d time.Duration) Timer {
	done := make(chan time.Time, 1)
	sendTime := func(c interface{}, now time.Time) {
		c.(chan time.Time) <- now
	}

	s := &sleeper{
		clock:    fc,
		until:    fc.time.Add(d),
		callback: sendTime,
		arg:      done,
		c:        done,
	}
	fc.addTimer(s)
	return s
}

// AfterFunc waits for the duration to elapse on the fake clock and then calls f
// in its own goroutine.
// It returns a Timer that can be used to cancel the call using its Stop method.
func (fc *fakeClock) AfterFunc(d time.Duration, f func()) Timer {
	goFunc := func(fn interface{}, _ time.Time) {
		go fn.(func())()
	}

	s := &sleeper{
		clock:    fc,
		until:    fc.time.Add(d),
		callback: goFunc,
		arg:      f,
		// zero-valued c, the same as it is in the `time` pkg
	}
	fc.addTimer(s)
	return s
}

// Advance advances fakeClock to a new point in time, ensuring channels from any
// previous invocations of After are notified appropriately before returning
func (fc *fakeClock) Advance(d time.Duration) {
	fc.l.Lock()
	defer fc.l.Unlock()

	end := fc.time.Add(d)
	var newSleepers []*sleeper
	for _, s := range fc.sleepers {
		if end.Sub(s.until) >= 0 {
			s.awaken(end)
		} else {
			newSleepers = append(newSleepers, s)
		}
	}
	fc.sleepers = newSleepers
	fc.blockers = notifyBlockers(fc.blockers, len(fc.sleepers))
	fc.time = end
}

// BlockUntil will block until the fakeClock has the given number of sleepers
// (callers of Sleep or After)
func (fc *fakeClock) BlockUntil(n int) {
	fc.l.Lock()
	// Fast path: current number of sleepers is what we're looking for
	if len(fc.sleepers) == n {
		fc.l.Unlock()
		return
	}
	// Otherwise, set up a new blocker
	b := &blocker{
		count: n,
		ch:    make(chan struct{}),
	}
	fc.blockers = append(fc.blockers, b)
	fc.l.Unlock()
	<-b.ch
}
