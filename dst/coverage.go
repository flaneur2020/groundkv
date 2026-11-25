package dst

import (
	"fmt"
	"sort"
	"strings"
)

// Antithesis/TigerBeetle-style "sometimes" scenario coverage events.
// Every critical storage-engine mechanism and fault path should be hit across
// a DST swarm; if any event drops to 0 hits, DST fails so coverage never rots.
const (
	EventLeafSplit               = "leaf_split"
	EventBranchSplit             = "branch_split"
	EventRootCollapse            = "root_collapse"
	EventEmptyTree               = "empty_tree"
	EventMultiPageFreelist       = "multi_page_freelist"
	EventFreelistReclaim         = "freelist_reclaim"
	EventReaderBlockedReclaim    = "reader_blocked_reclaim"
	EventSameTxDirtyOverwrite    = "same_tx_dirty_overwrite"
	EventTxRollbackRecycled      = "tx_rollback_recycled"
	EventCrashAfterDataWrite     = "crash_after_data_write"
	EventCrashAfterDataSync      = "crash_after_data_sync"
	EventCrashAfterMetaWritePre  = "crash_after_meta_write_pre_commit"
	EventCrashAfterMetaWritePost = "crash_after_meta_write_post_commit"
	EventCrashTornMetaFallback   = "crash_torn_meta_fallback"
	EventIOFailWrite             = "io_fail_write"
	EventIOFailSync              = "io_fail_sync"
)

// AllRequiredEvents enumerates every "sometimes" scenario that a full DST swarm
// must exercise at least once.
var AllRequiredEvents = []string{
	EventLeafSplit,
	EventBranchSplit,
	EventRootCollapse,
	EventEmptyTree,
	EventMultiPageFreelist,
	EventFreelistReclaim,
	EventReaderBlockedReclaim,
	EventSameTxDirtyOverwrite,
	EventTxRollbackRecycled,
	EventCrashAfterDataWrite,
	EventCrashAfterDataSync,
	EventCrashAfterMetaWritePre,
	EventCrashAfterMetaWritePost,
	EventCrashTornMetaFallback,
	EventIOFailWrite,
	EventIOFailSync,
}

// CoverageTracker counts hits of critical DST scenarios.
type CoverageTracker struct {
	Counts map[string]int
}

func NewCoverageTracker() *CoverageTracker {
	return &CoverageTracker{Counts: make(map[string]int)}
}

func (c *CoverageTracker) Hit(event string) {
	c.Counts[event]++
}

func (c *CoverageTracker) Merge(other *CoverageTracker) {
	if other == nil {
		return
	}
	for k, v := range other.Counts {
		c.Counts[k] += v
	}
}

func (c *CoverageTracker) Missing(required []string) []string {
	var missing []string
	for _, ev := range required {
		if c.Counts[ev] == 0 {
			missing = append(missing, ev)
		}
	}
	sort.Strings(missing)
	return missing
}

func (c *CoverageTracker) Summary(required []string) string {
	var b strings.Builder
	hit := 0
	for _, ev := range required {
		n := c.Counts[ev]
		if n > 0 {
			hit++
		}
		fmt.Fprintf(&b, "  %-36s %d\n", ev, n)
	}
	return fmt.Sprintf("DST Scenario Coverage: %d/%d required events hit\n%s", hit, len(required), b.String())
}
