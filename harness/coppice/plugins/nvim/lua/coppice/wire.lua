-- Pure helpers for the socket protocol. They hold no vim call, so the spec
-- can run them in any Lua 5.1.

local M = {}

-- reply reads one decoded reply. null is the value the JSON decoder gives
-- for JSON null, vim.NIL in nvim. It returns the result as a table, or nil
-- and a message.
function M.reply(msg, null)
  if msg.ok == true then
    local res = msg.result
    if res == nil or res == null or type(res) ~= "table" then
      res = {}
    end
    return res, nil
  end
  local err = msg.error
  if type(err) == "table" and err ~= null and type(err.message) == "string" then
    return nil, err.message
  end
  return nil, "the server refused"
end

return M
