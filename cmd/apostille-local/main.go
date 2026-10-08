package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ifandonlyif-io/iff-apostille-local/internal/config"
	"github.com/ifandonlyif-io/iff-apostille-local/internal/evidence"
	"github.com/ifandonlyif-io/iff-apostille-local/internal/gateway"
)

func run() error {
	path := flag.String("config", "config.json", "configuration path")
	test := flag.Bool("insecure-test", false, "allow HTTP on loopback ONLY for synthetic tests")
	flag.Parse()
	c, err := config.Load(*path)
	if err != nil {
		return err
	}
	if *test {
		host, _, _ := net.SplitHostPort(c.Listen)
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("insecure_requires_loopback")
		}
	} else if c.TLSCertFile == "" || c.TLSKeyFile == "" {
		return fmt.Errorf("tls_required")
	}
	var store *evidence.Store
	if c.Evidence.Directory != "" {
		store, err = evidence.New(c.Evidence.Directory, c.Evidence.KeyFile, c.Evidence.AgentID)
		if err != nil {
			return fmt.Errorf("evidence_initialization_failed")
		}
		defer store.Close()
	}
	g, err := gateway.New(c, store)
	if err != nil {
		return err
	}
	defer g.Close()
	srv := &http.Server{Addr: c.Listen, Handler: g, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13}, ErrorLog: log.New(io.Discard, "", 0)}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() {
		if *test {
			errc <- srv.ListenAndServe()
		} else {
			errc <- srv.ListenAndServeTLS(c.TLSCertFile, c.TLSKeyFile)
		}
	}()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			g.Drain()
			deadline, cancel := context.WithTimeout(context.Background(), time.Duration(c.TimeoutSeconds+5)*time.Second)
			defer cancel()
			if srv.Shutdown(deadline) != nil {
				_ = srv.Close()
			}
			return nil
		case e := <-errc:
			if e == http.ErrServerClosed {
				return nil
			}
			return fmt.Errorf("server_unavailable")
		case <-ticker.C:
			if store != nil {
				_ = store.Purge()
			}
		}
	}
}
func main() {
	if run() != nil {
		fmt.Fprintln(os.Stderr, "apostille_local_start_failed")
		os.Exit(1)
	}
}
