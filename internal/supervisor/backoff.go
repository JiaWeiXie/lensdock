package supervisor

import "time"

const (
	backoffMin    = time.Second
	backoffMax    = 30 * time.Second
	backoffStable = 60 * time.Second
)

// backoff produces restart delays that double from min up to max and reset
// once a process has run for at least stable.
type backoff struct {
	min, max, stable time.Duration
	now              func() time.Time

	next      time.Duration
	startedAt time.Time
}

func newBackoff(now func() time.Time) *backoff {
	return &backoff{min: backoffMin, max: backoffMax, stable: backoffStable, now: now, next: backoffMin}
}

// Started records that the process was (re)started.
func (b *backoff) Started() { b.startedAt = b.now() }

// Exited returns the delay to wait before the next start.
func (b *backoff) Exited() time.Duration {
	if !b.startedAt.IsZero() && b.now().Sub(b.startedAt) >= b.stable {
		b.next = b.min
	}
	d := b.next
	b.next = min(b.next*2, b.max)
	return d
}
