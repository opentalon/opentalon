-- format-response.lua: Post-LLM response formatter.
-- Converts standard Markdown to the target channel's native format.
-- Cheap/small models often ignore system prompt formatting hints and output
-- standard Markdown regardless — this script fixes it deterministically.
--
-- Usage in config.yaml:
--   orchestrator:
--     response_formatters:
--       - plugin: "lua:format-response"
--
-- Supports: slack, teams, whatsapp, telegram, html, text.
-- No-op for: markdown, discord, or empty (already standard Markdown).

-- Split text into segments: protected (inside code blocks/inline code) and
-- unprotected (regular text that can be transformed).
local function split_segments(text)
  local segments = {}
  local pos = 1
  local len = #text

  while pos <= len do
    -- Look for fenced code block (```)
    local fence_start = text:find("```", pos, true)
    -- Look for inline code (`)
    local inline_start = text:find("`", pos, true)

    -- Pick whichever comes first
    if fence_start and (not inline_start or fence_start <= inline_start) then
      -- Add unprotected segment before the fence
      if fence_start > pos then
        table.insert(segments, { text = text:sub(pos, fence_start - 1), protected = false })
      end
      -- Find closing fence
      local fence_end = text:find("```", fence_start + 3, true)
      if fence_end then
        table.insert(segments, { text = text:sub(fence_start, fence_end + 2), protected = true })
        pos = fence_end + 3
      else
        -- No closing fence: rest of text is protected
        table.insert(segments, { text = text:sub(fence_start), protected = true })
        pos = len + 1
      end
    elseif inline_start then
      -- Check if it's actually a fence start (handled above)
      if text:sub(inline_start, inline_start + 2) == "```" then
        -- Should have been caught above; safety fallback
        if inline_start > pos then
          table.insert(segments, { text = text:sub(pos, inline_start - 1), protected = false })
        end
        local fence_end = text:find("```", inline_start + 3, true)
        if fence_end then
          table.insert(segments, { text = text:sub(inline_start, fence_end + 2), protected = true })
          pos = fence_end + 3
        else
          table.insert(segments, { text = text:sub(inline_start), protected = true })
          pos = len + 1
        end
      else
        -- Inline code
        if inline_start > pos then
          table.insert(segments, { text = text:sub(pos, inline_start - 1), protected = false })
        end
        local inline_end = text:find("`", inline_start + 1, true)
        if inline_end then
          table.insert(segments, { text = text:sub(inline_start, inline_end), protected = true })
          pos = inline_end + 1
        else
          -- No closing backtick: treat rest as unprotected
          table.insert(segments, { text = text:sub(inline_start), protected = false })
          pos = len + 1
        end
      end
    else
      -- No more code markers
      table.insert(segments, { text = text:sub(pos), protected = false })
      pos = len + 1
    end
  end

  return segments
end

-- Apply a transform function to unprotected segments only, then reassemble.
local function apply_transform(text, transform_fn)
  local segments = split_segments(text)
  local parts = {}
  for _, seg in ipairs(segments) do
    if seg.protected then
      table.insert(parts, seg.text)
    else
      table.insert(parts, transform_fn(seg.text))
    end
  end
  return table.concat(parts)
end

-- Slack mrkdwn: **bold** -> *bold*, ## Heading -> *Heading*
local function format_slack(s)
  -- Headings: # ... or ## ... -> *...* (placeholder to protect from italic pass)
  s = s:gsub("(\n)#+%s+([^\n]+)", function(nl, heading)
    heading = heading:gsub("%*%*(.-)%*%*", "%1")
    return nl .. "\0HEAD\0" .. heading .. "\0HEAD\0"
  end)
  if s:sub(1, 1) == "#" then
    s = s:gsub("^#+%s+([^\n]+)", function(heading)
      heading = heading:gsub("%*%*(.-)%*%*", "%1")
      return "\0HEAD\0" .. heading .. "\0HEAD\0"
    end)
  end
  -- Italic: *text* -> _text_ (Slack italic is _text_, not *text*)
  -- Protect **bold** first, convert *italic*, then restore bold and headings.
  s = s:gsub("%*%*(.-)%*%*", "\0BOLD\0%1\0BOLD\0")
  s = s:gsub("%*(.-)%*", "_%1_")
  s = s:gsub("\0BOLD\0(.-)\0BOLD\0", "*%1*")
  s = s:gsub("\0HEAD\0(.-)\0HEAD\0", "*%1*")
  -- Bold underscore: __text__ -> _text_
  s = s:gsub("__(.-)__", "_%1_")
  return s
end

-- Teams: ## Heading -> **Heading** (Teams renders bold but not # headings well)
local function format_teams(s)
  s = s:gsub("(\n)#+%s+([^\n]+)", function(nl, heading)
    heading = heading:gsub("%*%*(.-)%*%*", "%1")
    return nl .. "**" .. heading .. "**"
  end)
  if s:sub(1, 1) == "#" then
    s = s:gsub("^#+%s+([^\n]+)", function(heading)
      heading = heading:gsub("%*%*(.-)%*%*", "%1")
      return "**" .. heading .. "**"
    end)
  end
  return s
end

-- WhatsApp: **text** -> *text*, ## Heading -> *Heading*
local function format_whatsapp(s)
  -- Headings
  s = s:gsub("(\n)#+%s+([^\n]+)", function(nl, heading)
    heading = heading:gsub("%*%*(.-)%*%*", "%1")
    return nl .. "*" .. heading .. "*"
  end)
  if s:sub(1, 1) == "#" then
    s = s:gsub("^#+%s+([^\n]+)", function(heading)
      heading = heading:gsub("%*%*(.-)%*%*", "%1")
      return "*" .. heading .. "*"
    end)
  end
  -- Bold: **text** -> *text*
  s = s:gsub("%*%*(.-)%*%*", "*%1*")
  return s
end

-- Plain text: strip all formatting
local function format_text(s)
  -- Headings
  s = s:gsub("(\n)#+%s+([^\n]+)", "%1%2")
  if s:sub(1, 1) == "#" then
    s = s:gsub("^#+%s+([^\n]+)", "%1")
  end
  -- Bold
  s = s:gsub("%*%*(.-)%*%*", "%1")
  -- Italic
  s = s:gsub("%*(.-)%*", "%1")
  -- Bold/italic underscore
  s = s:gsub("__(.-)__", "%1")
  s = s:gsub("_(.-)_", "%1")
  -- Strikethrough
  s = s:gsub("~~(.-)~~", "%1")
  -- Links: [text](url) -> text (url)
  s = s:gsub("%[(.-)%]%((.-)%)", "%1 (%2)")
  return s
end

-- HTML: **text** -> <b>text</b>, *text* -> <i>text</i>, etc.
local function format_html(s)
  -- Headings: # Heading -> <b>Heading</b>
  s = s:gsub("(\n)#+%s+([^\n]+)", function(nl, heading)
    heading = heading:gsub("%*%*(.-)%*%*", "%1")
    return nl .. "<b>" .. heading .. "</b>"
  end)
  if s:sub(1, 1) == "#" then
    s = s:gsub("^#+%s+([^\n]+)", function(heading)
      heading = heading:gsub("%*%*(.-)%*%*", "%1")
      return "<b>" .. heading .. "</b>"
    end)
  end
  -- Bold: **text** -> <b>text</b>
  s = s:gsub("%*%*(.-)%*%*", "<b>%1</b>")
  -- Italic: *text* -> <i>text</i>
  s = s:gsub("%*(.-)%*", "<i>%1</i>")
  return s
end

-- Telegram: same as HTML (Telegram supports <b>, <i>, <code>, <pre>)
local function format_telegram(s)
  return format_html(s)
end

-- Format [tool_call] and [tool_result] debug blocks that appear when
-- orchestrator.show_tool_calls is enabled. Returns (formatted_debug, rest)
-- where rest is the response body after the "---" separator.
-- If no debug blocks are found, returns (nil, text).
local function split_tool_debug(text)
  -- The orchestrator prepends: tool_call/result lines + "\n\n---\n\n" + response
  local sep = text:find("\n\n%-%-%-%s*\n\n")
  if not sep then return nil, text end

  local debug_part = text:sub(1, sep - 1)
  local rest = text:sub(text:find("\n", sep + 3) or sep + 5)
  rest = rest:gsub("^%s+", "")

  -- Only treat as debug if it actually contains [tool_call] lines
  if not debug_part:find("%[tool_call%]") then
    return nil, text
  end
  return debug_part, rest
end

local function format_tool_debug_slack(debug_part)
  local lines = {}
  for line in debug_part:gmatch("[^\n]+") do
    -- [tool_call] plugin.action(args) -> blockquote with code
    local tool = line:match("^%[tool_call%]%s*(.+)$")
    if tool then
      table.insert(lines, "> :wrench:  `" .. tool .. "`")
    else
      -- [tool_result] content or [tool_result] error: msg
      local err = line:match("^%[tool_result%] error:%s*(.+)$")
      if err then
        table.insert(lines, "> :x:  " .. err)
      else
        local result = line:match("^%[tool_result%]%s*(.+)$")
        if result then
          -- Truncate long results for readability
          if #result > 200 then
            result = result:sub(1, 200) .. "..."
          end
          table.insert(lines, "> :white_check_mark:  `" .. result .. "`")
        else
          table.insert(lines, "> " .. line)
        end
      end
    end
  end
  return table.concat(lines, "\n")
end

local function format_tool_debug_html(debug_part)
  local lines = {}
  for line in debug_part:gmatch("[^\n]+") do
    local tool = line:match("^%[tool_call%]%s*(.+)$")
    if tool then
      table.insert(lines, "<b>Tool:</b> <code>" .. tool .. "</code>")
    else
      local err = line:match("^%[tool_result%] error:%s*(.+)$")
      if err then
        table.insert(lines, "<b>Error:</b> " .. err)
      else
        local result = line:match("^%[tool_result%]%s*(.+)$")
        if result then
          if #result > 200 then
            result = result:sub(1, 200) .. "..."
          end
          table.insert(lines, "<b>Result:</b> <code>" .. result .. "</code>")
        else
          table.insert(lines, line)
        end
      end
    end
  end
  return "<blockquote>" .. table.concat(lines, "\n") .. "</blockquote>"
end

local function format_tool_debug_text(debug_part)
  local lines = {}
  for line in debug_part:gmatch("[^\n]+") do
    local tool = line:match("^%[tool_call%]%s*(.+)$")
    if tool then
      table.insert(lines, "  Tool: " .. tool)
    else
      local err = line:match("^%[tool_result%] error:%s*(.+)$")
      if err then
        table.insert(lines, "  Error: " .. err)
      else
        local result = line:match("^%[tool_result%]%s*(.+)$")
        if result then
          if #result > 200 then
            result = result:sub(1, 200) .. "..."
          end
          table.insert(lines, "  Result: " .. result)
        else
          table.insert(lines, "  " .. line)
        end
      end
    end
  end
  return table.concat(lines, "\n")
end

local function format_tool_debug_markdown(debug_part)
  local lines = {}
  for line in debug_part:gmatch("[^\n]+") do
    local tool = line:match("^%[tool_call%]%s*(.+)$")
    if tool then
      table.insert(lines, "> **Tool:** `" .. tool .. "`")
    else
      local err = line:match("^%[tool_result%] error:%s*(.+)$")
      if err then
        table.insert(lines, "> **Error:** " .. err)
      else
        local result = line:match("^%[tool_result%]%s*(.+)$")
        if result then
          if #result > 200 then
            result = result:sub(1, 200) .. "..."
          end
          table.insert(lines, "> **Result:** `" .. result .. "`")
        else
          table.insert(lines, "> " .. line)
        end
      end
    end
  end
  return table.concat(lines, "\n")
end

-- Main entry point called by OpenTalon's response formatter pipeline.
-- text: the LLM response
-- response_format: channel format string ("slack", "teams", "text", etc.)
function format(text, response_format)
  if not text or text == "" then
    return text
  end

  -- Split off debug tool_call blocks (from show_tool_calls) before formatting.
  local debug_part, rest = split_tool_debug(text)

  local formatted
  if response_format == "slack" then
    formatted = apply_transform(rest, format_slack)
  elseif response_format == "teams" then
    formatted = apply_transform(rest, format_teams)
  elseif response_format == "whatsapp" then
    formatted = apply_transform(rest, format_whatsapp)
  elseif response_format == "text" then
    -- For plain text, also strip code delimiters from protected segments
    local segments = split_segments(rest)
    local parts = {}
    for _, seg in ipairs(segments) do
      if seg.protected then
        local inner = seg.text
        inner = inner:gsub("^```[^\n]*\n?", "")
        inner = inner:gsub("\n?```$", "")
        inner = inner:gsub("^`", "")
        inner = inner:gsub("`$", "")
        table.insert(parts, inner)
      else
        table.insert(parts, format_text(seg.text))
      end
    end
    formatted = table.concat(parts)
  elseif response_format == "html" then
    formatted = apply_transform(rest, format_html)
  elseif response_format == "telegram" then
    formatted = apply_transform(rest, format_telegram)
  else
    -- markdown, discord, empty: no-op on response body
    formatted = rest
  end

  -- If no debug blocks, return the formatted response as-is.
  if not debug_part then
    return formatted
  end

  -- Format the debug blocks for the target channel.
  local debug_formatted
  if response_format == "slack" then
    debug_formatted = format_tool_debug_slack(debug_part)
  elseif response_format == "html" or response_format == "telegram" then
    debug_formatted = format_tool_debug_html(debug_part)
  elseif response_format == "text" then
    debug_formatted = format_tool_debug_text(debug_part)
  else
    -- markdown, discord, teams, whatsapp: use markdown blockquotes
    debug_formatted = format_tool_debug_markdown(debug_part)
  end

  return debug_formatted .. "\n\n" .. formatted
end
