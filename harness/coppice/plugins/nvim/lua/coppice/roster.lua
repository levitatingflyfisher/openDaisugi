-- The pure roster core. It holds no vim call, so a spec can run it in any
-- Lua 5.1. group orders the rows, and render turns them into lines, the
-- highlight spans of the state words, and the pane id of each line.

local M = {}

-- The order the roster shows states in. The panes that need you come first.
local ORDER = { blocked = 1, working = 2, idle = 3, unknown = 4, done = 5 }

-- The highlight group of each state word.
local GROUPS = {
  blocked = "CoppiceBlocked",
  working = "CoppiceWorking",
  idle = "CoppiceIdle",
  unknown = "CoppiceUnknown",
  done = "CoppiceDone",
}

local ELLIPSIS = "…"

-- codepoints returns the code points of s. A byte that starts no valid
-- sequence counts as one code point of its own value.
function M.codepoints(s)
  local out = {}
  local i, n = 1, #s
  while i <= n do
    local c = s:byte(i)
    local len, cp = 1, c
    if c >= 0xF0 and c <= 0xF4 then
      len, cp = 4, c % 0x08
    elseif c >= 0xE0 then
      len, cp = 3, c % 0x10
    elseif c >= 0xC2 and c <= 0xDF then
      len, cp = 2, c % 0x20
    end
    local ok = i + len - 1 <= n
    for k = 1, len - 1 do
      local b = ok and s:byte(i + k)
      if not b or b < 0x80 or b > 0xBF then
        ok = false
        break
      end
      cp = cp * 64 + (b % 0x40)
    end
    if not ok then
      len, cp = 1, c
    end
    out[#out + 1] = cp
    i = i + len
  end
  return out
end

-- chars splits s into its UTF-8 sequences, so a cut never lands inside one.
local function chars(s)
  local out = {}
  local i, n = 1, #s
  while i <= n do
    local c = s:byte(i)
    local len = 1
    if c >= 0xF0 and c <= 0xF4 then
      len = 4
    elseif c >= 0xE0 and c <= 0xEF then
      len = 3
    elseif c >= 0xC2 and c <= 0xDF then
      len = 2
    end
    local ok = i + len - 1 <= n
    for k = 1, len - 1 do
      local b = ok and s:byte(i + k)
      if not b or b < 0x80 or b > 0xBF then
        ok = false
        break
      end
    end
    if not ok then
      len = 1
    end
    out[#out + 1] = s:sub(i, i + len - 1)
    i = i + len
  end
  return out
end

-- valid_utf8 reports whether every byte of s belongs to a whole sequence.
function M.valid_utf8(s)
  for _, ch in ipairs(chars(s)) do
    local c = ch:byte(1)
    if #ch == 1 and c >= 0x80 then
      return false
    end
  end
  return true
end

-- width is the default measure: one cell for each code point. A caller in
-- nvim passes vim.fn.strdisplaywidth, which counts wide glyphs as two.
function M.width(s)
  return #M.codepoints(s)
end

-- clean drops control runes and bytes that start no valid sequence, so a
-- label can never break a buffer line.
local function clean(s)
  if type(s) ~= "string" then
    return ""
  end
  local out = {}
  for _, ch in ipairs(chars(s)) do
    local c = ch:byte(1)
    local control = #ch == 1 and (c < 0x20 or c == 0x7F or c >= 0x80)
    if #ch == 2 and c == 0xC2 and ch:byte(2) < 0xA0 then
      control = true
    end
    if not control then
      out[#out + 1] = ch
    end
  end
  return table.concat(out)
end

-- fit cuts line to at most width cells under measure. A cut line ends in an
-- ellipsis when the ellipsis fits.
local function fit(line, width, measure)
  if width <= 0 then
    return ""
  end
  if measure(line) <= width then
    return line
  end
  local parts = chars(line)
  local tail = measure(ELLIPSIS) <= width and ELLIPSIS or ""
  local keep = {}
  for _, ch in ipairs(parts) do
    keep[#keep + 1] = ch
    if measure(table.concat(keep) .. tail) > width then
      keep[#keep] = nil
      break
    end
  end
  return table.concat(keep) .. tail
end

-- state_of is the state the roster shows. A blocked row whose ask a
-- foreman holds reads as working, since it waits on the foreman first.
local function state_of(row)
  local s = row.state
  if s == "blocked" and type(row.held) == "table" then
    return "working"
  end
  if ORDER[s] then
    return s
  end
  return "unknown"
end

-- group returns the open rows, blocked first, then working, idle, unknown
-- and done. Rows in one state keep the order they came in.
function M.group(rows)
  local open = {}
  for i, r in ipairs(rows or {}) do
    if not r.closed then
      open[#open + 1] = { row = r, at = i }
    end
  end
  table.sort(open, function(a, b)
    local oa, ob = ORDER[state_of(a.row)], ORDER[state_of(b.row)]
    if oa ~= ob then
      return oa < ob
    end
    return a.at < b.at
  end)
  local out = {}
  for _, e in ipairs(open) do
    out[#out + 1] = e.row
  end
  return out
end

-- render returns three lists. lines holds the head line and one line per
-- row, none wider than width cells. spans holds one highlight span per
-- state word: line is 0-based, col_start and col_end are byte columns.
-- ids maps a 1-based line number to its pane id.
function M.render(rows, width, measure)
  measure = measure or M.width
  width = math.floor(tonumber(width) or 0)
  local lines, spans, ids = {}, {}, {}
  local need = 0
  for _, r in ipairs(rows or {}) do
    if state_of(r) == "blocked" then
      need = need + 1
    end
  end
  local head = need == 0 and "coppice · quiet" or ("coppice · " .. need .. " need you")
  lines[1] = fit(head, width, measure)
  if #(rows or {}) == 0 then
    lines[2] = fit("no open panes", width, measure)
    return lines, spans, ids
  end
  for _, r in ipairs(rows) do
    local state = state_of(r)
    local name = clean(r.label)
    if name == "" then
      name = clean(r.id)
    end
    local line = string.format("%-8s %s", state, name)
    if type(r.ask) == "table" then
      local ask = clean(r.ask.tool)
      local summary = clean(r.ask.summary)
      if summary ~= "" then
        ask = ask .. ": " .. summary
      end
      if ask ~= "" then
        line = line .. "  " .. ask
      end
    end
    line = fit(line, width, measure)
    lines[#lines + 1] = line
    ids[#lines] = r.id
    local stop = math.min(#state, #line)
    if stop > 0 and line:sub(1, stop) == state:sub(1, stop) then
      spans[#spans + 1] = { line = #lines - 1, col_start = 0, col_end = stop, group = GROUPS[state] }
    end
  end
  return lines, spans, ids
end

return M
