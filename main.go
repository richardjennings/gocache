// Command gocache is a shared Go build cache for CI builders on one host.
//
//	gocache client  the program that the go command starts through
//	                GOCACHEPROG="gocache client". GOCACHE_SERVER gives the
//	                base URL of the server, for example http://HOST:5100.
//	                Without it, nothing is kept after the go command exits.
//	gocache server  the HTTP server that keeps the cache on disk. It has no
//	                authentication, so give -addr an address that only the
//	                builders can reach.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/richardjennings/gocache/internal/client"
	"github.com/richardjennings/gocache/internal/server"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "client":
		if err := client.Run(os.Stdin, os.Stdout, os.Stderr, os.Getenv("GOCACHE_SERVER")); err != nil {
			fmt.Fprintln(os.Stderr, "gocache client:", err)
			os.Exit(1)
		}
	case "server":
		serve(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: gocache client | gocache server [flags]")
	os.Exit(2)
}

func serve(args []string) {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:5100", "address to listen on")
	dir := fs.String("dir", "/var/lib/gocache", "data directory")
	maxBytes := fs.Int64("max-bytes", 50<<30, "size above which a sweep removes the least recently used files")
	every := fs.Duration("sweep", 10*time.Minute, "time between sweeps")
	fs.Parse(args)

	s, err := server.New(*dir, *maxBytes)
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		for range time.Tick(*every) {
			n, err := s.Sweep()
			if err != nil {
				log.Printf("sweep: %v", err)
			} else if n > 0 {
				log.Printf("sweep: removed %d files", n)
			}
		}
	}()
	log.Printf("serving %s on %s, sweeping above %d bytes", *dir, *addr, *maxBytes)
	srv := &http.Server{Addr: *addr, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
