package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/opentalon/opentalon/internal/bundle"
	"github.com/opentalon/opentalon/internal/config"
)

func runChannelCmd(args []string) {
	if len(args) == 0 {
		printChannelUsage()
		os.Exit(1)
	}

	switch args[0] {
	case "install":
		runChannelInstall(args[1:])
	case "list":
		runChannelList(args[1:])
	case "update":
		runChannelUpdate(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown channel subcommand: %s\n\n", args[0])
		printChannelUsage()
		os.Exit(1)
	}
}

func printChannelUsage() {
	fmt.Fprintln(os.Stderr, "Usage: opentalon channel <command>")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Commands:")
	fmt.Fprintln(os.Stderr, "  install   Fetch and build all channels from config")
	fmt.Fprintln(os.Stderr, "  list      Show installed channels with versions")
	fmt.Fprintln(os.Stderr, "  update    Re-resolve ref and rebuild a channel")
}

func loadConfigForCmd(args []string) *config.Config {
	fs := flag.NewFlagSet("channel", flag.ExitOnError)
	configPath := fs.String("config", "", "path to config file")
	_ = fs.Parse(args)

	if *configPath == "" {
		// Try default locations.
		for _, p := range []string{"config.yaml", "~/.opentalon/config.yaml"} {
			if _, err := os.Stat(p); err == nil {
				*configPath = p
				break
			}
		}
	}
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "Error: no config file found. Use -config <path>")
		os.Exit(1)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}
	return cfg
}

// runChannelInstall fetches and builds all channels from config.
func runChannelInstall(args []string) {
	cfg := loadConfigForCmd(args)
	ctx := context.Background()
	dataDir := cfg.State.DataDir

	if len(cfg.Channels) == 0 {
		fmt.Println("No channels configured.")
		return
	}

	for name, ch := range cfg.Channels {
		if !ch.Enabled {
			fmt.Printf("  skip  %s (disabled)\n", name)
			continue
		}
		if ch.GitHub == "" || ch.Ref == "" {
			if ch.Plugin != "" {
				fmt.Printf("  ok    %s (local: %s)\n", name, ch.Plugin)
			} else {
				fmt.Printf("  skip  %s (no github/ref and no path)\n", name)
			}
			continue
		}
		fmt.Printf("  fetch %s (%s@%s)...\n", name, ch.GitHub, ch.Ref)
		path, err := bundle.EnsureChannel(ctx, dataDir, name, ch.GitHub, ch.Ref)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  FAIL  %s: %v\n", name, err)
			continue
		}
		fmt.Printf("  ok    %s -> %s\n", name, path)
	}
	fmt.Println("\nDone. Lock file updated.")
}

// runChannelList shows installed channels from the lock file.
func runChannelList(args []string) {
	cfg := loadConfigForCmd(args)
	dataDir := cfg.State.DataDir

	lock, err := bundle.LoadChannelsLock(dataDir)
	if err != nil {
		fmt.Println("No channels.lock found (no channels installed via GitHub).")
		// Still list local-path channels from config.
		listLocalChannels(cfg)
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tGITHUB\tREF\tRESOLVED\tPATH")

	names := make([]string, 0, len(lock.Channels))
	for name := range lock.Channels {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		entry := lock.Channels[name]
		resolved := entry.Resolved
		if len(resolved) > 12 {
			resolved = resolved[:12]
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", name, entry.GitHub, entry.Ref, resolved, entry.Path)
	}

	// Also list local-path-only channels not in lock.
	for name, ch := range cfg.Channels {
		if _, ok := lock.Channels[name]; ok {
			continue
		}
		if ch.Plugin != "" {
			fmt.Fprintf(w, "%s\t(local)\t-\t-\t%s\n", name, ch.Plugin)
		}
	}

	_ = w.Flush()
}

func listLocalChannels(cfg *config.Config) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSOURCE\tPLUGIN")
	for name, ch := range cfg.Channels {
		if ch.Plugin != "" {
			fmt.Fprintf(w, "%s\tlocal\t%s\n", name, ch.Plugin)
		}
	}
	_ = w.Flush()
}

// runChannelUpdate re-resolves and rebuilds a specific channel.
func runChannelUpdate(args []string) {
	if len(args) == 0 || args[0] == "-config" {
		fmt.Fprintln(os.Stderr, "Usage: opentalon channel update <name> [-config <path>]")
		os.Exit(1)
	}
	channelName := args[0]
	cfg := loadConfigForCmd(args[1:])
	ctx := context.Background()
	dataDir := cfg.State.DataDir

	ch, ok := cfg.Channels[channelName]
	if !ok {
		fmt.Fprintf(os.Stderr, "Channel %q not found in config\n", channelName)
		os.Exit(1)
	}
	if ch.GitHub == "" || ch.Ref == "" {
		fmt.Fprintf(os.Stderr, "Channel %q has no github/ref (local path only)\n", channelName)
		os.Exit(1)
	}

	// Remove from lock to force re-resolve.
	lock, _ := bundle.LoadChannelsLock(dataDir)
	if lock != nil {
		delete(lock.Channels, channelName)
		_ = bundle.SaveChannelsLock(dataDir, lock)
	}

	fmt.Printf("Updating %s (%s@%s)...\n", channelName, ch.GitHub, ch.Ref)
	path, err := bundle.EnsureChannel(ctx, dataDir, channelName, ch.GitHub, ch.Ref)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Updated %s -> %s\n", channelName, path)
}
