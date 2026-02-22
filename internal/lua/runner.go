package lua

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// PrepareResult is the result of running a Lua content preparer script.
type PrepareResult struct {
	Content   string // transformed content (or message when SendToLLM is false)
	SendToLLM bool   // if false, skip LLM and return Content to the user
}

// RunPrepare runs the Lua script at scriptPath, calling the global prepare(text) function.
// The script must return either a string (new content, SendToLLM true) or a table
// with send_to_llm (bool) and message (string) to block and return a message.
// Scripts can use os.getenv for environment variables (e.g. HELLO_WORLD_PROMPT_FRAGMENT).
func RunPrepare(scriptPath, text string) (*PrepareResult, error) {
	lState := lua.NewState()
	defer lState.Close()

	// Allow os.getenv so scripts can read env vars (e.g. HELLO_WORLD_PROMPT_FRAGMENT).
	lState.PreloadModule("os", osModuleLoader)

	absPath, err := filepath.Abs(scriptPath)
	if err != nil {
		return nil, fmt.Errorf("script path: %w", err)
	}
	if err := lState.DoFile(absPath); err != nil {
		return nil, fmt.Errorf("load script: %w", err)
	}

	fn := lState.GetGlobal("prepare")
	if fn.Type() == lua.LTNil {
		return nil, fmt.Errorf("script must define global function prepare(text)")
	}
	if fn.Type() != lua.LTFunction {
		return nil, fmt.Errorf("prepare must be a function, got %s", fn.Type().String())
	}

	lState.Push(fn)
	lState.Push(lua.LString(text))
	if err := lState.PCall(1, 1, nil); err != nil {
		return nil, fmt.Errorf("prepare(): %w", err)
	}

	ret := lState.Get(-1)
	lState.Pop(1)

	switch ret.Type() {
	case lua.LTString:
		return &PrepareResult{Content: ret.String(), SendToLLM: true}, nil
	case lua.LTTable:
		tbl := ret.(*lua.LTable)
		sendToLLM := true
		var message string
		tbl.ForEach(func(k, v lua.LValue) {
			if k.String() == "send_to_llm" && v.Type() == lua.LTBool {
				sendToLLM = v.(lua.LBool) == lua.LTrue
			}
			if k.String() == "message" && v.Type() == lua.LTString {
				message = v.String()
			}
		})
		return &PrepareResult{Content: message, SendToLLM: sendToLLM}, nil
	default:
		return nil, fmt.Errorf("prepare() must return string or table { send_to_llm, message }, got %s", ret.Type().String())
	}
}

// osModuleLoader provides a minimal os module: getenv and time (for math.randomseed).
func osModuleLoader(lState *lua.LState) int {
	mod := lState.NewTable()
	lState.SetField(mod, "getenv", lState.NewFunction(func(ls *lua.LState) int {
		key := ls.CheckString(1)
		val := os.Getenv(key)
		ls.Push(lua.LString(val))
		return 1
	}))
	lState.SetField(mod, "time", lState.NewFunction(func(ls *lua.LState) int {
		ls.Push(lua.LNumber(time.Now().Unix()))
		return 1
	}))
	lState.Push(mod)
	return 1
}
