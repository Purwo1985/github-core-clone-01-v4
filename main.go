// github-core-clone-01-v4: a GitHub-compatible git forge in Go, standard
// library only. Entry point: read configuration, derive the scenario
// identity, bind an ephemeral port, print READY, serve forever.
package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"time"
)

type config struct {
	token    string
	trialNow int64
}

var cfg config
var trialISO string

var viewer, teammate, stranger user

func main() {
	// Configuration from the environment.
	cfg.token = os.Getenv("MAKEFUN_SHOP_TOKEN")
	nowText := os.Getenv("MAKEFUN_TRIAL_NOW")
	if nowText == "" {
		cfg.trialNow = 1735689600
	} else if n, err := strconv.ParseInt(nowText, 10, 64); err == nil {
		cfg.trialNow = n
	} else {
		cfg.trialNow = 1735689600
	}
	trialISO = time.Unix(cfg.trialNow, 0).UTC().Format("2006-01-02T15:04:05Z")

	// Scenario identity: the viewer login gates every owner check.
	users := generateUsers(12)
	viewer, teammate, stranger = users[0], users[1], users[2]

	gstore = newStore()
	initMCPTools()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "forge: listen: %v\n", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	fmt.Printf("READY port=%d\n", port)
	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		// Disable Nagle: the suite's Node client sends small requests and
		// waits for small responses (ping-pong). With Nagle on, each round
		// trip stalls on the delayed-ACK timer (~2ms/request); with it off
		// the same exchange is ~0.2ms/request.
		if tc, ok := conn.(*net.TCPConn); ok {
			tc.SetNoDelay(true)
		}
		go serveConn(conn)
	}
}
