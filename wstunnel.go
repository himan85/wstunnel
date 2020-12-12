package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"io/ioutil"
	"runtime"
	"strings"
	"sync"

	"flag"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/gorilla/websocket"
)

const (
	VERSION     = "0.01"
	CONN_TYPE   = "tcp"
	BUFFER_SIZE = 65536
	TIME_OUT    = 30
)

type Relay struct {
	wg sync.WaitGroup
}

func (r *Relay) pipeToWs(netConn net.Conn, wsConn *websocket.Conn) {
	defer func() {
		wsConn.Close() //send EOF
		r.wg.Done()
	}()
	for {
		err := netConn.SetReadDeadline(time.Now().Add(TIME_OUT * time.Second)) // timeout
		if err != nil {
			break
		}
		data := make([]byte, BUFFER_SIZE)
		n, err := netConn.Read(data)
		if err != nil {
			return
		}
		err = wsConn.WriteMessage(websocket.BinaryMessage, data[:n])
		if err != nil {
			return
		}
	}

}

func (r *Relay) pipeToNet(netConn net.Conn, wsConn *websocket.Conn) {
	defer func() {
		netConn.Close()
		r.wg.Done()
	}()
	for {
		err := wsConn.SetReadDeadline(time.Now().Add(TIME_OUT * time.Second))
		if err != nil {
			break
		}
		_, p, err := wsConn.ReadMessage()
		if err != nil {
			return
		}
		_, err = netConn.Write(p)
		if err != nil {
			return
		}
	}
}

func (r *Relay) relay(netConn net.Conn, wsConn *websocket.Conn) {
	r.wg.Add(2)
	go r.pipeToWs(netConn, wsConn)
	go r.pipeToNet(netConn, wsConn)
	r.wg.Wait()
}

type Local struct {
	listenAddr string
	serverAddr string
	hostname   string
	isTls      bool
}

var (
	Trace   *log.Logger
	Info    *log.Logger
	Warning *log.Logger
	Error   *log.Logger
)

func (l *Local) startLocal() error {
	// Listen for incoming connections.
	ln, err := net.Listen(CONN_TYPE, l.listenAddr)
	if err != nil {
		Error.Printf("error listening: %v", err.Error())
		return err
	}
	// Close the listener when the application closes.
	defer ln.Close()
	Info.Printf("listening on " + l.listenAddr)
	for {
		// Listen for an incoming connection.
		conn, err := ln.Accept()
		if err != nil {
			Error.Printf("error accepting: %v", err.Error())
			return err
		}
		go l.handleClientrRequest(conn)
	}
}

func (l *Local) handleClientrRequest(clientConn net.Conn) {
	clientAddr := clientConn.RemoteAddr()
	defer func() {
		clientConn.Close()
		Trace.Printf("src: %v, connection closed.", clientAddr)
	}()

	u, err := url.Parse(l.serverAddr)
	if err != nil {
		Error.Printf(err.Error())
		return
	}

	REQUEST_HEADERS := map[string][]string{
		// "Sec-WebSocket-Protocol": []string{"binary"},
		"User-Agent": []string{"Go-http-client/1.1"}}
	var pTlsConfig *tls.Config

	if len(l.hostname) > 0 {
		var headerHost string
		if serverPort := u.Port(); len(serverPort) > 0 {
			headerHost = l.hostname + ":" + u.Port()
		} else {
			headerHost = l.hostname
		}
		REQUEST_HEADERS["Host"] = []string{headerHost}
		pTlsConfig = &tls.Config{ServerName: l.hostname}
	}

	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	if l.isTls {
		dialer.TLSClientConfig = pTlsConfig
	}

	serverWs, _, err := dialer.Dial(l.serverAddr, REQUEST_HEADERS)
	if err != nil {
		Error.Printf("src: %v, client connection failed: %v", clientAddr, err.Error())
		return
	}
	defer func() {
		serverWs.Close()
		Trace.Printf("src: %v, dst: %v, server websocket connection closed.", clientAddr, l.serverAddr)
	}()

	Trace.Printf("src: %v, dst: %v, websocket connection established, proxying...", clientAddr, l.serverAddr)

	rl := Relay{}
	rl.relay(clientConn, serverWs)
}

type Server struct {
	upgrader   websocket.Upgrader
	listenAddr string
	remoteAddr string
	hostname   string
	isTls      bool
	cert       string
}

func (s *Server) handleLocalRequest(w http.ResponseWriter, r *http.Request) {
	localWs, err := s.upgrader.Upgrade(w, r, nil)
	localAddr := localWs.RemoteAddr()
	if err != nil {
		Error.Print("upgrade:", err)
		return
	}
	defer func() {
		localWs.Close()
		Trace.Printf("src: %v, local websocket connection closed.", localAddr)
	}()
	remoteConn, err := net.DialTimeout(CONN_TYPE, s.remoteAddr, 10*time.Second)
	if err != nil {
		Error.Println(err)
		return
	}
	remoteAddr := remoteConn.RemoteAddr()
	defer func() {
		remoteConn.Close()
		Trace.Printf("src: %v, dst: %v, remote connection closed.", localAddr, remoteAddr)
	}()

	rl := Relay{}
	rl.relay(remoteConn, localWs)
}

func (s *Server) startServer() error {
	http.HandleFunc("/", s.handleLocalRequest)
	Info.Println("Listening on " + s.listenAddr)

	var err error
	if s.isTls {
		cert := strings.Split(s.cert, ";")
		err = http.ListenAndServeTLS(s.listenAddr, cert[0], cert[1], nil)
	} else {
		err = http.ListenAndServe(s.listenAddr, nil)
	}
	if err != nil {
		Error.Println(err)
		return err
	}
	return nil
}

func initLogger(logLevel string) {
	file, err := os.OpenFile("errors.txt",
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		log.Fatalln("Failed to open error log file:", err)
	}

	var t io.Writer = os.Stdout
	var i io.Writer = os.Stdout
	var w io.Writer = os.Stdout
	var e io.Writer = os.Stdout

	if logLevel == "trace" {
		e = io.MultiWriter(file, os.Stderr)
	} else if logLevel == "info" {
		t = ioutil.Discard
		e = io.MultiWriter(file, os.Stderr)
	} else if logLevel == "warning" {
		t = ioutil.Discard
		i = ioutil.Discard
		e = io.MultiWriter(file, os.Stderr)
	} else if logLevel == "error" {
		t = ioutil.Discard
		i = ioutil.Discard
		w = ioutil.Discard
		e = io.MultiWriter(file, os.Stderr)
	}
	Trace = log.New(t, "TRACE: ", log.Ldate|log.Ltime)
	Info = log.New(i, "INFO: ", log.Ldate|log.Ltime)
	Warning = log.New(w, "WARNING: ", log.Ldate|log.Ltime|log.Lshortfile)
	Error = log.New(e, "ERROR: ", log.Ldate|log.Ltime|log.Lshortfile)
}

func printNumOfGo() {
	for {
		select {
		case <-time.After(time.Second * 2):
			goNum := runtime.NumGoroutine()
			fmt.Println(goNum)
		}
	}
}

func main() {
	// go printNumOfGo()
	isServer := flag.Bool("server", false, "enbale server mode")
	isTls := flag.Bool("tls", false, "optional tls mode")
	hostname := flag.String("hostname", "", "specify hostname for tls and websocket")
	cert := flag.String("cert", "", "specify cert files for tls mode")
	rhost := flag.String("rhost", "REQUIRED", "host:port, support ws://host:port or wss://host:port format")
	lhost := flag.String("lhost", "0.0.0.0:10888", "host:port")
	logl := flag.String("log", "info", "optional log level: trace, info, warning, error")
	flag.Parse()

	initLogger(*logl)
	if *rhost == "REQUIRED" {
		log.Println("Usage:")
		flag.PrintDefaults()
		os.Exit(1)
	}

	if *isServer {
		server := Server{
			listenAddr: *lhost,
			remoteAddr: *rhost,
			hostname:   *hostname,
			isTls:      *isTls,
			cert:       *cert}
		err := server.startServer()
		if err != nil {
			Error.Println(err)
			return
		}
	} else {
		local := Local{
			listenAddr: *lhost,
			serverAddr: *rhost,
			isTls:      *isTls,
			hostname:   *hostname}
		err := local.startLocal()
		if err != nil {
			Error.Println(err)
			return
		}
	}
}
