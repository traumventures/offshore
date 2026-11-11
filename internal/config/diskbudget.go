package config

import (
	"fmt"
	"strconv"
	"strings"
)

// DefaultDiskBudget is the share of a machine's cache file system build
// slots and machine caches may fill before the worker prunes.
const DefaultDiskBudget = "90%"

// DiskBudget is how much of a machine's cache disk may be used before
// slots are pruned: a fraction of the file system or absolute bytes.
type DiskBudget struct {
	Fraction float64 // 0 < Fraction <= 1, or 0 when Bytes is set
	Bytes    int64
}

// ParseDiskBudget parses "90%" (of the file system) or a size ("150G").
func ParseDiskBudget(s string) (DiskBudget, error) { return parseShare("disk budget", s) }

// DefaultSourceCache is the share of the disk budget a worker keeps of
// source contents fetched from the laptop.
const DefaultSourceCache = "12.5%"

// ParseSourceCache parses [scheduler] source_cache: "12.5%" (of the disk
// budget) or a size ("20G"); Of then applies to the disk budget.
func ParseSourceCache(s string) (DiskBudget, error) { return parseShare("source cache", s) }

// parseShare parses a percentage or a size.
func parseShare(what, s string) (DiskBudget, error) {
	s = strings.TrimSpace(s)
	if p, ok := strings.CutSuffix(s, "%"); ok {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil || f <= 0 || f > 100 {
			return DiskBudget{}, fmt.Errorf("invalid %s %q (want 1%%..100%% or a size like 150G)", what, s)
		}
		return DiskBudget{Fraction: f / 100}, nil
	}
	n, err := ParseBytes(s)
	if err != nil || n <= 0 {
		return DiskBudget{}, fmt.Errorf("invalid %s %q (want 1%%..100%% or a size like 150G)", what, s)
	}
	return DiskBudget{Bytes: n}, nil
}

// Of returns the budget in bytes for a file system of total bytes.
func (b DiskBudget) Of(total int64) int64 {
	if b.Bytes > 0 {
		if total > 0 {
			return min(b.Bytes, total)
		}
		return b.Bytes
	}
	return int64(b.Fraction * float64(total))
}
