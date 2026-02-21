package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/opentalon/opentalon/internal/config"
	"github.com/opentalon/opentalon/internal/version"
)

func main() {
	configPath := flag.String("config", "", "path to config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Get())
		os.Exit(0)
	}

	fmt.Println(version.Get())

	if *configPath != "" {
		cfg, err := config.Load(*configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Config loaded: %d providers, %d catalog entries\n",
			len(cfg.Models.Providers), len(cfg.Models.Catalog))
	}
}
