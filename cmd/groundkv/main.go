package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/flaneur2020/groundkv/engine"
	"github.com/flaneur2020/groundkv/engine/verified"
)

func openDB(impl, path string) (*engine.DB, error) {
	switch impl {
	case "go":
		return engine.Open(path, nil)
	case "verified", "dafny":
		return verified.Open(path, nil)
	default:
		return nil, fmt.Errorf("unknown engine %q (expected \"go\" or \"verified\")", impl)
	}
}

func main() {
	dbPath := flag.String("db", "groundkv.db", "path to database file")
	impl := flag.String("engine", "go", "storage kernel: go (pure Go) or verified (Dafny-verified Go)")
	flag.Parse()

	args := flag.Args()
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}

	db, err := openDB(*impl, *dbPath)
	if err != nil {
		fatalf("open %s (%s): %v", *dbPath, *impl, err)
	}
	defer db.Close()

	switch args[0] {
	case "put":
		if len(args) != 3 {
			usage()
			os.Exit(2)
		}
		if err := db.Put([]byte(args[1]), []byte(args[2])); err != nil {
			fatalf("put: %v", err)
		}
		fmt.Printf("OK (txid=%d, root=%d)\n", db.Meta().TxID, db.Meta().Root)

	case "get":
		if len(args) != 2 {
			usage()
			os.Exit(2)
		}
		val, err := db.Get([]byte(args[1]))
		if errors.Is(err, engine.ErrNotFound) {
			fmt.Println("NOT_FOUND")
			return
		}
		if err != nil {
			fatalf("get: %v", err)
		}
		fmt.Println(string(val))

	case "del":
		if len(args) != 2 {
			usage()
			os.Exit(2)
		}
		if err := db.Delete([]byte(args[1])); err != nil {
			fatalf("del: %v", err)
		}
		fmt.Printf("OK (txid=%d, root=%d)\n", db.Meta().TxID, db.Meta().Root)

	case "scan":
		err := db.View(func(tx *engine.Tx) error {
			return tx.Scan(nil, nil, func(k, v []byte) bool {
				fmt.Printf("%s=%s\n", k, v)
				return true
			})
		})
		if err != nil {
			fatalf("scan: %v", err)
		}

	case "meta":
		m0, m1, err := db.DualMeta()
		if err != nil {
			fatalf("meta: %v", err)
		}
		active := db.Meta()
		fmt.Printf("Active: txid=%d root=%d highWater=%d pageSize=%d\n", active.TxID, active.Root, active.HighWater, active.PageSize)
		fmt.Printf("Meta 0: txid=%d root=%d highWater=%d\n", m0.TxID, m0.Root, m0.HighWater)
		fmt.Printf("Meta 1: txid=%d root=%d highWater=%d\n", m1.TxID, m1.Root, m1.HighWater)

	case "verify":
		if err := db.Validate(); err != nil {
			fatalf("verify failed: %v", err)
		}
		fmt.Printf("VALID (txid=%d, root=%d, pages=%d)\n", db.Meta().TxID, db.Meta().Root, db.Meta().HighWater)

	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage: groundkv [-db path] [-engine go|verified] <put k v | get k | del k | scan | meta | verify>\n")
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
