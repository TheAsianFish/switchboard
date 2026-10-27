package ratelimit

// MemoryWindows reports how many windows m holds, for the test that its
// memory stays bounded.
func MemoryWindows(m *MemoryFixedWindow) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.windows)
}
