package main

import (
	"crypto/tls"
	"flag"
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
	"github.com/esote/openshim"
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
	lock  = sync.RWMutex{}

	validName = regexp.MustCompile("^[a-z]+$")
	validMsg  = regexp.MustCompile(`^[[:print:]]+$`)
)

const (
	maxRoomCount = 5
	maxMsgLen    = 100
	maxMsgsCount = 25
	maxNameLen   = 5

	roomLifespan = 24 * time.Hour

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
	<script>
		const http = new XMLHttpRequest();
		const chat = document.getElementById("chat");

		http.onreadystatechange = function() {
			if (http.readyState == 4 && http.responseText != "") {
				chat.innerHTML = http.responseText;
			}
		}

		function update() {
			http.open("GET", window.location.href+"?chat", true);
			http.send(null);
		}

		setInterval(update, 2000);
	</script>
</body></html>`

	welcome_html_start = `<!DOCTYPE html>
<html><body>
	<p>welcome, join existing rooms:</p>`

	welcome_html_end = `
	<p>or make a room: <code>/name_here</code> (max length %d)</p>
	<p>room lifespan: %s (time until room pruning may occur)</p>
</body></html>`
)

func pruneRooms(lifespan time.Duration) {
	for k, v := range rooms {
		if time.Now().Sub(v.last) > lifespan {
			delete(rooms, k)
		}
	}
}

func printable(name string) []string {
	ret := make([]string, len(rooms[name].msgs))

	for k, v := range rooms[name].msgs {
		ret[len(ret)-k-1] = v.t + ": " + v.s
	}

	return ret
}

func get(name string, w http.ResponseWriter, r *http.Request) {
	lock.RLock()
	defer lock.RUnlock()

	if _, ok := r.URL.Query()["chat"]; ok {
		msgs := printable(name)
		for _, m := range msgs {
			fmt.Fprintf(w, "<pre>%s</pre>\n", m)
		}
		w.Header().Set("Content-Type", "text/plain")
		return
	}

	pruneRooms(roomLifespan)

	if _, ok := rooms[name]; !ok {
		if len(rooms) > maxRoomCount {
			http.Error(w, "too many rooms", http.StatusBadRequest)
			return
		}

		rooms[name] = room{msgs: make([]msg, 0)}
	}

	fmt.Fprintf(w, room_html_start, name, name)

	msgs := printable(name)
	for _, m := range msgs {
		fmt.Fprintf(w, "<pre>%s</pre>\n", m)
	}

	fmt.Fprint(w, room_html_end)
}

func post(name string, w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "form invalid", http.StatusBadRequest)
		return
	}

	str := r.PostFormValue("msg")
	str = strings.Replace(str, "\r", "", -1)

	if len(str) > maxMsgLen {
		http.Error(w, "msg too long", http.StatusBadRequest)
		return
	} else if !validMsg.MatchString(str) {
		http.Error(w, "bad msg", http.StatusBadRequest)
		return
	}

	str = strings.TrimSpace(str)

	if str == "" {
		http.Redirect(w, r, name, http.StatusSeeOther)
		return
	}

	str = html.EscapeString(str)

	lock.RLock()
	defer lock.RUnlock()

	rm, ok := rooms[name]

	if !ok {
		http.Error(w, "join room before posting", http.StatusBadRequest)
		return
	}

	for _, m := range rm.msgs {
		if m.s == str {
			http.Redirect(w, r, name, http.StatusSeeOther)
			return
		}
	}

	rm.last = time.Now()
	rm.msgs = append(rm.msgs, msg{
		s: str,
		t: rm.last.Format("2006-01-02 15:04"),
	})

	if l := len(rm.msgs); l > maxMsgsCount {
		rm.msgs = rm.msgs[l-maxMsgsCount:]
	}

	rooms[name] = rm

	http.Redirect(w, r, name, http.StatusSeeOther)
}

func handler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET", "POST":
		break
	default:
		http.Error(w, "bad http verb", http.StatusMethodNotAllowed)
		return
	}

	name := r.URL.Path[1:]

	if name == "" {
		fmt.Fprint(w, welcome_html_start)
		lock.RLock()
		for name := range rooms {
			fmt.Fprintf(w, `<p><a href="/%s">%s &gt;</a></p>`, name,
				name)
		}
		lock.RUnlock()
		fmt.Fprintf(w, welcome_html_end, maxNameLen, roomLifespan)
		return
	} else if len(name) > maxNameLen {
		http.Error(w, "name too long", http.StatusBadRequest)
		return
	} else if !validName.MatchString(name) {
		http.Error(w, "bad name", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case "GET":
		get(name, w, r)
	case "POST":
		post(name, w, r)
	}
}

func main() {
	var (
		cert string
		key  string
	)

	if _, err := openshim.Unveil("/etc/letsencrypt/archive/", "r"); err != nil {
		log.Fatal(err)
	}

	flag.StringVar(&cert, "cert", "server.crt", "TLS cerificate file")
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

	srv := &http.Server{
		Addr:         ":8444",
		Handler:      http.HandlerFunc(handler),
		TLSConfig:    cfg,
		TLSNextProto: nil,
	}

	var w sync.WaitGroup
	w.Add(1)

	go graceful.Graceful(srv, func() {
		defer w.Done()
		if err := srv.ListenAndServeTLS(cert, key); err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}, os.Interrupt)

	time.Sleep(time.Millisecond)

	if _, err := openshim.Pledge("stdio rpath inet", ""); err != nil {
		log.Fatal(err)
	}

	w.Wait()
}
