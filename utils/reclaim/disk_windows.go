//go:build windows

package reclaim

// freeDiskBytes has no Windows implementation — the runner ships Linux (and
// Darwin) binaries, and reporting a wrong number would be worse than
// reporting none. Callers treat ok=false as "say nothing about disk".
func freeDiskBytes(string) (uint64, bool) {
	return 0, false
}
