// monad-failover promotes a synced Monad full node to validator, following
// the official node migration procedure.
//
//	monad-failover [--backup-dir PATH] [--public-ip IP] [--resume]
//	monad-failover --dry-run
package main

import (
	"fmt"
	"os"
	"syscall"

	"github.com/s0urledd/monad-failover-tool/internal/monad"
	"github.com/s0urledd/monad-failover-tool/internal/netinfo"
	"github.com/s0urledd/monad-failover-tool/internal/paths"
	"github.com/s0urledd/monad-failover-tool/internal/promote"
	"github.com/s0urledd/monad-failover-tool/internal/state"
	"github.com/s0urledd/monad-failover-tool/internal/ui"
)

// Version is the tool version printed by --version and in the banner.
const Version = "2.0.0-dev"

func usage(argv0 string) {
	fmt.Printf("monad-failover v%s — promote a synced Monad full node to validator\n", Version)
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Printf("  %s [--backup-dir PATH] [--resume]\n", argv0)
	fmt.Printf("  %s --dry-run\n", argv0)
	fmt.Println()
	fmt.Println("Flags:")
	fmt.Println("  --dry-run     Read-only preflight: run every check, change nothing")
	fmt.Println("  --backup-dir  Directory containing secp-backup / bls-backup key files")
	fmt.Println("                (skips the interactive key-source prompt)")
	fmt.Println("  --public-ip   Use this IPv4 in the name record instead of auto-detection")
	fmt.Println("  --resume      Continue from the last completed step")
	fmt.Println("  --version     Print version and exit")
}

func main() {
	os.Exit(run(os.Args))
}

func run(args []string) int {
	// Secrets (key backups, state) must never be created world-readable,
	// not even for the instant between open() and chmod.
	syscall.Umask(0o077)

	c := ui.New(os.Stdout, os.Stderr, os.Stdin)
	argv0 := args[0]
	opt := promote.Options{Version: Version, Argv0: argv0}
	dryRun := false

	fail := func(err error) int {
		c.Report(err)
		return 1
	}

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--dry-run":
			dryRun = true
		case "--resume":
			opt.Resume = true
		case "--backup-dir":
			i++
			if i >= len(args) || args[i] == "" {
				return fail(ui.Die("--backup-dir requires a value"))
			}
			opt.KeySourceDir = args[i]
		case "--public-ip":
			i++
			if i >= len(args) || !netinfo.ValidIPv4(args[i]) {
				return fail(ui.Die("--public-ip must be a valid IPv4 address"))
			}
			opt.PublicIP = args[i]
		case "--version":
			fmt.Printf("monad-failover v%s\n", Version)
			return 0
		case "-h", "--help", "help":
			usage(argv0)
			return 0
		default:
			return fail(ui.Die("Unknown argument: " + args[i] + ". Run with --help for usage."))
		}
	}

	p, err := paths.FromEnv()
	if err != nil {
		return fail(err)
	}

	if dryRun {
		return promote.DryRun(c, p, opt.KeySourceDir, Version)
	}

	d := state.Resolve(p.Sandbox, p.StateDirOverride)
	r := promote.New(c, p, d, opt)
	proceed, lock, err := r.Prepare()
	if lock != nil {
		defer lock.Release()
	}
	if err != nil {
		return fail(err)
	}
	if !proceed {
		return 0
	}
	monad.Clear(os.Stdout)
	if err := r.Promote(); err != nil {
		return fail(err)
	}
	return 0
}
