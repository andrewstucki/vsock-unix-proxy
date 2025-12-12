package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/linuxkit/virtsock/pkg/vsock"
)

func main() {
	if len(os.Args) != 3 {
		serverUsage := fmt.Sprintf("server usage: %s <vsock-port> <unix-socket-path>", os.Args[0])
		clientUsage := fmt.Sprintf("client usage: %s <unix-socket-path> <vsock-port>", os.Args[0])
		log.Fatal(strings.Join([]string{serverUsage, clientUsage}, "\n"))
	}

	server := true
	portStr := os.Args[1]
	unixPath := os.Args[2]

	port, err := strconv.ParseInt(portStr, 10, 64)
	if err != nil {
		originalErr := err

		// attempt to swap
		port, err = strconv.ParseInt(portStr, 10, 64)
		if err != nil {
			log.Fatalf("invalid vsock port: %v", originalErr)
		} else {
			// swap because we succeeded in parsing
			server = false
			unixPath = portStr
		}
	}
	if port < 0 {
		log.Fatalf("invalid vsock port: %d", port)
	}

	if server {
		listenVsock(port, unixPath)
	} else {
		listenUnix(port, unixPath)
	}
}

func handleVsockConn(v net.Conn, unixPath string) {
	defer v.Close()

	u, err := net.Dial("unix", unixPath)
	if err != nil {
		log.Printf("error dialing unix socket: %v", err)
		return
	}
	defer u.Close()

	go io.Copy(u, v)
	io.Copy(v, u)
}

func listenVsock(port int64, unixPath string) {
	l, err := vsock.Listen(vsock.CIDAny, uint32(port))
	if err != nil {
		log.Fatalf("failed to listen on vsock port %d: %v", port, err)
	}
	log.Printf("listening on vsock port %d -> unix %s", port, unixPath)

	for {
		conn, err := l.Accept()
		if err != nil {
			log.Printf("accept error: %v", err)
			continue
		}

		go handleVsockConn(conn, unixPath)
	}
}

func handleUnixConn(v net.Conn, port int64) {
	defer v.Close()

	u, err := vsock.Dial(vsock.CIDAny, uint32(port))
	if err != nil {
		log.Printf("error dialing vsock port: %v", err)
		return
	}
	defer u.Close()

	go io.Copy(u, v)
	io.Copy(v, u)
}

func listenUnix(port int64, unixPath string) {
	l, err := net.Listen("unix", unixPath)
	if err != nil {
		log.Fatalf("failed to listen on unix socket %s: %v", unixPath, err)
	}
	log.Printf("listening on unix socket %s -> vsock %d", unixPath, port)

	for {
		conn, err := l.Accept()
		if err != nil {
			log.Printf("accept error: %v", err)
			continue
		}

		go handleUnixConn(conn, port)
	}
}
