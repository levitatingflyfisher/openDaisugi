-- The coppice floor inside Neovim. :Coppice opens the roster in a floating
-- window. <Space> shows the pane's screen in a split, read-only. <CR> opens
-- a terminal buffer that runs coppice attach on the pane. <Esc> and q close
-- the roster, and r reads it again.
--
-- The plugin speaks the socket protocol in PROTOCOL.md: one JSON request per
-- line, one reply per request. It runs pane.list and pane.read, and nothing
-- else. It never runs agent.allow or agent.deny.

local roster = require("coppice.roster")
local wire = require("coppice.wire")

local M = {}

local config = {
  socket = nil,
  coppice = "coppice",
}

local chan = nil
local pending = {}
local partial = ""
local next_id = 0

-- default_socket is the path the coppice server listens on when nothing
-- else is set.
local function default_socket()
  local env = vim.env.COPPICE_SOCKET
  if env and env ~= "" then
    return env
  end
  local rt = vim.env.XDG_RUNTIME_DIR
  if rt and rt ~= "" then
    return rt .. "/coppice/server.sock"
  end
  return vim.fn.expand("~") .. "/.opendaisugi/coppice/server.sock"
end

local function socket()
  return config.socket or default_socket()
end

local function say(msg, level)
  vim.schedule(function()
    vim.notify("coppice: " .. msg, level or vim.log.levels.INFO)
  end)
end

-- on_data joins the chunks the channel gives into whole lines. The last
-- item of each chunk may be part of a line, so it waits for the next one.
local function on_data(_, data, _)
  if not data then
    return
  end
  if #data == 1 and data[1] == "" then
    chan = nil
    for id, cb in pairs(pending) do
      pending[id] = nil
      cb(nil, "the socket closed")
    end
    return
  end
  data[1] = partial .. data[1]
  partial = data[#data]
  for i = 1, #data - 1 do
    local line = data[i]
    if line ~= "" then
      local ok, msg = pcall(vim.json.decode, line)
      if ok and type(msg) == "table" and msg.id ~= nil then
        local cb = pending[msg.id]
        pending[msg.id] = nil
        if cb then
          cb(wire.reply(msg, vim.NIL))
        end
      end
    end
  end
end

local function connect()
  if chan then
    return true
  end
  local ok, id = pcall(vim.fn.sockconnect, "pipe", socket(), { on_data = on_data })
  if not ok or id == 0 then
    say("cannot reach the server at " .. socket() .. ". Run: coppice server start", vim.log.levels.ERROR)
    return false
  end
  chan = id
  partial = ""
  return true
end

-- request sends one verb and calls cb with the result, or with nil and a
-- message. cb runs on the main loop.
local function request(cmd, params, cb)
  if not connect() then
    return
  end
  next_id = next_id + 1
  local id = "nvim-" .. next_id
  local body = vim.deepcopy(params or {})
  body.id = id
  body.cmd = cmd
  pending[id] = function(res, err)
    vim.schedule(function()
      cb(res, err)
    end)
  end
  vim.fn.chansend(chan, vim.json.encode(body) .. "\n")
end

local function define_highlights()
  local links = {
    CoppiceBlocked = "DiagnosticError",
    CoppiceWorking = "DiagnosticWarn",
    CoppiceIdle = "DiagnosticOk",
    CoppiceUnknown = "Comment",
    CoppiceDone = "Comment",
  }
  for group, target in pairs(links) do
    vim.api.nvim_set_hl(0, group, { link = target, default = true })
  end
end

local ns = vim.api.nvim_create_namespace("coppice")
local view = { buf = nil, win = nil, ids = {} }

local function close()
  if view.win and vim.api.nvim_win_is_valid(view.win) then
    vim.api.nvim_win_close(view.win, true)
  end
  view.win = nil
end

local function pane_at_cursor()
  if not (view.win and vim.api.nvim_win_is_valid(view.win)) then
    return nil
  end
  local line = vim.api.nvim_win_get_cursor(view.win)[1]
  return view.ids[line]
end

local function draw(rows)
  local width = view.win and vim.api.nvim_win_get_width(view.win) or 60
  local lines, spans, ids = roster.render(roster.group(rows), width, vim.fn.strdisplaywidth)
  view.ids = ids
  vim.bo[view.buf].modifiable = true
  vim.api.nvim_buf_set_lines(view.buf, 0, -1, false, lines)
  vim.bo[view.buf].modifiable = false
  vim.api.nvim_buf_clear_namespace(view.buf, ns, 0, -1)
  for _, s in ipairs(spans) do
    vim.api.nvim_buf_set_extmark(view.buf, ns, s.line, s.col_start, { end_col = s.col_end, hl_group = s.group })
  end
end

local function refresh()
  request("pane.list", {}, function(res, err)
    if not res then
      say(err, vim.log.levels.ERROR)
      return
    end
    if view.buf and vim.api.nvim_buf_is_valid(view.buf) then
      draw(res.panes or {})
    end
  end)
end

-- peek shows the pane's visible screen in a split. The buffer is a copy and
-- takes no input.
local function peek()
  local pane = pane_at_cursor()
  if not pane then
    return
  end
  request("pane.read", { pane = pane, source = "visible" }, function(res, err)
    if not res then
      say(err, vim.log.levels.ERROR)
      return
    end
    local text = type(res.text) == "string" and res.text or ""
    local buf = vim.api.nvim_create_buf(false, true)
    vim.api.nvim_buf_set_lines(buf, 0, -1, false, vim.split(text, "\n", { plain = true }))
    vim.bo[buf].modifiable = false
    vim.bo[buf].bufhidden = "wipe"
    pcall(vim.api.nvim_buf_set_name, buf, "coppice://" .. pane)
    close()
    vim.cmd("botright split")
    vim.api.nvim_win_set_buf(0, buf)
  end)
end

-- open runs coppice attach on the pane in a terminal buffer, so keys reach
-- the pane. The attach key that leaves ends the terminal job.
local function open()
  local pane = pane_at_cursor()
  if not pane then
    return
  end
  close()
  vim.cmd("tabnew")
  local argv = { config.coppice, "--socket", socket(), "attach", pane }
  if vim.fn.has("nvim-0.11") == 1 then
    vim.fn.jobstart(argv, { term = true })
  else
    vim.fn.termopen(argv)
  end
  vim.cmd("startinsert")
end

-- show opens the floating roster, or moves to it when it is open.
function M.show()
  define_highlights()
  if view.win and vim.api.nvim_win_is_valid(view.win) then
    vim.api.nvim_set_current_win(view.win)
    refresh()
    return
  end
  if not (view.buf and vim.api.nvim_buf_is_valid(view.buf)) then
    view.buf = vim.api.nvim_create_buf(false, true)
    vim.bo[view.buf].bufhidden = "hide"
    local opts = { buffer = view.buf, nowait = true, silent = true }
    vim.keymap.set("n", "<Space>", peek, opts)
    vim.keymap.set("n", "<CR>", open, opts)
    vim.keymap.set("n", "<Esc>", close, opts)
    vim.keymap.set("n", "q", close, opts)
    vim.keymap.set("n", "r", refresh, opts)
  end
  local width = math.max(20, math.min(80, vim.o.columns - 4))
  local height = math.max(3, math.min(20, vim.o.lines - 4))
  view.win = vim.api.nvim_open_win(view.buf, true, {
    relative = "editor",
    width = width,
    height = height,
    row = math.floor((vim.o.lines - height) / 2),
    col = math.floor((vim.o.columns - width) / 2),
    style = "minimal",
    border = "rounded",
    title = " coppice ",
  })
  vim.bo[view.buf].modifiable = true
  vim.api.nvim_buf_set_lines(view.buf, 0, -1, false, { "coppice · reading the roster" })
  vim.bo[view.buf].modifiable = false
  refresh()
end

-- setup takes socket, the server socket path, and coppice, the program that
-- attach runs. Both are optional.
function M.setup(opts)
  opts = opts or {}
  if opts.socket ~= nil then
    config.socket = opts.socket
  end
  if opts.coppice ~= nil then
    config.coppice = opts.coppice
  end
  vim.api.nvim_create_user_command("Coppice", M.show, { desc = "Open the coppice roster" })
end

-- list reads the roster once and calls cb with the lines render gives. It
-- serves a smoke check and a status line.
function M.list(width, cb)
  request("pane.list", {}, function(res, err)
    if not res then
      cb(nil, err)
      return
    end
    local lines = roster.render(roster.group(res.panes or {}), width, vim.fn.strdisplaywidth)
    cb(lines, nil)
  end)
end

return M
