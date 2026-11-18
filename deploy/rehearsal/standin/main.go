// Stand-in for cli-proxy-api-plus in deploy rehearsals. It answers the same
// local endpoints the deploy script checks. Behaviour is fixed at build time,
// so each mode is a distinct binary with its own sha.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
)

var (
	Version = "v0"
	Mode    = "good" // good | crash | badreq
)

func main() {
	cfg := flag.String("config", "", "config path")
	help := flag.Bool("h", false, "help")
	flag.Parse()
	fmt.Printf("CLIProxyAPI Version: %s, Commit: standin, BuiltAt: rehearsal\n", Version)
	if *help {
		return
	}
	if Mode == "crash" {
		fmt.Println("stand-in: crashing at start")
		os.Exit(1)
	}
	port, key := "", ""
	f, err := os.Open(*cfg)
	if err != nil {
		fmt.Println("no config:", err)
		os.Exit(1)
	}
	sc := bufio.NewScanner(f)
	inKeys := false
	for sc.Scan() {
		l := sc.Text()
		switch {
		case strings.HasPrefix(l, "port:"):
			port = strings.TrimSpace(strings.TrimPrefix(l, "port:"))
		case strings.HasPrefix(l, "api-keys:"):
			inKeys = true
		case inKeys && strings.HasPrefix(strings.TrimSpace(l), "-") && key == "":
			key = strings.Trim(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "-")), `"'`)
		case inKeys && len(l) > 0 && l[0] != ' ':
			inKeys = false
		}
	}
	f.Close()
	authed := func(r *http.Request) bool {
		return r.Header.Get("Authorization") == "Bearer "+key || r.Header.Get("x-api-key") == key
	}
	const secret = `{"error":{"message":"upstream said no","leak":"sk-STANDIN-SECRET-must-not-be-logged","authorization":"Bearer sk-STANDIN-SECRET-must-not-be-logged"}}`
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"status":"ok"}`)) })
	http.HandleFunc("/pool/usage", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) })
	http.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(`{"data":[{"id":"claude-haiku-4-5-20251001"},{"id":"gpt-5.6-luna"}]}`))
	})
	http.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			w.WriteHeader(401)
			return
		}
		if Mode == "badreq" {
			w.WriteHeader(502)
			w.Write([]byte(secret))
			return
		}
		w.Write([]byte(`{"model":"claude-haiku-4-5-20251001","content":[{"type":"text","text":"OK"}]}`))
	})
	http.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			w.WriteHeader(401)
			return
		}
		if Mode == "badreq" {
			w.WriteHeader(502)
			w.Write([]byte(secret))
			return
		}
		w.Write([]byte(`{"model":"gpt-5.6-luna","status":"completed","output":[{"type":"message"}]}`))
	})
	fmt.Println("API server started successfully on: 127.0.0.1:" + port)
	if err := http.ListenAndServe("127.0.0.1:"+port, nil); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
