package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/cockroachdb/pebble"

	"github.com/scrypster/muninndb/internal/index/hnsw"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// runReindex rebuilds vault HNSW graphs from their stored vectors, offline.
//
// WHY: graphs written before the 2026-07-17 insert fixes (back-link
// persistence 8afa3fa, entry-promotion-before-wiring 6286265) are fractured
// on disk -- forward-only edges and per-level-up islands -- so vector search
// reaches only a fraction of each vault. The fixes stop new rot; this command
// repairs what is already stored by re-inserting every vector through the
// fixed path. Vectors themselves are never touched.
//
// Requires exclusive access (server stopped), same as `muninn backup`.
func runReindex(args []string) {
	var dataDir string
	var vaults []string
	all := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--data-dir":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "error: --data-dir requires an argument")
				osExit(1)
				return
			}
			i++
			dataDir = args[i]
		case "--vault":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "error: --vault requires an argument")
				osExit(1)
				return
			}
			i++
			vaults = append(vaults, args[i])
		case "--all":
			all = true
		case "--help", "-h":
			fmt.Println("Usage: muninn reindex (--vault <name> [--vault <name> ...] | --all) [--data-dir <dir>]")
			fmt.Println("Rebuilds vault HNSW graphs from stored vectors. Server must be stopped.")
			return
		default:
			fmt.Fprintf(os.Stderr, "error: unknown flag %q\n", args[i])
			fmt.Fprintln(os.Stderr, "Usage: muninn reindex (--vault <name> ... | --all) [--data-dir <dir>]")
			osExit(1)
			return
		}
	}

	if !all && len(vaults) == 0 {
		fmt.Fprintln(os.Stderr, "error: pass --vault <name> (repeatable) or --all")
		osExit(1)
		return
	}
	if dataDir == "" {
		dataDir = defaultDataDir()
	}

	// Same running-server guard as backup: Pebble is single-writer.
	pidPath := filepath.Join(dataDir, "muninn.pid")
	if pid, err := readPID(pidPath); err == nil && isProcessRunning(pid) {
		fmt.Fprintf(os.Stderr, "error: muninn is running (pid %d) — reindex needs exclusive access; stop it first\n", pid)
		osExit(1)
		return
	}

	db, err := pebble.Open(filepath.Join(dataDir, "pebble"), &pebble.Options{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: open pebble: %v\n", err)
		osExit(1)
		return
	}
	defer db.Close()

	type target struct {
		name string
		ws   [8]byte
	}
	var targets []target

	if all {
		// Enumerate vaults via 0x0E meta keys: 0x0E|ws(8) -> name.
		iter, err := db.NewIter(&pebble.IterOptions{
			LowerBound: []byte{0x0E},
			UpperBound: []byte{0x0F},
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			osExit(1)
			return
		}
		for iter.First(); iter.Valid(); iter.Next() {
			k := iter.Key()
			if len(k) != 9 {
				continue
			}
			var ws [8]byte
			copy(ws[:], k[1:9])
			targets = append(targets, target{name: string(iter.Value()), ws: ws})
		}
		iter.Close()
	} else {
		for _, name := range vaults {
			var ws [8]byte
			if val, closer, err := db.Get(keys.VaultNameIndexKey(name)); err == nil && len(val) == 8 {
				copy(ws[:], val)
				closer.Close()
			} else {
				ws = keys.VaultPrefix(name)
			}
			targets = append(targets, target{name: name, ws: ws})
		}
	}

	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "error: no vaults found")
		osExit(1)
		return
	}

	failed := 0
	for _, tg := range targets {
		start := time.Now()
		n, err := hnsw.RebuildGraph(db, tg.ws)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %-20s ERROR: %v\n", tg.name, err)
			failed++
			continue
		}
		fmt.Printf("  %-20s rebuilt %6d vectors in %s\n", tg.name, n, time.Since(start).Round(time.Millisecond))
	}
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "%d vault(s) failed\n", failed)
		osExit(1)
	}
}
