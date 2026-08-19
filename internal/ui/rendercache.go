package ui

// renderCache memoizes rebuild()'s two layers of repeated work: renders is the
// per-entry cache keyed by entry.seq (an entry never changes after stamp(), so a
// render cached under its seq is exact forever at a given width/maxLines), and
// joined is the whole-transcript string rebuild() last handed to the viewport.
// dirty says whether joined is still current; when it is, rebuild() does nothing
// at all — no re-join, no SetContent — which matters while active countdown and
// spinner ticks redraw surrounding chrome without changing the transcript.
//
// A map survives Model's every-Update copy safely: copying the struct copies the
// map header, not its contents, so every copy of Model shares one underlying
// entries map. width, maxLines, joined and dirty are plain value types with no
// internal self-pointer, exactly like the entries/seq fields already sitting on
// Model — nothing like strings.Builder or sync.Mutex here, so nothing to crash
// on the copy.
type renderCache struct {
	renders  map[int]string // entry.seq -> its rendered form, at width/maxLines
	width    int
	maxLines int
	joined   string // the last string handed to m.vp.SetContent
	dirty    bool

	// setContentCalls is a test seam: it only increments where rebuild() actually
	// calls m.vp.SetContent, so a test can prove a no-op rebuild() skipped it.
	setContentCalls int
}

// markDirty flags the transcript as changed. Every site that appends to or
// replaces m.entries must call this, or the next rebuild() will keep serving a
// stale joined string.
func (m *Model) markDirty() { m.rc.dirty = true }

// rebuild re-renders the transcript at the current width if anything changed.
// New output follows only while the viewport was already at the bottom. Once a
// user scrolls back, their offset is theirs until they explicitly return to the
// newest output; a tick or unrelated redraw must never steal it.
func (m *Model) rebuild() {
	if !m.ready {
		return
	}
	width := m.vp.Width()
	if width != m.rc.width || m.maxLines != m.rc.maxLines {
		// A resize invalidates every cached render: they were wrapped to the old
		// width. Simplicity over fine-grained eviction — a resize is rare, and
		// keying the cache per-width would hold onto renders for sizes the
		// terminal isn't at any more.
		m.rc.renders = make(map[int]string, len(m.entries))
		m.rc.width, m.rc.maxLines = width, m.maxLines
		m.rc.dirty = true
	} else if m.rc.renders == nil {
		m.rc.renders = make(map[int]string, len(m.entries))
	}
	if m.rc.dirty {
		follow := m.vp.AtBottom()
		if len(m.rc.renders) > len(m.entries) {
			// Bound memory after /clear or capReplay drop entries: without this the
			// per-entry cache only grows, holding renders for seqs that can never
			// appear again.
			m.rc.renders = pruneRenderCache(m.rc.renders, m.entries)
		}
		m.rc.joined = renderEntries(m.entries, width, m.maxLines, m.rc.renders)
		m.rc.dirty = false
		m.rc.setContentCalls++
		m.vp.SetContent(m.rc.joined)
		if follow {
			m.vp.GotoBottom()
		}
	}
}

// pruneRenderCache drops cached renders for seqs no longer present in entries.
func pruneRenderCache(cache map[int]string, entries []entry) map[int]string {
	fresh := make(map[int]string, len(entries))
	for _, e := range entries {
		if r, ok := cache[e.seq]; ok {
			fresh[e.seq] = r
		}
	}
	return fresh
}
