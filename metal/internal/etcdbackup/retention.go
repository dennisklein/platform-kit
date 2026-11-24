package etcdbackup

import (
	"cmp"
	"slices"
	"strings"
	"time"
)

// State is where one snapshot record stands, for retention.
type State int

const (
	// InProgress: not finished. Never pruned: the controller is writing it.
	InProgress State = iota
	// Verified: stored, and checked to be an intact etcd snapshot.
	Verified
	// Failed: finished without a verified snapshot (the stream, the sink
	// or the check failed).
	Failed
)

// Record is one snapshot as retention sees it.
type Record struct {
	Name  string
	Time  time.Time
	State State
}

// Retention is a policy's limits. Both apply.
type Retention struct {
	// Count keeps at most this many verified snapshots, the newest.
	Count int
	// MaxAge drops verified snapshots older than this; zero keeps any age.
	MaxAge time.Duration
	// KeepFailed keeps this many of the newest failed records for a person
	// to read why; older failures are dropped.
	KeepFailed int
}

// Prune returns the names of the records to delete, oldest first. The newest
// verified snapshot is never pruned, whatever its age: with every snapshot
// failing for a while, MaxAge alone would otherwise delete the last good one.
func Prune(records []Record, r Retention, now time.Time) []string {
	sorted := slices.Clone(records)
	// Newest first; the name breaks ties, so the answer is stable.
	slices.SortFunc(sorted, func(a, b Record) int {
		return cmp.Or(b.Time.Compare(a.Time), strings.Compare(b.Name, a.Name))
	})
	var drop []string
	verified, failed := 0, 0
	for _, rec := range sorted {
		switch rec.State {
		case Verified:
			verified++
			tooMany := r.Count > 0 && verified > r.Count
			tooOld := r.MaxAge > 0 && now.Sub(rec.Time) > r.MaxAge
			if verified > 1 && (tooMany || tooOld) {
				drop = append(drop, rec.Name)
			}
		case Failed:
			failed++
			if failed > r.KeepFailed {
				drop = append(drop, rec.Name)
			}
		}
	}
	slices.Reverse(drop)
	return drop
}
