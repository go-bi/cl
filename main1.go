package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

var (
	argv0 string

	codes map[os.Signal]int = map[os.Signal]int{
		syscall.SIGINT:  130,
		syscall.SIGKILL: 137,
	}
)

type host struct {
	user     string
	addr     string
	identity string
}

func unmarshal(r io.Reader) map[string][]host {
	s := bufio.NewScanner(r)
	m := make(map[string][]host)

	curr := ""

	for s.Scan() {
		line := strings.TrimSpace(s.Text())

		if line == "" {
			continue
		}

		end := len(line) - 1

		if line[end] == ':' {
			curr = line[:end]
			continue
		}

		h := host{
			user:     os.Getenv("USER"),
			identity: filepath.Join(os.Getenv("HOME"), ".ssh", "id_rsa"),
		}

		if _, ok := m[curr]; !ok {
			m[curr] = make([]host, 0)
		}

		if strings.Contains(line, " ") {
			parts := strings.Split(line, " ")

			h.identity = parts[1]

			if h.identity[0] == '~' {
				h.identity = strings.Replace(h.identity, "~", os.Getenv("HOME"), 1)
			}

			line = parts[0]
		}

		if strings.Contains(line, "@") {
			parts := strings.Split(line, "@")

			h.user = parts[0]
			line = parts[1]
		}

		host, port, _ := net.SplitHostPort(line)

		if host == "" {
			host = line
		}

		if port == "" {
			port = "22"
		}

		h.addr = net.JoinHostPort(host, port)

		m[curr] = append(m[curr], h)
	}

	return m
}

func run(h host, cmd string, cert string) ([]byte, error) {
	key, err := ioutil.ReadFile(cert)

	if err != nil {
		return []byte{}, err
	}

	signer, err := ssh.ParsePrivateKey(key)

	if err != nil {
		return []byte{}, err
	}

	cfg := &ssh.ClientConfig{
		User: h.user,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         time.Duration(time.Second * 60),
	}

	conn, err := ssh.Dial("tcp", h.addr, cfg)

	if err != nil {
		return []byte{}, err
	}

	defer conn.Close()

	sess, err := conn.NewSession()

	if err != nil {
		return []byte{}, err
	}

	defer sess.Close()

	b, err := sess.CombinedOutput(cmd)

	if err != nil {
		if _, ok := err.(*ssh.ExitError); !ok {
			return b, err
		}
	}

	return b, nil
}

func main() {
	argv0 = os.Args[0]

	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "usage: cl [cluster] [commands...]\n")
		os.Exit(1)
	}

	f, err := os.Open("hosts")

	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %s\n", argv0, err)
		os.Exit(1)
	}

	defer f.Close()
	breadline := bufio.NewReader(f)
	var saveHosts string
	certs := readCerts()

	for {
		line, _, err := breadline.ReadLine()
		if err == io.EOF {
			break
		}
		if err != nil {
			fmt.Println("read err:", err)
			os.Exit(-1)
		}
		saveHosts += string(line) + "\n"
	}
	f.Seek(0, 0)

	cluster := unmarshal(f)

	hosts, ok := cluster[os.Args[1]]

	if !ok {
		fmt.Fprintf(os.Stderr, "%s: unknown cluster\n", argv0)
		os.Exit(1)
	}

	wg := &sync.WaitGroup{}
	cmd := strings.Join(os.Args[2:], " ")

	errs := make(chan error)
	out := make(chan []byte)

	for _, h := range hosts {
		wg.Add(1)
		go func(h host, cmd string) {
			defer wg.Done()
			for _, cert := range certs {
				b, err := run(h, cmd, "certs/"+cert)
				if err != nil {
					errs <- err
					continue
				}

				outlog := h.user + "@" + h.addr + " " + h.identity
				saveHosts = strings.Replace(saveHosts, outlog+"\n", "", -1)
				saveHosts = strings.Replace(saveHosts, outlog, "", -1)

				f, err := os.OpenFile("hosts", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
				if err != nil {
					fmt.Println(err)
					return
				}
				fmt.Fprintf(f, saveHosts)
				f.Close()

				log(h.user + "@" + h.addr + " " + cert + " " + strings.Replace(string(b[:]), "\n", "", -1))
				out <- append([]byte("Host: "+h.addr+"\n"), b...)
				break
			}

		}(h, cmd)
	}

	c, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigs := make(chan os.Signal)

	signal.Notify(sigs, syscall.SIGINT, syscall.SIGKILL)

	code := 0

	go func() {
		sig := <-sigs
		cancel()
		code = codes[sig]
	}()

	go func() {
		wg.Wait()

		close(errs)
		close(out)
	}()

	for errs != nil && out != nil {
		select {
		case <-c.Done():
			fmt.Fprintf(os.Stderr, "%s: %s\n", argv0, c.Err())
			err = nil
			out = nil
			break
		case err, ok := <-errs:
			if !ok {
				errs = nil
				break
			}

			code = 1

			fmt.Fprintf(os.Stderr, "%s: %s\n", argv0, err)
			break
		case p, ok := <-out:
			if !ok {
				out = nil
				break
			}

			i := bytes.Index(p, []byte("\n"))

			os.Stderr.Write(p[:i])

			line := make([]byte, 0)

			for _, b := range p[i:] {
				line = append(line, b)

				if b == '\n' {
					os.Stdout.Write(append([]byte("  "), line...))
					line = make([]byte, 0)
				}
			}
		}
	}

	os.Exit(code)
}

func log(text string) {
	f, err := os.OpenFile("succ.txt", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Println(err)
		return
	}
	_, err = fmt.Fprintln(f, text)
	if err != nil {
		fmt.Println(err)
		f.Close()
		return
	}
	err = f.Close()
	if err != nil {
		fmt.Println(err)
		return
	}
}

func readCerts() []string {
	r, err := ioutil.ReadDir("certs")
	if err != nil {
		fmt.Println(err)
		return nil
	}
	var cert []string
	for _, ri := range r {
		if !ri.IsDir() {
			cert = append(cert, ri.Name())
		}
	}
	return cert
}
