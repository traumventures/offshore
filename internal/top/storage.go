package top

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/traumventures/offshore/internal/events"
	"github.com/traumventures/offshore/internal/proto"
)

// storageRow is one row of the storage panel: a machine header or one
// item of a machine's disk breakdown.
type storageRow struct {
	Machine string
	Header  bool // machine summary row
	Disk    *proto.DiskStatus
	Item    proto.StorageItem
}

// ID identifies the row for the detail view.
func (r storageRow) ID() string {
	if r.Header {
		return r.Machine
	}
	return r.Machine + "/" + r.Item.Kind + "/" + r.Item.Name
}

var kindOrder = map[string]int{proto.StorageSlot: 0, proto.StoragePages: 1, proto.StorageFiles: 2, proto.StorageSource: 3,
	proto.StorageMachineCache: 4, proto.StorageGlobal: 5, proto.StorageOther: 6}

// storageRows groups the disk breakdown by machine (running machines with
// a measured disk): a header row, then slots (with their page tiers next
// to them), files, sources, caches and the rest.
func storageRows(s Snapshot) []storageRow {
	var out []storageRow
	for _, m := range s.Up() {
		if !running(m) || m.Status == nil || m.Status.Disk == nil {
			continue
		}
		d := m.Status.Disk
		out = append(out, storageRow{Machine: m.ID, Header: true, Disk: d})
		items := append([]proto.StorageItem(nil), d.Items...)
		sort.SliceStable(items, func(i, j int) bool {
			a, b := items[i], items[j]
			ra, rb := a.Repo, b.Repo
			if ra == "" {
				ra = "~"
			}
			if rb == "" {
				rb = "~"
			}
			if ka, kb := kindOrder[a.Kind] >= 2, kindOrder[b.Kind] >= 2; ka != kb {
				return !ka
			}
			if ra != rb {
				return ra < rb
			}
			if kindOrder[a.Kind] != kindOrder[b.Kind] {
				return kindOrder[a.Kind] < kindOrder[b.Kind]
			}
			if a.Bytes != b.Bytes {
				return a.Bytes > b.Bytes
			}
			return a.Name < b.Name
		})
		for _, it := range items {
			out = append(out, storageRow{Machine: m.ID, Item: it})
		}
	}
	return out
}

// itemName renders what an item is: a slot's worktree and branch, a page
// tier's repository, a cache's name.
func itemName(it proto.StorageItem) string {
	switch it.Kind {
	case proto.StorageSlot:
		n := shortRepo(it.Repo)
		if it.Worktree != "" {
			n = shortPath(it.Worktree)
		}
		if it.Branch != "" {
			n += " (" + it.Branch + ")"
		}
		return n
	case proto.StoragePages:
		return "pages of " + shortRepo(it.Repo)
	}
	return it.Name
}

func shortRepo(r string) string {
	if len(r) > 12 {
		return r[:12]
	}
	return r
}

var storageCols = []Col{
	{Title: "MACHINE"}, {Title: "KIND"}, {Title: "NAME", Flex: true, Min: 16}, {Title: "SIZE", Right: true}, {Title: "RESIDENT", Right: true, Drop: 3},
	{Title: "STORED", Right: true, Drop: 4}, {Title: "DIRTY", Right: true, Drop: 2}, {Title: "LAST USED", Right: true, Drop: 5},
	{Title: "COMMITTED", Right: true, Drop: 6}, {Title: "LAG", Right: true, Drop: 1},
}

func ago(now time.Time, unix int64) string {
	if unix <= 0 {
		return "-"
	}
	return short(now.Sub(time.Unix(unix, 0))) + " ago"
}

func hum0(b int64) string {
	if b <= 0 {
		return "-"
	}
	return human(b)
}

// storeLag is how far the store head lags behind the slot's last commit
// ("-": in sync or unknown, "not flushed": no head in the store).
func storeLag(it proto.StorageItem) string {
	switch {
	case it.Kind != proto.StorageSlot || it.Committed == 0:
		return "-"
	case it.StoreAt == 0:
		return "not flushed"
	case it.Committed > it.StoreAt:
		return short(time.Duration(it.Committed-it.StoreAt) * time.Second)
	}
	return "-"
}

func storageCells(r storageRow, now time.Time) []Cell {
	if r.Header {
		d := r.Disk
		return []Cell{C(r.Machine), C("disk"), C(fmt.Sprintf("budget %s", human(d.Budget))), C(human(d.Used) + "/" + human(d.Total)), C(""), C(""), C(""), C(""), C(""), C("")}
	}
	it := r.Item
	name := itemName(it)
	if it.Leased {
		name += " *"
	}
	return []Cell{C(""), C(it.Kind), C(name), C(hum0(it.Bytes)), C(hum0(it.Resident)), C(hum0(it.Stored)), C(hum0(it.Dirty)),
		C(ago(now, it.LastUsed)), C(ago(now, it.Committed)), C(storeLag(it))}
}

func storagePanel(s Snapshot, h *History, rows []storageRow, v View, st Styles, n int) string {
	inner := v.Width - 2
	cells := make([][]Cell, len(rows))
	stored := map[string]int64{}
	for i, r := range rows {
		cells[i] = storageCells(r, s.Time)
		if !r.Header && r.Item.Stored > stored[r.Item.Repo] {
			stored[r.Item.Repo] = r.Item.Stored
		}
	}
	var total int64
	for _, b := range stored {
		total += b
	}
	var pin, wb float64
	for _, m := range s.Up() {
		if ps := h.Machine[m.ID]; running(m) && len(ps) > 0 {
			pin += ps[len(ps)-1].PageIn
			wb += ps[len(ps)-1].Writeback
		}
	}
	ws := layoutCols(storageCols, cells, inner)
	lines := []string{st.Header.Render(fit(renderHeader(storageCols, ws, nil), inner))}
	limit := n
	if v.Height <= 0 {
		limit = len(rows)
	}
	sel := min(max(v.SSelected, 0), max(len(rows)-1, 0))
	off := window(sel, v.SOffset, limit, len(rows))
	for i := off; i < len(rows) && i < off+limit; i++ {
		line := renderRow(storageCols, ws, cells[i], st)
		if rows[i].Header {
			line = st.Title.Render(line)
		}
		line = fit(line, inner)
		if i == sel && v.Focus == FocusStorage && v.Height > 0 {
			line = st.Selected.Render(pad(line, inner))
		}
		lines = append(lines, line)
	}
	title := fmt.Sprintf("Storage  store %s in %d repos  page-in %s/s  writeback %s/s  (* leased)", human(total), len(stored), human(int64(pin)), human(int64(wb)))
	if len(rows) > limit {
		title += fmt.Sprintf("  %d rows", len(rows))
	}
	pad := 0
	if v.Height > 0 {
		pad = limit + 1
	}
	return panel(st.Title.Render(fit(title, inner)), lines, pad, inner, st)
}

// StorageEvent is a storage event of a repository on a machine (commit,
// restore, writeback, page-in, prune, reclaim).
type StorageEvent struct {
	T       time.Time
	Type    string
	Machine string
	Repo    string
	Bytes   int64
	Seconds float64
	Reason  string
}

// storageEvent records storage events (History.Events).
func (h *History) storageEvent(ev events.Event) {
	switch ev.Type {
	case events.SnapshotSave, events.SnapshotRestore, events.SlotAction, events.Writeback, events.PageIn, events.Reclaim, events.Thrash:
	default:
		return
	}
	se := StorageEvent{T: ev.Time, Type: ev.Type, Machine: ev.Machine, Repo: ev.Repo, Bytes: ev.Bytes, Seconds: ev.Seconds, Reason: ev.Reason}
	for i := len(h.Storage) - 1; i >= 0 && !h.Storage[i].T.Before(se.T); i-- {
		if h.Storage[i] == se {
			return // polls overlap
		}
	}
	h.Storage = append(h.Storage, se)
	if n := len(h.Storage); n > 2000 {
		h.Storage = append([]StorageEvent(nil), h.Storage[n-2000:]...)
	}
}

// storageDetail renders the detail view of a storage row.
func storageDetail(s Snapshot, h *History, v View, st Styles, id string) string {
	inner := v.Width - 2
	var row *storageRow
	for _, r := range storageRows(s) {
		if r.ID() == id {
			r := r
			row = &r
		}
	}
	if row == nil {
		return panel(st.Title.Render("Storage "+id), []string{st.Dim.Render("item gone")}, 0, inner, st)
	}
	var b strings.Builder
	if row.Header {
		d := row.Disk
		pairs := [][2]string{{"disk", fmt.Sprintf("%s used of %s (budget %s)", human(d.Used), human(d.Total), human(d.Budget))},
			{"slots", human(d.Slots)}, {"machine caches", human(d.MachineCaches)}}
		byKind := map[string]int64{}
		for _, it := range d.Items {
			byKind[it.Kind] += it.Bytes
		}
		for _, k := range []string{proto.StorageSlot, proto.StoragePages, proto.StorageFiles, proto.StorageSource, proto.StorageMachineCache, proto.StorageGlobal, proto.StorageOther} {
			if byKind[k] > 0 {
				pairs = append(pairs, [2]string{k, human(byKind[k])})
			}
		}
		b.WriteString(panel(st.Title.Render(fit("Storage of "+row.Machine, inner)), kv(pairs, inner), 0, inner, st))
		return b.String()
	}
	it := row.Item
	pairs := [][2]string{{"kind", it.Kind}, {"name", it.Name}, {"machine", row.Machine}}
	if it.Repo != "" {
		pairs = append(pairs, [2]string{"repository", it.Repo})
	}
	if it.Worktree != "" || it.Branch != "" {
		pairs = append(pairs, [2]string{"last session", dashS(shortPath(it.Worktree)) + " (" + dashS(it.Branch) + ")"})
	}
	pairs = append(pairs, [2]string{"size", hum0(it.Bytes)}, [2]string{"resident", hum0(it.Resident)}, [2]string{"dirty", hum0(it.Dirty)},
		[2]string{"in store", hum0(it.Stored)}, [2]string{"last used", ago(s.Time, it.LastUsed)}, [2]string{"last commit", ago(s.Time, it.Committed)},
		[2]string{"store head", ago(s.Time, it.StoreAt)}, [2]string{"store lag", storeLag(it)})
	if it.Leased {
		pairs = append(pairs, [2]string{"leased", "yes (a session runs on it)"})
	}
	// The page tier of the repository carries the head breakdown.
	pages := it
	if it.Kind == proto.StorageSlot {
		if m, ok := findMachine(s, row.Machine); ok && m.Status != nil && m.Status.Disk != nil {
			for _, x := range m.Status.Disk.Items {
				if x.Kind == proto.StoragePages && x.Repo == it.Repo {
					pages = x
				}
			}
		}
	}
	if pages.WorkingSet > 0 {
		pairs = append(pairs, [2]string{"working set", human(pages.WorkingSet)})
	}
	b.WriteString(panel(st.Title.Render(fit("Storage "+itemName(it), inner)), kv(pairs, inner), 0, inner, st))
	if len(pages.Classes) > 0 {
		var lines []string
		var total int64
		for _, n := range pages.Classes {
			total += n
		}
		for _, c := range []string{"immutable", "write-once", "churn", "ephemeral", "unknown"} {
			if n := pages.Classes[c]; n > 0 {
				lines = append(lines, renderRow([]Col{{Title: "c"}, {Title: "b", Bar: true}}, []int{12, 22}, []Cell{C(c), B(float64(n), float64(total), human(n))}, st))
			}
		}
		b.WriteString(panel(st.Title.Render("Committed head by access class"), lines, 0, inner, st))
	}
	if len(pages.TopDirs) > 0 {
		var lines []string
		for _, d := range pages.TopDirs {
			lines = append(lines, fit(fmt.Sprintf("%9s  %s", human(d.Bytes), d.Path), inner))
		}
		b.WriteString(panel(st.Title.Render("Largest directories"), lines, 0, inner, st))
	}
	var hist []string
	for i := len(h.Storage) - 1; i >= 0 && len(hist) < max(5, v.Height-len(pairs)-20); i-- {
		e := h.Storage[i]
		if e.Machine != row.Machine || (it.Repo != "" && e.Repo != "" && e.Repo != it.Repo) || (it.Repo != "" && e.Repo == "" && e.Type != events.PageIn && e.Type != events.Reclaim) {
			continue
		}
		line := fmt.Sprintf("%s  %-16s %9s", e.T.Local().Format("15:04:05"), e.Type, hum0(e.Bytes))
		if e.Seconds > 0 {
			line += fmt.Sprintf("  %.1fs", e.Seconds)
		}
		if e.Reason != "" {
			line += "  " + e.Reason
		}
		hist = append(hist, fit(line, inner))
	}
	if len(hist) == 0 {
		hist = []string{st.Dim.Render("no storage events in this session of top")}
	}
	b.WriteString(panel(st.Title.Render("History (commits, restores, writebacks, page-ins, prunes, reclaims)"), hist, 0, inner, st))
	return b.String()
}
