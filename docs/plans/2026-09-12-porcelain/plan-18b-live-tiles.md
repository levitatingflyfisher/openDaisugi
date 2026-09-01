# Plan 18b: live tiles and the phone dock

**Goal.** Every pane the floor shows takes the keyboard when it has focus. The operator sees all panes at once and types into any of them without a screen change. The phone keeps one rail as a dock and shows one full pane above it.

**Why.** The first hand run of the floor showed that view-only tiles force a screen change for every line typed. Enter is how a harness submits, so Enter can never mean "go full screen" while a pane has focus. ctrl-] is hard to reach. The operator wants the dock-and-pane shape on the phone.

**Spec.** This file. It amends plan 15 (tiles were view-only) and plan 17 (ctrl-] leaves, Enter goes in).

## Rulings

- L1 Focus. The floor has one keyboard owner: the roster, or one tile. `Model.Typing string` holds the pane id of the tile that owns the keyboard, empty for the roster. A left click on a tile gives it the keyboard. A left click on the roster or the prompt gives the keyboard back to the roster. The leave key gives it back too.
- L2 Leave key. The default leave key becomes ctrl-space, byte 0x00. Claude Code binds no ctrl-space, and it is one hand. `config.ParseKey` accepts `ctrl-space` and `ctrl-@` as 0x00. ctrl-] and the other ctrl keys stay settable in `[keys] leave`. The hold stays: one press leaves after 300 ms, two presses inside the hold forward one leave byte. The same key leaves a full-screen attach.
- L3 Typing into a tile. While a tile owns the keyboard, every byte goes to its pane with `pane.send_text`, raw, the way attach sends it, Enter, Esc, Tab, and arrows included. Only the leave key and mouse reports are held back, through `attach.KeyReader`. ctrl-c goes to the pane too; ctrl-c quits the floor only while the roster owns the keyboard.
- L4 Size. Every terminal tile attaches with input rights and with its inner size: the tile width, and the tile height less its header line. When the tile size changes the floor sends `pane.resize`. A pane shown in a tile takes the tile's size, the way tmux does. When a full-screen attach returns, the tiles send their sizes again. view_only stays in the protocol for the peek and for callers that want it.
- L5 Scream the mode. The tile that owns the keyboard draws its header in reverse video with the word `typing` and the leave key's name, like `typing · ctrl-space leaves`. The footer, while a tile types, shows only `ctrl-space roster  click a tile to move`. The roster rows draw dim.
- L6 Enter on the roster. Enter on a row gives that pane the keyboard. When tiles show, the pane fills the focused slot and types there. When the screen is too narrow for tiles, Enter attaches full screen as before. Full screen from tiles is a double click on a tile, or the prompt word `zoom`, which attaches the typing pane or the row under the cursor. The key table help for Enter stays `go in`.
- L7 Phone dock. Under 600 px the floor page is one pane screen above a dock. The dock is one horizontal band fixed at the bottom, one chip per pane in roster order, NEEDS YOU first, the chip amber when the pane is blocked, the current pane's chip marked. A tap on a chip opens that pane above the dock. A swipe left or right on the pane opens the next or previous chip's pane. The pane screen keeps its text box and key row, which send `pane.send_text` and `pane.send_keys`; the attach is a full attach with no size, so the phone never resizes a pane. The ask bar and Deny stay on the pane screen.
- L8 Web tiles type. At 600 px and wider a click on a tile gives it the keyboard; the tile's border marks it and the tile header says `typing`. keydown events on the page go to that pane as raw bytes through `pane.send_text`: printable text, Enter as `\r`, Backspace as `\x7f`, Tab, Esc, and the arrows as their VT sequences. ctrl-space or a click on the rail gives the keyboard back. A focused text field on the page keeps its own keys.
- L9 Docs. README floor section, PROTOCOL.md where it names the leave key, the footer text, and RESUME.md follow the new keys. ROADMAP.md rules line is the controller's.

## Tasks

1. `internal/config`: ParseKey and DefaultLeave per L2, tests.
2. `internal/tui`: Typing focus, key routing, tile attaches with size and `pane.resize`, header and footer per L5, Enter and double click and `zoom` per L6, click routing per L1. Tests drive the floor over a pipe with a fake server as the other run tests do: a click on a tile then typed bytes reach `pane.send_text` for that pane with Enter as `\r`; the leave key returns the keyboard and the next byte edits the prompt; ctrl-c inside a typing tile reaches the pane and does not quit; a tile attach carries cols and rows equal to its inner size; Enter on a row with tiles showing types into that pane and does not attach full screen.
3. `internal/web/static`: the dock per L7 and typing tiles per L8, tests in `_tests` in the style of floor.test.mjs.
4. Docs per L9.
