// Copyright 2026 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package git

import (
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

// GitCallStats collects per-call-type timing information for git process invocations.
type GitCallStats struct {
	mu    sync.Mutex
	calls map[string][]time.Duration
}

var globalStats = &GitCallStats{
	calls: make(map[string][]time.Duration),
}

// DumpGlobalGitCallStats writes accumulated git call statistics to stderr.
// It is safe to call multiple times; it prints nothing if no calls were recorded.
// Intended to be deferred from main or called at process exit.
func DumpGlobalGitCallStats() {
	globalStats.DumpStats()
}

// Record adds a call duration for the given call type.
func (s *GitCallStats) Record(callType string, d time.Duration) {
	s.mu.Lock()
	s.calls[callType] = append(s.calls[callType], d)
	s.mu.Unlock()
}

// DumpStats writes per-call-type statistics to stderr.
func (s *GitCallStats) DumpStats() {
	s.mu.Lock()
	// Snapshot the data under lock so we can release quickly.
	snapshot := make(map[string][]time.Duration, len(s.calls))
	for k, v := range s.calls {
		snapshot[k] = append([]time.Duration(nil), v...)
	}
	s.mu.Unlock()

	if len(snapshot) == 0 {
		return
	}

	// Sort call types alphabetically for stable output.
	types := make([]string, 0, len(snapshot))
	for k := range snapshot {
		types = append(types, k)
	}
	sort.Strings(types)

	fmt.Fprintf(os.Stderr, "\n=== Git Process Call Stats ===\n")
	fmt.Fprintf(os.Stderr, "%-20s %8s %10s %10s %10s\n", "call-type", "count", "mean", "p10", "p90")

	total := 0
	for _, ct := range types {
		durations := snapshot[ct]
		n := len(durations)
		total += n

		sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })

		mean := avgDuration(durations)
		p10 := percentile(durations, 0.10)
		p90 := percentile(durations, 0.90)

		fmt.Fprintf(os.Stderr, "%-20s %8d %10s %10s %10s\n", ct, n, fmtDur(mean), fmtDur(p10), fmtDur(p90))
	}
	fmt.Fprintf(os.Stderr, "%-20s %8d\n", "TOTAL", total)
}

func avgDuration(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	var sum time.Duration
	for _, d := range ds {
		sum += d
	}
	return sum / time.Duration(len(ds))
}

// percentile returns the value at the given percentile (0–1) from a sorted slice.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)-1))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func fmtDur(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%.1fus", float64(d)/float64(time.Microsecond))
	case d < time.Second:
		return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
	default:
		return fmt.Sprintf("%.2fs", float64(d)/float64(time.Second))
	}
}
