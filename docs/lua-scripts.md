# Lua scripts

Lua plugins run as **content preparers**: they run before the first LLM call and can transform the user message or block the request. No compiled binary is required. The core loads your script and calls a global `prepare(text)` function.

**Contract:**

- **Return a string** — the new content is sent to the LLM.
- **Return a table** `{ send_to_llm = false, message = "..." }` — the LLM is skipped and the user sees `message`.

Scripts can use `os.getenv()` and `os.time()` (the runner exposes a minimal `os` module). See [internal/lua/runner.go](../internal/lua/runner.go) for details.

## Hello-world example

This example matches the behavior of the [Go hello-world plugin](https://github.com/opentalon/hellow-world-plugin): if the user says "hello", the script adds " world" and a random prompt fragment; otherwise it blocks with a guard message.

The script lives in the repo as [scripts/hello-world.lua](../scripts/hello-world.lua). You can reuse it as-is or copy and adapt it.

```lua
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
```

## Config

**Local scripts:** set `lua.scripts_dir` to a directory containing `.lua` files (e.g. `./scripts`). Each file is exposed as a plugin by basename without extension (e.g. `hello-world.lua` → `lua:hello-world`).

**Download from GitHub:** set `lua.plugins` to a list of plugin names and optionally `lua.default_github` and `lua.default_ref` so the core clones the repo and runs scripts from it. You can also specify per-plugin `github` and `ref`. See [config.example.yaml](../config.example.yaml) for the commented `lua` block.

**Wire as content preparer:** in `orchestrator.content_preparers`, use `plugin: lua:hello-world` (and `action: prepare`, `arg_key: text` if you want to match the Go convention). The core will run the script and pass the current message as `text`.

Example:

```yaml
lua:
  scripts_dir: ./scripts

orchestrator:
  content_preparers:
    - plugin: lua:hello-world
      action: prepare
      arg_key: text
```
