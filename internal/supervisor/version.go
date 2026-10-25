package supervisor

// ChromiumVersion returns the Browser string Chromium reported when it last
// became ready, or "" before the first readiness. Unlike Detail it does not
// scan /proc, so it is cheap enough for state polling.
func (s *Supervisor) ChromiumVersion() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.chromiumVer
}
