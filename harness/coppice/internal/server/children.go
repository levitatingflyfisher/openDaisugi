package server

import (
	"fmt"
	"sort"
	"strings"

	"github.com/opendaisugi/coppice/internal/pane"
	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/textwidth"
)

// childDoneKeep is how long a done subagent stays on its parent's row.
const childDoneKeep = 60.0

// childrenKept caps the subagents one pane keeps. The oldest go first.
const childrenKept = 32

// child is one subagent inside a pane's harness. It is read only: coppice
// cannot type into it or close it.
type child struct {
	ID    string
	Label string
	State string
	TS    float64
}

// childEvent is the wire shape of a child event.
type childEvent struct {
	Event string  `json:"event"`
	Pane  string  `json:"pane"`
	Child string  `json:"child"`
	State string  `json:"state"`
	Label string  `json:"label"`
	TS    float64 `json:"ts"`
}

// ReportChild records one subagent of pane paneID and sends a child
// event. A label left empty keeps the one the child had.
func (s *Server) ReportChild(paneID, id, state, label string) {
	now := nowSeconds()
	label = textwidth.Printable(label, noteMax)
	id = textwidth.Printable(id, noteMax)
	s.childMu.Lock()
	kids := s.children[paneID]
	if kids == nil {
		kids = map[string]*child{}
		s.children[paneID] = kids
	}
	c := kids[id]
	if c == nil {
		c = &child{ID: id}
		kids[id] = c
	}
	c.State, c.TS = state, now
	if label != "" {
		c.Label = label
	}
	if len(kids) > childrenKept {
		var oldest *child
		for _, k := range kids {
			if oldest == nil || k.TS < oldest.TS {
				oldest = k
			}
		}
		delete(kids, oldest.ID)
	}
	ev := childEvent{Event: "child", Pane: paneID, Child: id, State: state, Label: c.Label, TS: now}
	s.childMu.Unlock()
	s.Broadcast("child", paneID, ev)
}

// applyChildEvent records an adapter's child event.
func (s *Server) applyChildEvent(paneID string, ev pane.Event) {
	if ev.Child == "" {
		return
	}
	s.ReportChild(paneID, ev.Child, ev.State, ev.Text)
}

// childrenOf lists the subagents of one pane, oldest first. A done child
// older than childDoneKeep is dropped.
func (s *Server) childrenOf(paneID string) []map[string]any {
	now := nowSeconds()
	s.childMu.Lock()
	defer s.childMu.Unlock()
	kids := s.children[paneID]
	list := make([]*child, 0, len(kids))
	for id, k := range kids {
		if k.State == proto.StateDone && now-k.TS > childDoneKeep {
			delete(kids, id)
			continue
		}
		list = append(list, k)
	}
	sort.Slice(list, func(a, b int) bool {
		if list[a].TS != list[b].TS {
			return list[a].TS < list[b].TS
		}
		return list[a].ID < list[b].ID
	})
	out := make([]map[string]any, 0, len(list))
	for _, k := range list {
		out = append(out, map[string]any{"id": k.ID, "label": k.Label, "state": k.State, "ts": k.TS})
	}
	return out
}

// forgetChildren drops the subagents of a pane that closed.
func (s *Server) forgetChildren(paneID string) {
	s.childMu.Lock()
	delete(s.children, paneID)
	s.childMu.Unlock()
}

// handleReportChild is pane.report_child: a gate hook reports a subagent
// that started or stopped inside a pane's harness.
func (s *Server) handleReportChild(c *Client, r *proto.Request) proto.Response {
	id, ok := r.Str("pane")
	if !ok || id == "" {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, "pane.report_child needs pane. Run: coppice pane list")
	}
	if _, ok := s.tree.Pane(id); !ok {
		return proto.ErrResp(r.ID, proto.ErrNoSuchPane, fmt.Sprintf("no pane %q. Run: coppice pane list", id))
	}
	kid, _ := r.Str("child")
	kid = strings.TrimSpace(kid)
	if kid == "" {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, "pane.report_child needs child, the subagent id.")
	}
	state, _ := r.Str("state")
	if state != proto.StateWorking && state != proto.StateDone {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, "state must be working or done")
	}
	label, _ := r.Str("label")
	s.ReportChild(id, kid, state, label)
	return proto.OKResp(r.ID, map[string]any{"pane": id, "child": kid, "state": state})
}
