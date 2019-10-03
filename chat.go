package main

import (
	"fmt"
	"html"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/esote/graceful"
	"github.com/esote/openshim2"
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

	validName = regexp.MustCompile("^[a-z]*$")
	validMsg  = regexp.MustCompile(`^[[:print:]]+$`)
)

const (
	maxRoomCount = 50
	maxMsgLen    = 80
	maxMsgsCount = 50
	maxNameLen   = 5

	lifespan = 24 * time.Hour

	welcomeStart = `<!DOCTYPE html>
<html lang="en">
<head>
	<meta charset="utf-8">
	<meta name="viewport"
		content="width=device-width, initial-scale=1, shrink-to-fit=no">
	<meta name="author" content="Esote">
	<meta name="description" content="Room-based chat server">
	<title>Room-based chat server</title>
</head>
<body>
	<p>welcome, join existing rooms:</p>`

	welcomeEnd = `
	<form action="/" method="get" autocomplete="off">
		<label>or make a room: </label>
		<input type="text" name="name" required placeholder="name_here"
			maxlength="%d" pattern="%s" title="lowercase letters">
		<input type="submit" value="make room">
	</form>
	<p>chat is not moderated, and no connection logs are kept</p>
	<p>room lifespan: %s (time until lossy room pruning may occur)</p>
	<p>Author: <a href="https://github.com/esote"
		target="_blank">Esote</a>.

		<a href="https://github.com/esote/chat"
		target="_blank">Source code</a>.</p>
</body>
</html>`

	roomStart = `<!DOCTYPE html>
<html lang="en">
<head>
	<meta charset="utf-8">
	<meta name="viewport"
		content="width=device-width, initial-scale=1, shrink-to-fit=no">
	<title>Room: %s</title>
</head>
<body>
	<p>room: %s</p>
	<p><a href="/">&lt; back</a></p>
	<form action="%s" method="post" autocomplete="off">
		<input type="text" name="msg" required autofocus maxlength="%d">
		<input type="submit" value="msg">
	</form>
	<p>chat history (time in UTC):</p><div id="chat">`

	roomEnd = `</div>
	<noscript>
		<p>without JS manually refresh to page to see new messages</p>
	</noscript>
	<script src="/realtime.js" integrity="sha512-+1INo3ZKQFSCijyLvXUVgQI00PLvSRnaqMZzUOqVW2bLzq8u6Bs0NdJci1GSAkLAmMvEdY3rkKNQPzPcn/XUMQ=="></script>
</body>
</html>`

	realtimeJS = `"use strict";
const http = new XMLHttpRequest();
const chat = document.getElementById("chat");
const path = window.location.pathname.split("/").pop();

http.onreadystatechange = function() {
	if (http.readyState == 4 && http.responseText != ""
		&& http.responseText != chat.innerHTML) {
		chat.innerHTML = http.responseText;
	}
}

function update() {
	http.open("PATCH", path, true);
	http.send(null);
}

setInterval(update, 1000);
`
)

func pruneRooms() {
	for k, v := range rooms {
		if time.Now().UTC().Sub(v.last) > lifespan {
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

func printChat(name string, w http.ResponseWriter) {
	fmt.Fprintf(w, "<pre>")

	for _, m := range rooms[name].msgs {
		fmt.Fprintf(w, "%s: %s\n\n", m.t, m.s)
	}

	fmt.Fprintf(w, "</pre>")
}

func get(name string, w http.ResponseWriter, r *http.Request) {
	pruneRooms()

	if !tryCreateRoom(name, w) {
		return
	}

	w.Header().Set("Content-Security-Policy", "default-src 'none';"+
		"script-src 'self'; connect-src 'self'")

	fmt.Fprintf(w, roomStart, name, name, name, maxMsgLen)
	printChat(name, w)
	fmt.Fprint(w, roomEnd)
}

func patch(name string, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", "default-src 'none';")
	w.Header().Set("Content-Type", "text/plain")

	printChat(name, w)
}

func post(name string, w http.ResponseWriter, r *http.Request) {
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

	w.Header().Set("Content-Security-Policy", "default-src 'none';")

	rm.last = time.Now().UTC()
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

func home(w http.ResponseWriter, r *http.Request) {
	if name := r.URL.Query().Get("name"); name != "" {
		http.Redirect(w, r, "/"+name, http.StatusSeeOther)
		return
	}

	fmt.Fprint(w, welcomeStart)
	for name := range rooms {
		fmt.Fprintf(w, `<p><a href="/%s">%s &gt;</a></p>`, name,
			name)
	}
	fmt.Fprintf(w, welcomeEnd, maxNameLen, validName.String(),
		lifespan)
}

func realtime(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "bad http verb", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Security-Policy", "default-src 'none';")
	w.Header().Set("Content-Type", "application/javascript")

	fmt.Fprint(w, realtimeJS)
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

	if len(name) > maxNameLen {
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

	if name == "" {
		home(w, r)
	} else {
		switch r.Method {
		case "GET":
			get(name, w, r)
		case "PATCH":
			patch(name, w, r)
		case "POST":
			post(name, w, r)
		}
	}

	lock.Unlock()
}

func main() {
	if err := openshim2.LazySysctls(); err != nil {
		log.Fatal(err)
	}

	if err := openshim2.Pledge("stdio inet", ""); err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", handler)
	mux.HandleFunc("/realtime.js", realtime)

	srv := &http.Server{
		Addr:    ":8444",
		Handler: mux,
	}

	go func() {
		ticker := time.NewTicker(lifespan)
		quit := make(chan struct{})

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
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}, os.Interrupt)
}
