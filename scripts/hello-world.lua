-- Hello World content preparer (Lua port of https://github.com/opentalon/hellow-world-plugin)
-- If user text contains "hello", add " world" and a random prompt fragment; otherwise block with a guard message.
-- Environment: HELLO_WORLD_PROMPT_FRAGMENT (optional) overrides the random fragment.

local prompt_fragments = {
  "In which language was the first Hello, World! program printed?",
  "In which year was the first Hello, World! program ever printed?",
  "In which year was the first Hello, World! printed in C?",
  "In which year was the first Hello, World! printed in Java?",
  "In which year was the first Hello, World! printed in Python?",
  "In which year was the first Hello, World! printed in Ruby?",
  "In which year was the first Hello, World! printed in Go?",
  "In which year was the first Hello, World! printed in JavaScript?",
  "In which year was the first Hello, World! printed in Rust?",
  "In which year was the first Hello, World! printed in PHP?",
  "What is the most printed programming language for Hello, World!?",
}

local function trim(s)
  if type(s) ~= "string" then return "" end
  return s:match("^%s*(.-)%s*$") or s
end

local function pick_fragment()
  local fixed = os.getenv("HELLO_WORLD_PROMPT_FRAGMENT")
  if fixed and fixed ~= "" then
    return fixed
  end
  math.randomseed(os.time())
  return prompt_fragments[math.random(1, #prompt_fragments)]
end

function prepare(text)
  local trimmed = trim(text)
  local lower = string.lower(trimmed)
  if not string.find(lower, "hello") then
    return {
      send_to_llm = false,
      message = "Plugin only accepts send hello to LLM. All another knows human brain.",
    }
  end
  -- Add " world" only if not already ending with "world"
  if not string.find(lower, "world%s*$") then
    trimmed = trimmed .. " world"
  end
  local question = pick_fragment()
  return trimmed .. "\n\n" .. question
end
