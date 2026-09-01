# Omarchy roadmap: hand checks

The tests cover each rule with fakes. These steps need a person, a real
terminal, a real browser, a phone and a microphone. Run them on the box that
runs coppice. Each line says what you do and what you should see.

Before you start, rebuild and restart:

```
cd harness/coppice && ./scripts/install.sh --link
coppice server stop; coppice server start
```

## Part 1: the floor

### Start and one click

1. Run `coppice` in a terminal. The keys start on an agent that needs you, or on
   the first window, or on the rail. They never start on the bottom line.
2. Press `n`. A new agent starts near the selected one, with a name like
   `claude-<project>`. Press `N`, pick a project with its number. An agent
   starts there.
3. Run `coppice web`. The browser opens, signed in. Press New. An agent starts in
   one click. Open the small arrow beside New and pick a project.
4. Run `coppice project add ~/some/project` for each of your projects. They show
   in the New menu and under `N`, pinned first.

### Windows and keys

5. On the web at laptop width, type into the selected window. The keys reach the
   agent. The window has a blue border and says "typing here". Press
   ctrl-space. The rail has the keys.
6. Drag a row onto a window. That agent shows there and no other agent loses its
   window.
7. Double click a label, or press F2 on a row. Rename it in place.
8. Press × on a row. The line "Stop <name>? Enter stops it, Esc keeps it."
   shows. Press Esc. Nothing stops. Press × again and Enter. The agent stops
   and leaves the rail.

### Ended agents and Recent

9. In an agent's shell, run `exit 3`. The window shows "<name> ended (exit 3)"
   in amber until you click it. The agent is in Recent at the foot of the rail.
10. Press Resume on it. A claude agent resumes its conversation. Restart the
    server. Every agent that was running is in Recent, and Resume all brings
    them back.

### The stack bar and daisugi

11. Each window shows a bar: loop, daisugi and its mode, router, model. With
    claude under daisugi enforcing, the model name shows after the first reply,
    and "tokens today" grows in the header.
12. Ask an agent to run `curl https://example.com` in a project whose envelope
    does not allow curl. The window shows a red ✕. Click the daisugi part of the
    bar. It says why.

### Headless agents

13. Start a sprig agent (`coppice pane create --kind headless --harness sprig
    --cwd <dir>`). Its window has a message line at the foot. Type a sentence
    and press Enter. The agent gets the whole sentence once.

### Voice

14. Press Mic on the web. With daisugi's voice extra installed, speak a
    sentence. The text lands in the box, not sent. Without the extra, the page
    says so and offers the install line, typed and not run.
15. In the terminal floor, hold the talk key (shown in the footer) and speak.
    Release it. The text lands in the selected agent's input, not sent. In a
    terminal without key release reports, press once to start and once to stop.

### The foreman

16. Type a sentence in "Tell the floor", or speak it. With no foreman running,
    one starts in `~/.local/state/coppice/foreman`, reads its page, then gets
    your sentence. Ask it to start an agent in two of your projects and report
    back. It does, one line per agent.

### The phone

17. Open the floor on your phone over the tailnet. Home is the list with the
    colony strip at the top. Tap an agent. Its window slides in from the right.
    Type in the bottom bar and press Enter. Swipe right. You are home, and the
    page never reloaded.
18. With the phone keyboard open, the bottom bar stays above it.

### After you finish

19. Run `coppice pane forget w1:p15` on your own server. That record came from a
    reviewer's mistake on 2026-09-24.
