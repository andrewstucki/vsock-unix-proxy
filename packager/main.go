package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"

	"github.com/andrewstucki/vsock-unix-proxy/oci"
)

func main() {
	if len(os.Args) != 5 {
		log.Fatalf("usage: %s <tag> <initrd> <kernel> <destination>", os.Args[0])
	}

	tag := os.Args[1]
	initrdPath := os.Args[2]
	kernelPath := os.Args[3]
	destinationPath := os.Args[4]

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := oci.Package(ctx, tag, initrdPath, kernelPath, destinationPath); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}
