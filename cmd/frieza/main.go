// Copyright 2022 Cisco Inc. All Rights Reserved.
// Copyright 2014 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

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
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/exp/slices"
)

const (
	headerRegexp = `^([\w-]+):\s*(.+)`
	authRegexp   = `^(.+):([^\s].+)`
	heyUA        = "frieza/0.0.1"
)

// Yes, ths is copied from hey, becuase it would be nice to use the same flags.
var usage = `Usage: frieza [options...] <url>
Options:
  -c  Number of connections to make. Default is 50.
  -q  Rate limit, in connections per second (CPS). Default is no rate limit.
  -z  Duration of application to send requests. When duration is reached,
      application stops and exits. Default is 5m.
      Examples: -z 10s -z 3m -z 1h.
  -H  Custom HTTP header. You can specify as many as needed by repeating the flag.
      For example, -H "Accept: text/html" -H "Content-Type: application/xml" .
  -k  Allow insecure connections when using TLS.
  -d  data to send on websocket.
  -D  data to send on websocket from file. For example, /home/user/file.txt or ./file.txt.
  -U  User-Agent, defaults to version "frieza/0.0.1".
  -v  Verbose output.
  -vv Very verbose output.
  -resolve <host:port:addr[,addr]...> Use custom addr to override DNS.
  -host	HTTP Host header -- not implemented -- use -resolve
`

func main() {
	var body, bodyFile, hostHeader, userAgent string
	var resolve string
	var conc, t, q int
	var dur time.Duration
	var k, h2, v, vv bool
	flag.StringVar(&body, "d", "", "")
	flag.StringVar(&bodyFile, "D", "", "")
	flag.StringVar(&hostHeader, "host", "", "")
	flag.StringVar(&userAgent, "U", "", "")

	flag.IntVar(&conc, "c", 50, "")
	flag.IntVar(&q, "q", 0, "")
	flag.IntVar(&t, "t", 20, "")
	flag.DurationVar(&dur, "z", 5*time.Minute, "")
	flag.BoolVar(&h2, "h2", false, "")
	flag.BoolVar(&v, "v", false, "")
	flag.BoolVar(&vv, "vv", false, "")
	flag.BoolVar(&k, "k", false, "")

	flag.StringVar(&resolve, "resolve", "", "")
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, usage)
	}

	var hs headerSlice
	flag.Var(&hs, "H", "")

	flag.Parse()
	if flag.NArg() < 1 {
		usageAndExit("")
	}

	if q == 0 {
		q = conc
	}

	url := flag.Arg(0)
	// set content-type
	header := make(http.Header)
	for _, h := range hs {
		match, err := parseInputWithRegexp(h, headerRegexp)
		if err != nil {
			usageAndExit(err.Error())
		}
		header.Set(match[1], match[2])
	}

	w := &Work{
		URL:     url,
		C:       conc,
		CPS:     q,
		Timeout: t,
		resolve: resolve,
		verbose: v,
		vv:      vv,
		header:  header,
		k:       k,
	}
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		<-c
		w.Stop()
	}()
	go func() {
		time.Sleep(dur)
		w.Stop()
	}()
	w.Start()
	w.PrintReport()
}

type Work struct {
	// TODO: Unexport everything.
	N        int
	C        int
	CPS      int
	Timeout  int
	URL      string
	resolve  string
	SendData string
	started  time.Time
	stopped  time.Time
	verbose  bool
	vv       bool
	k        bool
	sockets  chan *websocket.Conn
	counters chan *counter
	ao       *aoverride
	dila     *websocket.Dialer
	header   http.Header
	stopCh   chan struct{}
}

func (w *Work) PrintReport() {
	// TODO: Report more stats.
	var total int
	for c := range w.counters {
		total += c.N
	}
	fmt.Println(total, "bytes read from", w.C, "websockets")
}

func (w *Work) Stop() {
	if w.verbose {
		fmt.Println("stopping")
	}
	// This could block if any worker returned already from error :(
	for i := 0; i < w.C; i++ {
		w.stopCh <- struct{}{}
	}
	close(w.sockets)
	close(w.counters)
	for s := range w.sockets {
		err := s.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		if err != nil {
			log.Println("write close:", err)
			return
		}
		// s.Close() // Close the underlying socket, not sure if I should.
	}
	if w.verbose {
		fmt.Println("stopped")
	}
}

func (w *Work) Start() {
	w.sockets = make(chan *websocket.Conn, w.C)
	w.counters = make(chan *counter, w.C)
	w.stopCh = make(chan struct{}, w.C)
	w.dila = &websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: 5 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: w.k,
		},
	}

	if w.resolve != "" {
		r := strings.Split(w.resolve, ":")
		host := r[0]
		// port := r[1]
		addrs := r[2:]
		w.ao = &aoverride{h: host, addrs: addrs}
		w.dila.NetDialContext = w.ao.dial
	}
	w.started = time.Now()
	var wg sync.WaitGroup
	wg.Add(w.C)
	for i := 0; i < w.C; i++ {
		go func(i int) {
			w.runWorker(i)
			wg.Done()
		}(i)
		// This is a very naive attempt at CPS.
		if i > 0 && i%w.CPS == 0 {
			if w.verbose {
				fmt.Println(i, "workers started")
			}
			time.Sleep(1 * time.Second)
		}
	}
	if w.verbose {
		fmt.Println(w.C, "workers started")
	}
	wg.Wait()
	w.stopped = time.Now()
}

func (w *Work) runWorker(i int) {
	ws, resp, err := w.dila.Dial(w.URL, w.header)
	if err != nil {
		log.Println("fatal error dialing websocket ", i, ":", err)
		if err == websocket.ErrBadHandshake {
			log.Printf("%v %v %v\n", resp.StatusCode, resp.Status, resp.Header)
			io.Copy(os.Stderr, resp.Body)
		}
		return
	}
	if w.verbose {
		log.Print("websocket ", i, " connected")
	}
	w.sockets <- ws
	if w.SendData != "" {
		ww, err := ws.NextWriter(websocket.BinaryMessage)
		if err != nil {
			log.Print("error writing to websocket: ", err)
		}
		io.WriteString(ww, w.SendData)
	}
	c := &counter{}
	w.counters <- c
	for {
		messageType, r, err := ws.NextReader()
		if err != nil {
			if w.verbose {
				log.Print("error reading from websocket ", i, " type ", messageType)
			}
			return
		}
		if messageType == websocket.CloseMessage {
			return
		}
		var out io.Writer = c
		if w.vv {
			out = io.MultiWriter(os.Stdout, c)
		}
		n, err := io.Copy(out, r)
		if err != nil {
			log.Print("error reading from websocket:", err)
			return
		}
		if w.verbose {
			log.Print("read ", n, " bytes from websocket ", i, " type ", messageType)
		}
		select {
		case <-w.stopCh:
			return
		default:
		}
	}
}

type headerSlice []string

func (h *headerSlice) String() string {
	return fmt.Sprintf("%s", *h)
}

func (h *headerSlice) Set(value string) error {
	*h = append(*h, value)
	return nil
}

func usageAndExit(msg string) {
	if msg != "" {
		fmt.Fprint(os.Stderr, msg)
		fmt.Fprintf(os.Stderr, "\n\n")
	}
	flag.Usage()
	fmt.Fprintf(os.Stderr, "\n")
	os.Exit(1)
}

func parseInputWithRegexp(input, regx string) ([]string, error) {
	re := regexp.MustCompile(regx)
	matches := re.FindStringSubmatch(input)
	if len(matches) < 1 {
		return nil, fmt.Errorf("could not parse the provided input; input = %v", input)
	}
	return matches, nil
}

type counter struct {
	N int
}

func (c *counter) Write(p []byte) (n int, err error) {
	c.N += len(p)
	return len(p), nil
}

// https://koraygocmen.medium.com/custom-dns-resolver-for-the-default-http-client-in-go-a1420db38a5d
// and https://github.com/benburkert/dns/blob/d356cf78cdfc/init/init.go
type aoverride struct {
	addrs []string
	n     int32
	h     string
	seen  []string
}

// dial is a terribly poorly written function which needs much love.
func (as *aoverride) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aoverride SHP error:%v\n", err)
		os.Exit(1)
	}
	if host != as.h {
		c, err := net.Dial(network, address)
		raddr := c.RemoteAddr()
		if !slices.Contains(as.seen, raddr.String()) {
			as.seen = append(as.seen, raddr.String())
			fmt.Fprintf(os.Stderr, "NO aoverride dial(%s,%s) for %s host=%s", network, address, as.h, host)
			fmt.Fprintf(os.Stderr, "!! dialed %s\n", raddr)
		}
		return c, err
	}
	a := as.addrs[int(as.n)%len(as.addrs)] + ":" + port
	atomic.AddInt32(&as.n, 1)
	//fmt.Fprintf(os.Stderr, "aoverride dial %s %s using %s\n", network, address, a)

	// I want to do this, but nettrace is internal :(
	// trace, _ := ctx.Value(nettrace.TraceKey{}).(*nettrace.Trace)
	// trace.DNSDone(a, )
	// So instead???

	c, err := net.Dial(network, a)
	if err != nil {
		log.Println("aoverride dial error dialing ", network, " ", a, ":", err)
	}
	return c, err
}