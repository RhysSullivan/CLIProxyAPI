// Stand-in for cli-proxy-api-plus in deploy rehearsals. It answers the same
// local endpoints the deploy script checks. Behaviour is fixed at build time,
// so each mode is a distinct binary with its own sha.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	Version    = "v0"
	Mode       = "good" // good | crash | badreq
	StartDelay = "0"    // seconds to wait before listening (slow start)
)

// Config watch, modelled on the engine: the config is read once at start and
// the watch is added only after the server already answers, so a write in
// that window is missed until the next write. /standin/config reports the
// sha of the config the process has loaded.
var (
	cfgMu     sync.Mutex
	loadedSha string
	reloads   int
)

func fileSha(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return "unreadable"
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func watchConfig(path string) {
	fd, err := syscall.InotifyInit()
	if err != nil {
		fmt.Println("inotify:", err)
		return
	}
	if _, err := syscall.InotifyAddWatch(fd, filepath.Dir(path), syscall.IN_CLOSE_WRITE|syscall.IN_MOVED_TO|syscall.IN_CREATE); err != nil {
		fmt.Println("inotify watch:", err)
		return
	}
	fmt.Println("stand-in: config watch active")
	buf := make([]byte, 64*1024)
	for {
		n, err := syscall.Read(fd, buf)
		if err != nil || n <= 0 {
			return
		}
		for off := 0; off+syscall.SizeofInotifyEvent <= n; {
			ev := (*syscall.InotifyEvent)(unsafe.Pointer(&buf[off]))
			nameBytes := buf[off+syscall.SizeofInotifyEvent : off+syscall.SizeofInotifyEvent+int(ev.Len)]
			name := strings.TrimRight(string(nameBytes), "\x00")
			off += syscall.SizeofInotifyEvent + int(ev.Len)
			if name == filepath.Base(path) {
				time.Sleep(100 * time.Millisecond)
				sha := fileSha(path)
				cfgMu.Lock()
				if sha != loadedSha {
					loadedSha = sha
					reloads++
					fmt.Println("stand-in: config reloaded")
				}
				cfgMu.Unlock()
			}
		}
	}
}

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
	if d, _ := strconv.Atoi(StartDelay); d > 0 {
		fmt.Printf("stand-in: slow start, %ds\n", d)
		time.Sleep(time.Duration(d) * time.Second)
	}
	port, key := "", ""
	loadedSha = fileSha(*cfg)
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
	http.HandleFunc("/standin/config", func(w http.ResponseWriter, r *http.Request) {
		cfgMu.Lock()
		defer cfgMu.Unlock()
		fmt.Fprintf(w, "%s %d\n", loadedSha, reloads)
	})
	go func() { time.Sleep(5 * time.Second); watchConfig(*cfg) }()
	fmt.Println("API server started successfully on: 127.0.0.1:" + port)
	if err := http.ListenAndServe("127.0.0.1:"+port, nil); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
