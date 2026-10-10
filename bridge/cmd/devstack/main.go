// Command devstack runs the bridge together with the in-memory test edge
// in one process, with invented mailboxes: the first with about 300 mails
// in INBOX and 40 in Sent, the others small. It is for trying a mail
// program against the bridge on a developer machine. It is not part of the
// production image and talks to nothing outside.
//
// Called as "mail <address> <subject> [text]" it asks the running devstack
// in the same container to deliver a new mail (./dev.sh mail ...).
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/bridge"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/config"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/edgestub"
)

type account struct{ address, password string }

// readAccounts reads "address password" lines from the credentials file
// (DEV_CREDENTIALS, default /run/dev-credentials). Lines starting with #
// are comments.
func readAccounts() ([]account, error) {
	path := os.Getenv("DEV_CREDENTIALS")
	if path == "" {
		path = "/run/dev-credentials"
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read the credentials file (DEV_CREDENTIALS)")
	}
	defer f.Close()
	var out []account
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 || !strings.Contains(parts[0], "@") || !strings.HasSuffix(parts[0], ".test") {
			return nil, fmt.Errorf("credentials file: every line is \"address password\", addresses end in .test")
		}
		out = append(out, account{parts[0], parts[1]})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("credentials file has no accounts")
	}
	return out, nil
}

// controlAddr is where a running devstack takes requests to put a new mail
// into a mailbox. It listens inside the container only.
const controlAddr = "127.0.0.1:8090"

// sendMail is the client side of "mail <address> <subject> [text]": it asks
// the running devstack in the same container to deliver a mail.
func sendMail(args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: mail <address> <subject> [text]")
		return 2
	}
	form := url.Values{"address": {args[0]}, "subject": {args[1]}}
	if len(args) > 2 {
		form.Set("text", strings.Join(args[2:], " "))
	}
	resp, err := http.PostForm("http://"+controlAddr+"/deliver", form)
	if err != nil {
		fmt.Fprintln(os.Stderr, "devstack is not running:", err)
		return 1
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	fmt.Println(strings.TrimSpace(string(body)))
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

// control serves the requests of sendMail.
func control(stub *edgestub.Stub, accounts []account) {
	known := map[string]bool{}
	for _, a := range accounts {
		known[strings.ToLower(a.address)] = true
	}
	var counter int
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("/deliver", func(w http.ResponseWriter, r *http.Request) {
		address := strings.ToLower(strings.TrimSpace(r.PostFormValue("address")))
		subject := strings.TrimSpace(r.PostFormValue("subject"))
		if r.Method != http.MethodPost || !known[address] || subject == "" || len(subject) > 300 {
			http.Error(w, "unknown mailbox or empty subject", http.StatusBadRequest)
			return
		}
		mu.Lock()
		counter++
		n := counter
		mu.Unlock()
		from := people[n%len(people)]
		text := r.PostFormValue("text")
		if text == "" {
			text = "Hello,\n\nthis mail was injected by hand at " + time.Now().Format("15:04:05") + " (./dev.sh mail).\n\nBest regards"
		}
		now := time.Now()
		raw := edgestub.BuildMail(edgestub.MailSpec{
			From: from.String(), To: []string{address}, Subject: subject, Date: now, Text: text,
			MessageID: fmt.Sprintf("injected-%d-%d@mail.werkstatt.test", now.UnixNano(), n),
		})
		uid := stub.Deliver(address, edgestub.Inbox, raw, now)
		fmt.Fprintf(w, "injected: uid %d in INBOX, sender %s\n", uid, from.address)
	})
	ln, err := net.Listen("tcp", controlAddr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "devstack: control port:", err)
		return
	}
	go func() { _ = http.Serve(ln, mux) }()
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "mail" {
		os.Exit(sendMail(os.Args[2:]))
	}
	accounts, err := readAccounts()
	if err != nil {
		fmt.Fprintln(os.Stderr, "devstack:", err)
		os.Exit(2)
	}

	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	secret := "dev_" + hex.EncodeToString(buf)
	// For trying test/against-edge.sh without a Convex: the test edge of
	// this devstack can be reached from outside (DEV_EDGE_ADDR, for
	// instance 0.0.0.0:3211) with a secret given from outside
	// (DEV_EDGE_SECRET, at least 32 characters). Without them the test
	// edge is only reachable inside the process.
	edgeAddr := "127.0.0.1:0"
	if v := strings.TrimSpace(os.Getenv("DEV_EDGE_ADDR")); v != "" {
		edgeAddr = v
		if given := strings.TrimSpace(os.Getenv("DEV_EDGE_SECRET")); len(given) >= config.MinSecretChars {
			secret = given
		} else {
			fmt.Fprintln(os.Stderr, "devstack: DEV_EDGE_ADDR needs DEV_EDGE_SECRET (at least 32 characters)")
			os.Exit(2)
		}
	}
	stub := edgestub.New(secret)
	ln, err := net.Listen("tcp", edgeAddr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "devstack: cannot listen for the test edge:", err)
		os.Exit(1)
	}
	go func() { _ = http.Serve(ln, stub) }()

	for i, a := range accounts {
		stub.AddMailbox(a.address)
		stub.SetPassword(a.address, a.password)
		// the first mailbox is the large one
		inbox, sent := 30, 6
		if i == 0 {
			inbox, sent = 300, 40
		}
		seed(stub, a.address, i, accounts, inbox, sent)
	}
	// A mail sent to one of the local mailboxes arrives there.
	local := map[string]bool{}
	for _, a := range accounts {
		local[strings.ToLower(a.address)] = true
	}
	stub.OnSent = func(from string, to []string, raw []byte) {
		time.Sleep(1500 * time.Millisecond)
		for _, rcpt := range to {
			if local[strings.ToLower(rcpt)] {
				stub.Deliver(rcpt, edgestub.Inbox, raw, time.Now())
			}
		}
	}
	control(stub, accounts)
	// Optionally a new mail every few seconds, to watch IDLE.
	if n, _ := strconv.Atoi(os.Getenv("DEV_TICK_SECONDS")); n > 0 {
		go func() {
			for i := 1; ; i++ {
				time.Sleep(time.Duration(n) * time.Second)
				for _, a := range accounts {
					stub.Deliver(a.address, edgestub.Inbox, edgestub.BuildMail(edgestub.MailSpec{
						From: people[i%len(people)].String(), To: []string{a.address},
						Subject: fmt.Sprintf("New mail no. %d (ticker)", i), Date: time.Now(),
						Text: "This mail comes from the test edge so that IDLE can be observed.",
					}), time.Now())
				}
			}
		}()
	}

	get := func(key string) string {
		switch key {
		case "CONVEX_SITE_URL":
			_, port, _ := net.SplitHostPort(ln.Addr().String())
			return "http://127.0.0.1:" + port
		case "MAIL_BRIDGE_SECRET":
			return secret
		case "PROXY_PROTOCOL":
			if v := os.Getenv(key); v != "" {
				return v
			}
			return "off"
		}
		return os.Getenv(key)
	}
	cfg, err := config.Load(get)
	if err != nil {
		fmt.Fprintln(os.Stderr, "devstack: configuration:", err)
		os.Exit(2)
	}
	b, err := bridge.Start(cfg, bridge.Options{LogWriter: os.Stdout})
	if err != nil {
		fmt.Fprintln(os.Stderr, "devstack: cannot listen:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "devstack: test edge and bridge are up, %d mailboxes (IMAP %s, SMTP %s, metrics %s)\n",
		len(accounts), b.IMAPAddr(), b.SMTPAddr(), b.AdminAddr())

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	b.Shutdown()
}
