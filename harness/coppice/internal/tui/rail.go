package tui

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/opendaisugi/coppice/internal/attach"
	"github.com/opendaisugi/coppice/internal/textwidth"
)

// railHint is what the floor says when a key that means nothing on the
// rail is pressed, so the way to type a line is always one message away.
const railHint = "That key does nothing on the rail. ctrl-t types a line."

// handle applies one keystroke while no window has the keys. quit is true
// when the floor should close. ctrl-c quits. A question takes the key
// first, then the picker, the rename field, the prompt line and the tree,
// whichever is open. A mouse release or motion never answers a question.
// Then the rail keys in Table act through Bind, arrows
// move, and a click lands. Other keys only say how to type a line.
func (f *floor) handle(k key) (quit bool, err error) {
	m := f.m
	if k.kind == keyByte && k.b == 0x03 {
		return true, nil
	}
	switch {
	case m.Confirm != "" || m.ClearAll:
		// A terminal sends a release after the press that asked, and
		// motion is not an answer either. Only a key or a new press is.
		if k.kind == keyMouse && (k.click.Release || k.click.Button&32 != 0) {
			return false, nil
		}
		return false, f.answerConfirm(k)
	case m.Picker != nil:
		return false, f.pickerKey(k)
	case m.Renaming != "":
		return false, f.renameKey(k)
	case m.Swapping != "":
		return false, f.swapTypeKey(k)
	case m.Talking:
		return false, f.talkKey(k)
	case m.Tree != nil:
		f.treeKey(k)
		return false, nil
	case m.Journal != nil:
		f.journalKey(k)
		return false, nil
	}
	if k.kind == keyByte && m.Peek != nil && m.Peek.Trust {
		switch k.b {
		case 'y', 'n':
			return false, f.answerTrust(k.b == 'y')
		}
	}
	if k.kind == keyByte && m.Peek.Answerable() {
		switch k.b {
		case 'y', 't', 'n':
			return false, f.answerPeek(k.b)
		}
	}
	switch Bind(k) {
	case Talk:
		m.Talking = true
		return false, nil
	case NextNeed:
		m.NextNeed()
		return false, nil
	case Stop:
		return false, f.stopKey()
	case Peek:
		return false, f.space()
	case GoIn:
		return false, f.enter()
	case Up:
		f.up()
		return false, nil
	case New:
		return false, f.newNear()
	case NewIn:
		return false, f.openPicker()
	case Rename:
		f.startRename()
		return false, nil
	case Journal:
		f.openJournal()
		return false, nil
	case Mode:
		return false, f.modeKey()
	case Swap:
		f.startSwap()
		return false, nil
	case Window:
		return false, f.toWindow(int(k.b - '0'))
	}
	switch k.kind {
	case keyUp:
		m.Move(-1)
	case keyDown:
		m.Move(1)
	case keyMouse:
		return false, f.click(k.click)
	case keyByte:
		if k.b >= 0x20 && k.b != 0x7f {
			m.Message = railHint
		}
	}
	return false, nil
}

// editLine applies one key to a line being typed: backspace drops the
// last rune, and a printable byte is added. It reports whether it used
// the key.
func editLine(line *string, k key) bool {
	if k.kind != keyByte {
		return false
	}
	switch {
	case k.b == 0x7f || k.b == 0x08:
		if *line != "" {
			_, size := utf8.DecodeLastRuneInString(*line)
			*line = (*line)[:len(*line)-size]
		}
		return true
	case k.b >= 0x20:
		*line += string([]byte{k.b})
		return true
	}
	return false
}

// talkKey applies one key while the prompt line has the keys. Enter runs
// the line and Esc lets it go. Every printable key is text, Space too.
// The arrows still move the rail cursor, and a click still lands.
func (f *floor) talkKey(k key) error {
	m := f.m
	switch {
	case k.kind == keyByte && (k.b == '\r' || k.b == '\n'):
		return f.enter()
	case k.kind == keyEsc:
		f.up()
		return nil
	case editLine(&m.Prompt, k):
		return nil
	case k.kind == keyUp:
		m.Move(-1)
	case k.kind == keyDown:
		m.Move(1)
	case k.kind == keyMouse:
		return f.click(k.click)
	}
	return nil
}

// startRename is r: the prompt line becomes the label of the live row
// under the cursor, to edit. A subagent row cannot be renamed.
func (f *floor) startRename() {
	m := f.m
	r, ok := m.Selected()
	if !ok {
		return
	}
	if r.Parent != "" {
		m.Message = ChildMessage
		return
	}
	m.Renaming = r.ID
	m.Prompt = textwidth.Printable(r.Label, 0)
}

// renameKey applies one key while the prompt line renames a pane. Enter
// sends pane.rename and refreshes. Esc keeps the old label.
func (f *floor) renameKey(k key) error {
	m := f.m
	switch {
	case k.kind == keyEsc:
		m.Renaming, m.Prompt = "", ""
	case k.kind == keyByte && (k.b == '\r' || k.b == '\n'):
		id, label := m.Renaming, strings.TrimSpace(m.Prompt)
		m.Renaming, m.Prompt = "", ""
		if label == "" {
			m.Message = "A label cannot be empty. The name stays."
			return nil
		}
		if _, err := call(f.o.Socket, "pane.rename", map[string]any{"pane": id, "label": label}); err != nil {
			return f.fail(err)
		}
		return f.refresh()
	default:
		editLine(&m.Prompt, k)
	}
	return nil
}

// startSwap is s: it opens the prompt line to swap the selected agent's
// model or router, for an agent that has one to swap.
func (f *floor) startSwap() {
	m := f.m
	r, ok := m.Selected()
	if !ok {
		return
	}
	if r.Parent != "" {
		m.Message = ChildMessage
		return
	}
	if mdl, rtr := swaps(r); !mdl && !rtr {
		m.Message = SwapNone(m.labelOf(r.ID), r.Harness)
		return
	}
	m.Swapping, m.Prompt = r.ID, ""
}

// swapTypeKey applies one key while the prompt line swaps. Enter reads
// the line: a model is sent at once, and a router asks first.
func (f *floor) swapTypeKey(k key) error {
	m := f.m
	switch {
	case k.kind == keyEsc:
		m.Swapping, m.Prompt = "", ""
	case k.kind == keyByte && (k.b == '\r' || k.b == '\n'):
		id, words := m.Swapping, strings.Fields(m.Prompt)
		m.Swapping, m.Prompt = "", ""
		if len(words) != 2 || (words[0] != "model" && words[0] != "router") {
			m.Message = SwapHint
			return nil
		}
		if words[0] == "router" {
			if words[1] != "direct" && words[1] != "gateway" {
				m.Message = SwapHint
				return nil
			}
			m.Confirm, m.ConfirmRouter = id, words[1]
			return nil
		}
		res, err := call(f.o.Socket, "agent.set_model", map[string]any{"pane": id, "model": words[1]})
		if err != nil {
			return f.fail(err)
		}
		m.Message = ModelDone(m.labelOf(id), words[1], str(res, "when"))
		return f.refresh()
	default:
		editLine(&m.Prompt, k)
	}
	return nil
}

// stopKey is ctrl-w. On a live row it asks once whether to stop that
// agent, and answerConfirm takes the next key. On an ended row it forgets
// the record. On Clear all it asks whether to forget them all.
func (f *floor) stopKey() error {
	m := f.m
	it, ok := m.SelectedItem()
	if !ok {
		return nil
	}
	switch it.kind {
	case itemRow:
		if it.row.Parent != "" {
			m.Message = ChildMessage
			return nil
		}
		m.Confirm, m.ConfirmMode = it.row.ID, false
	case itemEnded:
		return f.forget(it.row.ID)
	case itemClearAll:
		m.ClearAll = true
	}
	return nil
}

// answerConfirm takes the one key after a question. Enter does what it
// asked: it stops the agent, which the server removes at once, or it
// forgets every ended agent the fold lists. Any other key keeps them.
func (f *floor) answerConfirm(k key) error {
	m := f.m
	id, all, mode, router := m.Confirm, m.ClearAll, m.ConfirmMode, m.ConfirmRouter
	m.Confirm, m.ClearAll, m.ConfirmMode, m.ConfirmRouter = "", false, false, ""
	if k.kind != keyByte || (k.b != '\r' && k.b != '\n') {
		return nil
	}
	if router != "" {
		label := m.labelOf(id)
		if _, err := call(f.o.Socket, "agent.set_router", map[string]any{"pane": id, "router": router}); err != nil {
			return f.fail(err)
		}
		m.Message = RouterDone(label, router)
		return f.refresh()
	}
	if all {
		return f.clearAll()
	}
	if mode {
		return f.setMode(id, "watching")
	}
	if _, err := call(f.o.Socket, "pane.close", map[string]any{"pane": id}); err != nil {
		return f.fail(err)
	}
	m.Message = m.labelOf(id) + " stopped."
	return f.refresh()
}

// modeKey is m: the selected agent's daisugi mode. An agent the owner set
// to watching goes back to enforcing at once. An enforcing agent asks once
// first, and answerConfirm takes the next key. Off is never offered:
// turning the gate off stays the command daisugi gate disarm.
func (f *floor) modeKey() error {
	m := f.m
	r, ok := m.Selected()
	if !ok {
		return nil
	}
	if r.Parent != "" {
		m.Message = ChildMessage
		return nil
	}
	if r.Mode == "" || r.Mode == "off" {
		m.Message = ModeNoGate(m.labelOf(r.ID))
		return nil
	}
	if m.Facts != nil && m.Facts.HookRoot != "" {
		m.Message = HookRootLine(m.Facts.HookRoot)
		return nil
	}
	if r.Marker == "watching" {
		return f.setMode(r.ID, "enforcing")
	}
	if r.Mode == "watching" {
		m.Message = GateWatches(m.labelOf(r.ID))
		return nil
	}
	m.Confirm, m.ConfirmMode = r.ID, true
	return nil
}

// setMode sends agent.set_mode and says the new mode on the status line.
func (f *floor) setMode(id, mode string) error {
	if _, err := call(f.o.Socket, "agent.set_mode", map[string]any{"pane": id, "mode": mode}); err != nil {
		return f.fail(err)
	}
	f.m.Message = ModeDone(f.m.labelOf(id), mode)
	return f.refresh()
}

// forget removes one ended record and refreshes.
func (f *floor) forget(id string) error {
	f.endedDue = true
	if _, err := call(f.o.Socket, "pane.forget", map[string]any{"pane": id}); err != nil {
		return f.fail(err)
	}
	return f.refresh()
}

// clearAll forgets every ended agent the fold lists.
func (f *floor) clearAll() error {
	f.endedDue = true
	var tally batch
	for _, r := range f.m.recent() {
		_, err := call(f.o.Socket, "pane.forget", map[string]any{"pane": r.ID})
		if err := tally.add(err); err != nil {
			return err
		}
	}
	f.m.Message = tally.say("Forgot %d ended agents.")
	return f.refresh()
}

// batch counts the results of one verb sent for many agents.
type batch struct {
	done, failed int
	first        error
}

// add counts one result. A server that is gone ends the batch with its
// error.
func (b *batch) add(err error) error {
	if err == nil {
		b.done++
		return nil
	}
	if errors.Is(err, attach.ErrServerGone) {
		return err
	}
	b.failed++
	if b.first == nil {
		b.first = err
	}
	return nil
}

// say is the message for the batch: format with the count done, then
// how many failed and the first error.
func (b *batch) say(format string) string {
	msg := fmt.Sprintf(format, b.done)
	if b.failed > 0 {
		msg += fmt.Sprintf(" %d failed: %v", b.failed, b.first)
	}
	return msg
}

// resume starts an ended agent again and puts it in a window. The server
// says whether it resumed the old session or started fresh.
func (f *floor) resume(r Row) (string, bool, error) {
	cols, rows := f.newSize()
	f.endedDue = true
	res, err := call(f.o.Socket, "pane.resume", map[string]any{"pane": r.ID, "cols": cols, "rows": rows})
	if err != nil {
		return "", false, err
	}
	id, _ := res["pane"].(string)
	resumed, _ := res["resumed"].(bool)
	return id, resumed, nil
}

// enterRecent is Enter on a Recent item. The fold opens or closes. Resume
// all starts every ended agent again, Clear all asks first, and an ended
// row resumes and goes into a window.
func (f *floor) enterRecent(it item) error {
	m := f.m
	switch it.kind {
	case itemGroup:
		m.ToggleGroup(it.group.Key)
		return nil
	case itemFold:
		m.ToggleRecent()
		return nil
	case itemClearAll:
		m.ClearAll = true
		return nil
	case itemResumeAll:
		return f.resumeAllEnded()
	}
	id, resumed, err := f.resume(it.row)
	if err != nil {
		return f.fail(err)
	}
	label := m.labelOfEnded(it.row)
	if resumed {
		m.Message = label + " resumed its session."
	} else {
		m.Message = label + " started fresh. It had no session to resume."
	}
	if err := f.refresh(); err != nil {
		return err
	}
	f.showNew(id)
	return nil
}

// labelOfEnded is an ended row's label, or its id when it has none.
func (m *Model) labelOfEnded(r Row) string {
	if r.Label != "" {
		return textwidth.Printable(r.Label, 0)
	}
	return r.ID
}

// enter is Enter. While the prompt has the keyboard it runs the line, or
// only lets the keyboard go when the line is empty. On a Recent item it
// acts on that item. Otherwise it goes in. When windows show, the row's
// agent fills a window and that window takes the keys. When the screen is
// too narrow for windows, it attaches the row full screen. On an empty
// floor it opens the default harness full screen.
func (f *floor) enter() error {
	m := f.m
	m.Message = ""
	if m.Talking {
		m.Talking = false
		line := m.Prompt
		m.Prompt = ""
		if line == "" {
			return nil
		}
		// With a permanent ask open in the peek, the prompt is the name
		// field. A line that is not the name allows nothing and goes to
		// nobody.
		if p := m.Peek; p.Answerable() && p.Permanent() {
			if strings.TrimSpace(line) != p.Name() {
				m.Message = "That is not the pane name. Type " + p.Name() + " and enter to allow, or n to deny."
				return nil
			}
			return f.sendAnswer("agent.allow", map[string]any{"confirm": p.Name()}, "Allowed.")
		}
		return f.runPrompt(line)
	}
	if it, ok := m.SelectedItem(); ok {
		if it.kind != itemRow {
			return f.enterRecent(it)
		}
		if it.row.Parent != "" {
			m.Message = ChildMessage
			return nil
		}
		if f.typeInTile(it.row.ID) {
			return nil
		}
		return f.attachTo(it.row.ID)
	}
	id, err := f.createHarnessPane(f.o.Default)
	if err != nil {
		return f.fail(err)
	}
	return f.attachTo(id)
}

// newNear is n: a new agent with the default harness, near the live row
// under the cursor. The server picks its directory: that row's, else the
// most recent project. The new agent goes into a window and takes the
// keys when windows show.
func (f *floor) newNear() error {
	cols, rows := f.newSize()
	params := map[string]any{"kind": "pty", "cols": cols, "rows": rows}
	if r, ok := f.m.Selected(); ok {
		near := r.ID
		if r.Parent != "" {
			near = r.Parent
		}
		params["near"] = near
	}
	res, err := call(f.o.Socket, "pane.create", params)
	if err != nil {
		return f.fail(err)
	}
	id, _ := res["pane"].(string)
	if err := f.refresh(); err != nil {
		return err
	}
	f.sayStarted(id, res, "")
	return nil
}

// openPicker is N: it fetches project.list and opens the picker.
func (f *floor) openPicker() error {
	res, err := call(f.o.Socket, "project.list", nil)
	if err != nil {
		return f.fail(err)
	}
	p := &Picker{}
	raw, _ := res["projects"].([]any)
	for _, r := range raw {
		pm, ok := r.(map[string]any)
		if !ok || str(pm, "path") == "" {
			continue
		}
		pinned, _ := pm["pinned"].(bool)
		missing, _ := pm["missing"].(bool)
		p.Projects = append(p.Projects, Project{Path: str(pm, "path"), Name: str(pm, "name"), Pinned: pinned, Missing: missing})
	}
	f.m.Picker = p
	return nil
}

// pickerKey applies one key while the picker is open. A number from 1 to
// 9 picks that project, and 0 picks the tenth. Enter picks the one under
// the cursor, x removes a missing one from the list, the arrows move, and
// Esc closes the picker.
// A missing project starts nothing: its row says so.
func (f *floor) pickerKey(k key) error {
	m := f.m
	p := m.Picker
	switch {
	case k.kind == keyEsc:
		m.Picker = nil
	case k.kind == keyUp:
		p.Cursor = clamp(p.Cursor-1, len(p.Projects))
	case k.kind == keyDown:
		p.Cursor = clamp(p.Cursor+1, len(p.Projects))
	case k.kind == keyByte && (k.b == '\r' || k.b == '\n'):
		if len(p.Projects) == 0 {
			m.Picker = nil
			return nil
		}
		return f.startIn(p.Projects[p.Cursor])
	case k.kind == keyByte && k.b == 'x':
		return f.removeProject()
	case k.kind == keyByte && k.b >= '0' && k.b <= '9':
		i := int(k.b - '1') // 1 is the first project and 0 the tenth
		if k.b == '0' {
			i = 9
		}
		if i >= len(p.Projects) {
			m.Message = fmt.Sprintf("There is no project %d.", i+1)
			return nil
		}
		return f.startIn(p.Projects[i])
	case k.kind == keyMouse:
		return f.click(k.click)
	}
	return nil
}

// startIn starts the default harness in project pr, closes the picker,
// and puts the new agent in a window.
func (f *floor) startIn(pr Project) error {
	if pr.Missing {
		f.m.Message = pr.Name + " is missing. x removes it from the list."
		return nil
	}
	f.m.Picker = nil
	res, err := f.createReply(map[string]any{"harness": f.o.Default, "cwd": pr.Path})
	if err != nil {
		return f.fail(err)
	}
	id, _ := res["pane"].(string)
	if err := f.refresh(); err != nil {
		return err
	}
	f.sayStarted(id, res, pr.Name)
	return nil
}

// removeProject is x in the picker: it takes the missing project under
// the cursor out of the list. A project whose directory is there is
// refused, since x is only for the ones that are gone.
func (f *floor) removeProject() error {
	p := f.m.Picker
	if len(p.Projects) == 0 {
		return nil
	}
	pr := p.Projects[p.Cursor]
	if !pr.Missing {
		f.m.Message = pr.Name + " is there. x removes only a missing project."
		return nil
	}
	if _, err := call(f.o.Socket, "project.remove", map[string]any{"path": pr.Path}); err != nil {
		return f.fail(err)
	}
	p.Projects = append(p.Projects[:p.Cursor], p.Projects[p.Cursor+1:]...)
	p.Cursor = clamp(p.Cursor, len(p.Projects))
	f.m.Message = "Removed " + pr.Name + " from the list."
	return nil
}

// resumeAllEnded resumes every ended agent, as the Resume all item does,
// and says how many started again.
func (f *floor) resumeAllEnded() error {
	m := f.m
	var tally batch
	for _, r := range m.recent() {
		_, _, err := f.resume(r)
		if err := tally.add(err); err != nil {
			return err
		}
	}
	m.Message = tally.say("Started %d agents again.")
	return f.refresh()
}

// sayRestart asks the server once, at the start. When restore.resumable
// says agents that stopped with the old server can still come back, the
// status line says so and the footer offers R. Any key ends the line for
// the rest of the session. It never takes the place of a line already
// there, an error included. A server that does not answer says nothing:
// the line is a courtesy, and Recent still lists them.
func (f *floor) sayRestart() {
	res, err := call(f.o.Socket, "server.status", nil)
	if err != nil {
		return
	}
	back := 0
	if rs, ok := res["restore"].(map[string]any); ok {
		if v, ok := rs["resumable"].(float64); ok {
			back = int(v)
		}
	}
	if back > 0 && f.m.Message == "" {
		f.m.Restarted = back
		f.m.Message = RestartLine(back)
	}
}

// showNew puts a new agent where the owner sees it. When windows show,
// it fills a window and takes the keys. Otherwise the cursor moves to its
// row and the message says how to go in.
func (f *floor) showNew(id string) {
	if id == "" {
		return
	}
	if !f.placeNew(id) && f.m.Message == "" {
		f.m.Message = f.m.labelOf(id) + " started. Enter goes in."
	}
}

// placeNew moves the cursor to the new agent's row and, when windows
// show, puts it in a window with the keys. It reports whether it did.
func (f *floor) placeNew(id string) bool {
	m := f.m
	m.Cursor = m.indexOf(id, m.Cursor)
	return f.typeInTile(id)
}

// sayStarted is the status line after a new agent: where it started and
// why. An agent that is not in a window also says how to go in.
func (f *floor) sayStarted(id string, res map[string]any, where string) {
	if id == "" {
		return
	}
	tail := ""
	if !f.placeNew(id) {
		tail = " Enter goes in."
	}
	line := StartedLine(res, where)
	// A long path would push the end of the line off the screen. The
	// directory's own name says where, and the tail still shows.
	if cols, _ := f.o.Size(); textwidth.Width(line+tail) > cols {
		short := make(map[string]any, len(res))
		for k, v := range res {
			short[k] = v
		}
		if dir, _ := res["cwd"].(string); dir != "" {
			short["cwd"] = dirBase(dir)
		}
		line = StartedLine(short, where)
	}
	f.m.Message = line + tail
}

// click applies one mouse report. A release and a wheel event do nothing.
// A left press on the ended line clears it and opens Recent. A left press
// on the close mark of a window header or a live row asks to stop that
// agent, and on an ended row forgets it. A left press inside a window
// focuses that slot and gives its agent the keys, and a second left press
// on the same window within doubleClick attaches that agent full screen.
// Any other left press gives the keys back to the rail. A left press on a
// live row moves the cursor there, and when windows show, fills the agent
// into a window and gives it the keys; a release over another window then
// drags it there. On a group header or a Recent item it acts as Enter
// does. A middle press on a row opens a new slot for its agent. A release
// ends a drag and does nothing else.
func (f *floor) click(c Click) error {
	m := f.m
	if c.Release {
		f.release(c)
		return nil
	}
	if c.Button >= 64 {
		return nil
	}
	f.dragFrom = ""
	cols, _ := f.o.Size()
	if c.Button == 0 && m.EndedY >= 0 && c.Y-1 == m.EndedY {
		f.setTyping("")
		m.clearEnded()
		if !m.RecentOpen {
			m.ToggleRecent()
		}
		return nil
	}
	if c.Button == 0 && m.Tiles != nil && OnTiles(m, c.Y) {
		if slot, ok := HitTile(m, c.X); ok {
			id := ""
			if shown := m.Tiles.Shown(windowCount(cols)); slot < len(shown) {
				id = shown[slot]
			}
			if id != "" && c.Y-1 == m.HeaderY && slot < len(m.TileX) && c.X-1 >= m.TileX[slot]+m.TileW[slot]-2 {
				f.setTyping("")
				m.Confirm, m.ConfirmMode = id, false
				return nil
			}
			if word := m.toggleWord(id); word != "" && c.Y-1 == m.HeaderY && slot < len(m.TileX) &&
				c.X-1 >= m.TileX[slot]+m.TileW[slot]-3-textwidth.Width(word) && c.X-1 < m.TileX[slot]+m.TileW[slot]-2 {
				m.Tiles.Focus = slot
				f.toggleScreen()
				f.setTyping(id)
				return nil
			}
			m.Tiles.Focus = slot
			now := time.Now()
			double := id != "" && id == f.lastPane && slot == f.lastSlot && now.Sub(f.lastPress) < doubleClick
			f.lastPress, f.lastSlot, f.lastPane = now, slot, id
			if double {
				f.lastPress = time.Time{}
				return f.attachTo(id)
			}
			f.setTyping(id)
			return nil
		}
	}
	f.lastPress = time.Time{}
	if c.Button == 0 {
		f.setTyping("")
	}
	i, ok := HitRow(m, c.Y)
	if !ok {
		return nil
	}
	m.Cursor = i
	m.landed()
	it, ok := m.SelectedItem()
	if !ok {
		return nil
	}
	onClose := !m.slim && c.X-1 >= m.RailW-2
	if it.kind != itemRow {
		switch {
		case c.Button != 0:
		case it.kind == itemEnded && onClose:
			return f.forget(it.row.ID)
		case it.kind != itemEnded:
			return f.enterRecent(it)
		}
		return nil
	}
	r := it.row
	if r.Parent != "" {
		m.Message = ChildMessage
		return nil
	}
	if c.Button == 0 && onClose {
		m.Confirm, m.ConfirmMode = r.ID, false
		return nil
	}
	if m.Tiles == nil {
		return nil
	}
	switch c.Button {
	case 0:
		// The press only marks the row. The release decides: over a
		// window it drags the agent there, on the same row it is a click
		// that fills a window and takes the keys. Nothing moves before
		// the release, so a drag trades against the windows as they were.
		f.dragFrom = r.ID
	case 1:
		m.Tiles.Open(r.ID)
	}
	return nil
}

// release ends a left press on a live row. Over a window it puts that
// row in the window under the release, by the same trade as the number
// keys. On the same row it is a click: the agent fills a window and takes
// the keys. Any other release does nothing.
func (f *floor) release(c Click) {
	m := f.m
	from := f.dragFrom
	f.dragFrom = ""
	if from == "" || c.Button != 0 || m.Tiles == nil || !m.has(from) {
		return
	}
	if OnTiles(m, c.Y) {
		if slot, ok := HitTile(m, c.X); ok {
			f.putInSlot(from, slot)
			return
		}
	}
	if i, ok := HitRow(m, c.Y); ok {
		if all := m.items(); i < len(all) && all[i].kind == itemRow && all[i].row.ID == from {
			f.typeInTile(from)
		}
	}
}

// putInSlot puts pane id in slot i. When it is already in another slot,
// the two slots trade places. Focus goes to slot i.
func (f *floor) putInSlot(id string, i int) {
	ts := f.m.Tiles
	for len(ts.Slots) <= i {
		ts.Slots = append(ts.Slots, "")
	}
	if from, ok := ts.SlotOf(id); ok {
		ts.Slots[from] = ts.Slots[i]
	}
	ts.Slots[i] = id
	ts.Focus = i
}

// toWindow is a number key on the rail: the live row under the cursor
// goes into window n. When it is already in another window, the two
// windows trade places. The keys stay on the rail.
func (f *floor) toWindow(n int) error {
	m := f.m
	r, ok := m.Selected()
	if !ok {
		return nil
	}
	if r.Parent != "" {
		m.Message = ChildMessage
		return nil
	}
	cols, _ := f.o.Size()
	shown := windowCount(cols)
	if shown == 0 || m.Tiles == nil {
		m.Message = "This screen is too narrow for windows. Enter goes in."
		return nil
	}
	if n > shown {
		m.Message = fmt.Sprintf("This screen shows %d windows. Press 1 to %d.", shown, shown)
		return nil
	}
	f.growSlots(n)
	f.putInSlot(r.ID, n-1)
	return nil
}
