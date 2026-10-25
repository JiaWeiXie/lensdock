package capture

import (
	"sync"
	"time"
)

const (
	// DefaultImageTTL is how long a captured PNG stays retrievable.
	DefaultImageTTL = 120 * time.Second
	// DefaultMaxImages bounds the PNGs held in memory (each up to 32 MiB).
	DefaultMaxImages = 2
	// replayTTL is how long a capture's metadata answers a repeated
	// Idempotency-Key.
	replayTTL  = 5 * time.Minute
	maxReplays = 64
)

// Store holds captured PNGs in memory just long enough for the Gateway to
// fetch them, and the metadata that answers a repeated Idempotency-Key. The
// runtime keeps no private image on disk.
type Store struct {
	ttl       time.Duration
	maxImages int
	now       func() time.Time

	mu      sync.Mutex
	images  []*stored
	replays map[string]*replay
}

type stored struct {
	id      string
	png     []byte
	expires time.Time
}

type replay struct {
	fingerprint string
	result      Result
	expires     time.Time
}

// NewStore returns a Store. Non-positive arguments select the defaults; a nil
// now selects time.Now.
func NewStore(ttl time.Duration, maxImages int, now func() time.Time) *Store {
	if ttl <= 0 {
		ttl = DefaultImageTTL
	}
	if maxImages <= 0 {
		maxImages = DefaultMaxImages
	}
	if now == nil {
		now = time.Now
	}
	return &Store{ttl: ttl, maxImages: maxImages, now: now, replays: map[string]*replay{}}
}

func (s *Store) pruneLocked() {
	now := s.now()
	kept := s.images[:0]
	for _, im := range s.images {
		if now.Before(im.expires) {
			kept = append(kept, im)
		} else {
			im.png = nil
		}
	}
	for i := len(kept); i < len(s.images); i++ {
		s.images[i] = nil
	}
	s.images = kept
	for k, r := range s.replays {
		if !now.Before(r.expires) {
			delete(s.replays, k)
		}
	}
}

// Put stores the PNG of r (evicting the oldest beyond the limit) and, when
// idemKey is non-empty, remembers r for replay under fingerprint.
func (s *Store) Put(idemKey, fingerprint string, r Result, png []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	s.images = append(s.images, &stored{id: r.CaptureID, png: png, expires: s.now().Add(s.ttl)})
	for len(s.images) > s.maxImages {
		s.images[0].png = nil
		s.images = s.images[1:]
	}
	if idemKey != "" {
		if len(s.replays) >= maxReplays {
			var oldest string
			var oldestAt time.Time
			for k, rp := range s.replays {
				if oldest == "" || rp.expires.Before(oldestAt) {
					oldest, oldestAt = k, rp.expires
				}
			}
			delete(s.replays, oldest)
		}
		s.replays[idemKey] = &replay{fingerprint: fingerprint, result: r, expires: s.now().Add(replayTTL)}
	}
}

// Replay looks up a capture by Idempotency-Key. mismatch is true when the key
// was used with a different request fingerprint.
func (s *Store) Replay(idemKey, fingerprint string) (r Result, found, mismatch bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	rp, ok := s.replays[idemKey]
	if !ok {
		return Result{}, false, false
	}
	if rp.fingerprint != fingerprint {
		return Result{}, true, true
	}
	return rp.result, true, false
}

// Image returns the PNG of captureID while it is still retained.
func (s *Store) Image(captureID string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	for _, im := range s.images {
		if im.id == captureID {
			return im.png, true
		}
	}
	return nil, false
}

// Delete drops the PNG of captureID. It reports whether it existed.
func (s *Store) Delete(captureID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	for i, im := range s.images {
		if im.id == captureID {
			im.png = nil
			s.images = append(s.images[:i], s.images[i+1:]...)
			return true
		}
	}
	return false
}
