# Slash commands

With the **[opentalon-commands](https://github.com/opentalon/opentalon-commands)** plugin (included in `config.example.yaml`), you can run built-in commands by typing a message that starts with `/`:

| Command | Description |
|--------|-------------|
| `/install skill <url> [ref]` | Install a skill from a GitHub URL; available immediately, no restart |
| `/show config` | Show current config (secrets redacted) |
| `/commands` or `/help` | List available slash commands |
| `/set prompt <text>` | Set the editable runtime prompt (applies to the next message) |
| `/clear` or `/new` | Clear the current conversation session |

The plugin runs as the first **content preparer**: when your message starts with `/`, it parses the command and the core runs the built-in **opentalon** executor (install skill, show config, etc.) without calling the LLM. Enable it in config with `github: "opentalon/opentalon-commands"` and `ref: "master"`; see [config.example.yaml](../config.example.yaml) and the [plugin README](https://github.com/opentalon/opentalon-commands#readme).
