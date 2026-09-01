-- The spec for the pure roster core. Run it from plugins/nvim:
--   nvim --headless --clean -c "luafile tests/roster_spec.lua"
-- It quits with code 1 when any case fails, and with 0 when all pass.

package.path = "lua/?.lua;lua/?/init.lua;" .. package.path

local failures = 0
local passes = 0

local function check(name, fn)
  local ok, err = pcall(fn)
  if ok then
    passes = passes + 1
  else
    failures = failures + 1
    io.stderr:write("FAIL " .. name .. ": " .. tostring(err) .. "\n")
  end
end

local function eq(got, want, what)
  if got ~= want then
    error((what or "value") .. ": want " .. tostring(want) .. ", got " .. tostring(got), 2)
  end
end

local ok_load, roster = pcall(require, "coppice.roster")
if not ok_load then
  io.stderr:write("FAIL cannot load coppice.roster: " .. tostring(roster) .. "\n")
  failures = failures + 1
  roster = nil
end

-- A width in cells that counts each code point as one cell, and a width that
-- counts the ideographs as two, the way a terminal draws them.
local function cells(s)
  return roster and roster.width(s) or #s
end

local function wide_cells(s)
  local n = 0
  for _, cp in ipairs(roster.codepoints(s)) do
    if cp >= 0x4E00 and cp <= 0x9FFF then
      n = n + 2
    else
      n = n + 1
    end
  end
  return n
end

local rows = {
  { id = "w1:p1", label = "docs", state = "idle" },
  { id = "w1:p2", label = "build", state = "blocked", ask = { tool = "Bash", summary = "rm -rf build/" } },
  { id = "w1:p3", label = "tests", state = "working" },
  { id = "w1:p4", label = "old", state = "idle", closed = true },
  { id = "w1:p5", label = "", state = "blocked" },
}

if roster then
  check("group puts blocked rows first, in their order", function()
    local g = roster.group(rows)
    eq(g[1].id, "w1:p2", "first")
    eq(g[2].id, "w1:p5", "second")
    eq(g[3].id, "w1:p3", "third")
    eq(g[4].id, "w1:p1", "fourth")
  end)

  check("group leaves closed rows out", function()
    local g = roster.group(rows)
    eq(#g, 4, "rows")
    for _, r in ipairs(g) do
      if r.id == "w1:p4" then
        error("a closed row stayed")
      end
    end
  end)

  check("group reads an unknown state as unknown and keeps it after idle", function()
    local g = roster.group({ { id = "a", state = "strange" }, { id = "b", state = "idle" } })
    eq(g[1].id, "b", "first")
    eq(g[2].id, "a", "second")
  end)

  check("render never returns a line wider than width", function()
    for width = 0, 60 do
      local lines = roster.render(roster.group(rows), width)
      for i, l in ipairs(lines) do
        if cells(l) > width then
          error("line " .. i .. " is " .. cells(l) .. " cells at width " .. width .. ": " .. l)
        end
      end
    end
  end)

  check("render counts wide cells with the measure it is given", function()
    local wide = { { id = "w1:p9", label = "建造建造建造建造建造", state = "working" } }
    for width = 0, 30 do
      local lines = roster.render(wide, width, wide_cells)
      for i, l in ipairs(lines) do
        if wide_cells(l) > width then
          error("line " .. i .. " is " .. wide_cells(l) .. " cells at width " .. width)
        end
      end
    end
  end)

  -- utf8_ok checks s byte by byte, with no code from the roster core, so a
  -- bug in the core's own splitter cannot pass both.
  local function utf8_ok(s)
    local i, n = 1, #s
    while i <= n do
      local c = s:byte(i)
      local need
      if c < 0x80 then
        need = 0
      elseif c >= 0xC2 and c <= 0xDF then
        need = 1
      elseif c >= 0xE0 and c <= 0xEF then
        need = 2
      elseif c >= 0xF0 and c <= 0xF4 then
        need = 3
      else
        return false
      end
      if i + need > n then
        return false
      end
      for k = 1, need do
        local b = s:byte(i + k)
        if b < 0x80 or b > 0xBF then
          return false
        end
      end
      i = i + need + 1
    end
    return true
  end

  check("render never splits a UTF-8 sequence", function()
    local wide = {
      { id = "w1:p9", label = "ééééééééééééé", state = "idle" },
      { id = "w1:p8", label = "建造建造建造", state = "working" },
      { id = "w1:p7", label = "😀😀😀😀😀😀", state = "blocked" },
    }
    for width = 0, 40 do
      for _, l in ipairs(roster.render(wide, width)) do
        if not utf8_ok(l) then
          error("a cut split a sequence at width " .. width)
        end
      end
    end
  end)

  check("the spec's own check refuses a split sequence", function()
    if utf8_ok("\195") or utf8_ok("a\228\187") or not utf8_ok("aé建😀") then
      error("utf8_ok is wrong")
    end
  end)

  check("render drops control runes from a label", function()
    local bad = { { id = "w1:p1", label = "a\nb\27[31mc", state = "idle" } }
    for _, l in ipairs(roster.render(bad, 80)) do
      if l:find("[%c]") then
        error("a control rune reached a line: " .. l)
      end
    end
  end)

  check("render heads the roster with the count that needs you", function()
    local lines = roster.render(roster.group(rows), 80)
    eq(lines[1], "coppice · 2 need you", "head")
    local quiet = roster.render({ { id = "a", state = "idle" } }, 80)
    eq(quiet[1], "coppice · quiet", "quiet head")
  end)

  check("render names each pane by its label, or its id when it has none", function()
    local lines, _, ids = roster.render(roster.group(rows), 80)
    eq(ids[2], "w1:p2", "id of line 2")
    if not lines[2]:find("build", 1, true) then
      error("line 2 has no label: " .. lines[2])
    end
    if not lines[2]:find("Bash: rm -rf build/", 1, true) then
      error("line 2 has no ask: " .. lines[2])
    end
    if not lines[3]:find("w1:p5", 1, true) then
      error("line 3 has no id: " .. lines[3])
    end
    eq(ids[1], nil, "the head names no pane")
  end)

  check("render gives each state word a highlight span inside its line", function()
    local lines, spans = roster.render(roster.group(rows), 80)
    eq(#spans, 4, "spans")
    eq(spans[1].group, "CoppiceBlocked", "first group")
    eq(spans[3].group, "CoppiceWorking", "third group")
    for _, s in ipairs(spans) do
      local l = lines[s.line + 1]
      if s.col_start < 0 or s.col_end > #l or s.col_start >= s.col_end then
        error("span outside line " .. s.line)
      end
    end
  end)

  check("a reply whose result is null reads as an empty result", function()
    local wire = require("coppice.wire")
    local null = {}
    local res, err = wire.reply({ id = "1", ok = true, result = null }, null)
    eq(type(res), "table", "result")
    eq(next(res), nil, "empty result")
    eq(err, nil, "error")
    res, err = wire.reply({ id = "1", ok = false, error = { message = "no pane" } }, null)
    eq(res, nil, "refused result")
    eq(err, "no pane", "refused error")
    res, err = wire.reply({ id = "1", ok = false, error = null }, null)
    eq(res, nil, "refused result")
    eq(err, "the server refused", "a null error")
  end)

  check("a held ask counts and sorts as working, not as need you", function()
    local held = {
      { id = "a", state = "blocked", held = { by = "f", task = "t1" } },
      { id = "b", state = "blocked" },
    }
    local grouped = roster.group(held)
    eq(grouped[1].id, "b", "the free ask comes first")
    local lines = roster.render(grouped, 80)
    eq(lines[1], "coppice · 1 need you", "head")
  end)

  check("render says so when there is no open pane", function()
    local lines, spans = roster.render({}, 80)
    eq(lines[2], "no open panes", "empty")
    eq(#spans, 0, "spans")
  end)
end

io.stdout:write(string.format("roster spec: %d passed, %d failed\n", passes, failures))
if vim then
  if failures > 0 then
    vim.cmd("cquit 1")
  else
    vim.cmd("qall!")
  end
else
  os.exit(failures > 0 and 1 or 0)
end
