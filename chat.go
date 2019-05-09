package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"html"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/esote/graceful"
	"golang.org/x/sys/unix"
)

type msg struct {
	s string
	t string
}

type room struct {
	msgs []msg
	last time.Time
}

var (
	rooms = make(map[string]room)
	lock  = sync.Mutex{}

	validName = regexp.MustCompile("^[a-z]+$")
	validMsg  = regexp.MustCompile(`^[[:print:]]+$`)
)

const (
	maxRoomCount = 50
	maxMsgLen    = 200
	maxMsgsCount = 50
	maxNameLen   = 5

	lifespan = 24 * time.Hour

	welcome_html_start = `<!DOCTYPE html>
<html><body>
	<p>welcome, join existing rooms:</p>`

	welcome_html_end = `
	<p>or make a room: <code>/name_here</code> (max length %d)</p>
	<p>room lifespan: %s (time until lossy room pruning may occur)</p>
	<p>Author: <a href="https://github.com/esote"
		target="_blank">Esote</a>.

		<a href="https://github.com/esote/chat"
		target="_blank">Source code</a>.</p>
</body></html>`

	room_html_start = `<!DOCTYPE html>
<html><body>
	<p>room: %s</p>
	<p><a href="/">&lt; back</a></p>
	<form action="%s" method="post">
		<input type="text" name="msg" required autofocus>
		<input type="submit" value="msg">
	</form>
	<p>chat history:</p><div id="chat">`

	room_html_end = `</div>
	<noscript>
		<p>without JS manually refresh to page to see new messages</p>
	</noscript>
	<script src="/realtime.js" integrity="sha512-ZCdVUxX4G0AmsVIZqa3kzVRr/zjHUj6vWKfDrY7SVAPvPSEBwKXqpgG6pCjyG0aUouSbtjcNUBY5XHB0c36veQ=="></script>
</body></html>`
)

func pruneRooms() {
	for k, v := range rooms {
		if time.Now().Sub(v.last) > lifespan {
			delete(rooms, k)
		}
	}
}

func tryCreateRoom(name string, w http.ResponseWriter) bool {
	if _, ok := rooms[name]; !ok {
		if len(rooms)+1 > maxRoomCount {
			http.Error(w, "too many rooms", http.StatusBadRequest)
			return false
		}

		rooms[name] = room{msgs: make([]msg, 0)}
	}

	return true
}

func get(name string, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", "default-src 'none';"+
		"script-src 'self'; connect-src 'self'")

	pruneRooms()

	if !tryCreateRoom(name, w) {
		return
	}

	fmt.Fprintf(w, room_html_start, name, name)

	for _, m := range rooms[name].msgs {
		fmt.Fprintf(w, "<pre>%s: %s</pre>\n", m.t, m.s)
	}

	fmt.Fprint(w, room_html_end)
}

func patch(name string, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", "default-src 'none';")
	w.Header().Set("Content-Type", "text/plain")

	for _, m := range rooms[name].msgs {
		fmt.Fprintf(w, "<pre>%s: %s</pre>\n", m.t, m.s)
	}
}

func post(name string, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", "default-src 'none';")

	if err := r.ParseForm(); err != nil {
		http.Error(w, "form invalid", http.StatusBadRequest)
		return
	}

	str := r.PostFormValue("msg")

	if len(str) > maxMsgLen {
		http.Error(w, "msg too long", http.StatusBadRequest)
		return
	}

	str = strings.Replace(str, "\r", "", -1)
	str = strings.TrimSpace(str)

	if !validMsg.MatchString(str) {
		http.Error(w, "bad msg", http.StatusBadRequest)
		return
	}

	if str == "" {
		http.Redirect(w, r, name, http.StatusSeeOther)
		return
	}

	str = html.EscapeString(str)

	if !tryCreateRoom(name, w) {
		return
	}

	rm := rooms[name]

	for _, m := range rm.msgs {
		if m.s == str {
			http.Redirect(w, r, name, http.StatusSeeOther)
			return
		}
	}

	rm.last = time.Now()
	rm.msgs = append([]msg{{
		s: str,
		t: rm.last.Format("2006-01-02 15:04"),
	}}, rm.msgs...)

	if len(rm.msgs) > maxMsgsCount {
		rm.msgs = rm.msgs[:maxMsgsCount]
	}

	rooms[name] = rm

	http.Redirect(w, r, name, http.StatusSeeOther)
}

func realtime(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "bad http verb", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/javascript")

	fmt.Fprint(w, `"use strict";
const http = new XMLHttpRequest();
const chat = document.getElementById("chat");
const path = window.location.pathname.split("/").pop();

http.onreadystatechange = function() {
	if (http.readyState == 4 && http.responseText != "") {
		chat.innerHTML = http.responseText;
	}
}

function update() {
	http.open("PATCH", path, true);
	http.send(null);
}

setInterval(update, 1000);
`)
}

func handler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET", "PATCH", "POST":
		break
	default:
		http.Error(w, "bad http verb", http.StatusMethodNotAllowed)
		return
	}

	name := r.URL.Path[1:]

	if name == "" {
		fmt.Fprint(w, welcome_html_start)
		lock.Lock()
		for name := range rooms {
			fmt.Fprintf(w, `<p><a href="/%s">%s &gt;</a></p>`, name,
				name)
		}
		lock.Unlock()
		fmt.Fprintf(w, welcome_html_end, maxNameLen, lifespan)
		return
	} else if len(name) > maxNameLen {
		http.Error(w, "name too long", http.StatusBadRequest)
		return
	} else if !validName.MatchString(name) {
		http.Error(w, "bad name", http.StatusBadRequest)
		return
	}

	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Strict-Transport-Security", "max-age=31536000;"+
		"includeSubDomains;preload")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "deny")
	w.Header().Set("X-XSS-Protection", "1")

	lock.Lock()
	switch r.Method {
	case "GET":
		get(name, w, r)
	case "PATCH":
		patch(name, w, r)
	case "POST":
		post(name, w, r)
	}
	lock.Unlock()
}

func main() {
	// force init of lazy sysctls
	if l, err := net.Listen("tcp", "localhost:0"); err != nil {
		log.Fatal(err)
	} else {
		l.Close()
	}

	if err := unix.Pledge("stdio rpath inet unveil", ""); err != nil {
		log.Fatal(err)
	}

	var (
		cert string
		key  string
	)

	if err := unix.Unveil("/etc/letsencrypt/archive/", "r"); err != nil {
		log.Fatal(err)
	}

	if err := unix.Pledge("stdio rpath inet", ""); err != nil {
		log.Fatal(err)
	}

	flag.StringVar(&cert, "cert", "server.crt", "TLS certificate file")
	flag.StringVar(&key, "key", "server.key", "TLS key file")

	flag.Parse()

	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		CurvePreferences: []tls.CurveID{
			tls.CurveP521,
			tls.X25519,
		},
		PreferServerCipherSuites: true,
		CipherSuites: []uint16{
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		},
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/", handler)
	mux.HandleFunc("/realtime.js", realtime)

	srv := &http.Server{
		Addr:         ":8444",
		Handler:      mux,
		TLSConfig:    cfg,
		TLSNextProto: nil,
	}

	ticker := time.NewTicker(lifespan)
	quit := make(chan struct{})

	go func() {
		for {
			select {
			case <-ticker.C:
				lock.Lock()
				pruneRooms()
				lock.Unlock()
			case <-quit:
				ticker.Stop()
				return
			}
		}
	}()

	graceful.Graceful(srv, func() {
		if err := srv.ListenAndServeTLS(cert, key); err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}, os.Interrupt)
}
