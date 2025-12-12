package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

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

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if server {
		retryUntilCanceled(ctx, 1*time.Second, port, unixPath, listenVsock)
	} else {
		retryUntilCanceled(ctx, 1*time.Second, port, unixPath, listenUnix)
	}
}

func retryUntilCanceled(ctx context.Context, backoff time.Duration, port int64, unixPath string, operation func(ctx context.Context, port int64, unixPath string) error) {
	doOperation := func() {
		if err := operation(ctx, port, unixPath); err != nil {
			log.Printf("operation failed: %v, retrying in %s...", err, backoff)
		}
	}

	doOperation()

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
			doOperation()
		}
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

func listenVsock(ctx context.Context, port int64, unixPath string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	l, err := vsock.Listen(vsock.CIDAny, uint32(port))
	if err != nil {
		return fmt.Errorf("failed to listen on vsock port %d: %v", port, err)
	}

	go func() {
		<-ctx.Done()
		l.Close()
	}()

	log.Printf("listening on vsock port %d -> unix %s", port, unixPath)

	for {
		conn, err := l.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
			}
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

func listenUnix(ctx context.Context, port int64, unixPath string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	l, err := net.Listen("unix", unixPath)
	if err != nil {
		return fmt.Errorf("failed to listen on unix socket %s: %v", unixPath, err)
	}

	log.Printf("listening on unix socket %s -> vsock %d", unixPath, port)

	go func() {
		<-ctx.Done()
		l.Close()
	}()

	for {
		conn, err := l.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
			}
			log.Printf("accept error: %v", err)
			continue
		}

		go handleUnixConn(conn, port)
	}
}
