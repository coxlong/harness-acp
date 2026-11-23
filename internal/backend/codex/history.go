package codex

import (
	"context"
	"errors"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/ironpark/acp-go/schema/optional"
	schema "github.com/ironpark/acp-go/schema/v2"
)

func (b *Backend) History(ctx context.Context, id string) (backend.History, error) {
	p, err := b.connect(ctx)
	if err != nil {
		return backend.History{}, err
	}
	t, err := b.readThread(ctx, p, id, false)
	if err != nil {
		return backend.History{}, err
	}
	if t.ParentID != "" {
		return backend.History{}, &backend.SessionNotFoundError{ID: id}
	}
	var h backend.History
	if title := threadTitle(t); title != "" {
		h.Updates = append(h.Updates, backend.HistoryUpdate{
			Update: schema.NewSessionUpdate(schema.SessionUpdateSessionInfoUpdate{Title: optional.Of(title)}),
		})
	}
	r := historyReader{
		backend: b, process: p, root: p.find(id), history: &h,
		seen: map[string]bool{id: true}, missing: make(map[string]bool),
	}
	if err := r.replay(ctx, t, ""); err != nil {
		return backend.History{}, err
	}
	return h, nil
}

type historyReader struct {
	backend *Backend
	process *process
	root    *Session
	history *backend.History
	seen    map[string]bool
	missing map[string]bool
}

func (r *historyReader) replay(ctx context.Context, t thread, sid string) error {
	err := r.backend.readItems(ctx, r.process, t, func(i item) {
		if i.Cwd == "" {
			i.Cwd = t.Cwd
		}
		for _, ev := range i.events(true, false) {
			r.history.Updates = append(r.history.Updates, backend.HistoryUpdate{Session: sid, Update: ev.Update})
		}
		if i.SenderThreadID == "" || i.SenderThreadID == t.ID {
			for _, id := range i.childIDs() {
				r.includeChild(ctx, t.ID, sid, id)
			}
		}
	})
	if err != nil || r.root == nil {
		return err
	}
	// A child can start during an unpersisted parent turn. Read its current
	// association as well so later events on the child stream can be routed.
	r.root.mu.Lock()
	var children []string
	for _, id := range sortedIDs(r.root.children) {
		if c := r.root.children[id]; c.parent.id == t.ID && !c.closed {
			children = append(children, id)
		}
	}
	live := r.root
	if sid != "" {
		live = r.root.children[sid]
	}
	if live != nil && !live.closed {
		for _, id := range sortedIDs(live.terminals) {
			for _, ev := range live.terminals[id].events() {
				r.history.Updates = append(r.history.Updates, backend.HistoryUpdate{Session: sid, Update: ev.Update})
			}
		}
	}
	r.root.mu.Unlock()
	for _, id := range children {
		r.includeChild(ctx, t.ID, sid, id)
	}
	return nil
}

func (r *historyReader) includeChild(ctx context.Context, nativeParent, parent, id string) {
	if id == "" || r.seen[id] || ctx.Err() != nil {
		return
	}
	t, err := r.backend.readThread(ctx, r.process, id, false)
	if err != nil || t.ParentID != nativeParent {
		if !r.missing[id] {
			r.missing[id] = true
			r.history.Missing++
		}
		return
	}
	if r.missing[id] {
		delete(r.missing, id)
		r.history.Missing--
	}
	r.seen[id] = true
	child := schema.SessionUpdateSubagentUpdate{SessionID: schema.SessionID(id)}
	if title := childLabel(t); title != "" {
		child.Title = optional.Of(title)
	}
	if cwd := fullPath("", t.Cwd); cwd != "" {
		child.Cwd = optional.Of(cwd)
	}
	var end backend.TurnEvent
	if r.root != nil {
		r.root.mu.Lock()
		if live := r.root.children[id]; live != nil && !live.closed && live.parent.id == nativeParent {
			child, end = live.association(), live.last
			end.Background = live.background()
		}
		r.root.mu.Unlock()
	}
	r.history.Updates = append(r.history.Updates, backend.HistoryUpdate{Session: parent, Update: schema.NewSessionUpdate(child)})
	index := len(r.history.Children)
	r.history.Children = append(r.history.Children, backend.ReplayedChild{ID: id, Parent: parent, End: end})
	if err := r.replay(ctx, t, id); err != nil {
		r.history.Missing++
	}
	// Stored turns carry the final state even when the child is no longer
	// subscribed to the app-server.
	if end.State == backend.TurnIdle && end.StopReason == "" && end.Err == nil {
		var latest struct {
			Data []turn `json:"data"`
		}
		if r.process.call(ctx, "thread/turns/list", object{
			"threadId": id, "limit": 1, "sortDirection": "desc", "itemsView": "notLoaded",
		}, &latest) == nil && len(latest.Data) > 0 {
			r.history.Children[index].End = turnEnd(latest.Data[0])
		}
	}
}

func (b *Backend) readItems(ctx context.Context, p *process, t thread, appendItem func(item)) error {
	id := t.ID
	if t.HistoryMode != "paginated" {
		t, err := b.readThread(ctx, p, id, true)
		if err != nil {
			return err
		}
		for _, turn := range t.Turns {
			for _, i := range turn.Items {
				appendItem(i)
			}
		}
		return nil
	}

	// A thread can have an allocated rollout path before its first turn.
	var fresh bool
	if active := p.find(id); active != nil {
		active.mu.Lock()
		fresh = active.fresh
		active.mu.Unlock()
	}
	var cursor *string
	for {
		var page struct {
			Data []struct {
				Item item `json:"item"`
			} `json:"data"`
			NextCursor *string `json:"nextCursor"`
		}
		err := p.call(ctx, "thread/items/list", object{
			"threadId": id, "cursor": cursor, "limit": 100, "sortDirection": "asc",
		}, &page)
		if err != nil {
			var rpc *rpcError
			if fresh && cursor == nil && errors.As(err, &rpc) && rpc.Code == -32601 {
				return nil
			}
			return threadError(id, err)
		}
		for _, row := range page.Data {
			appendItem(row.Item)
		}
		if page.NextCursor == nil {
			return nil
		}
		if cursor != nil && *page.NextCursor == *cursor {
			return errors.New("Codex returned a repeated history cursor")
		}
		cursor = page.NextCursor
	}
}
