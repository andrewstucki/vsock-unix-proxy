package test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestProxy(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 1*time.Minute)
	defer cancel()

	instance := NewVMInstance(VMInstanceConfig{
		VMLinuzPath: "test-kernel",
		InitrdPath:  "test-initrd.img",
		DiskPath:    "disk",
		DiskSize:    100 * MB,
		MemorySize:  2 * GB,
		CPUs:        1,
		Port:        8080,
	})

	errs := make(chan error, 1)
	go func() {
		errs <- instance.Run(ctx)
	}()

	require.Eventually(t, func() bool {
		conn, err := instance.Dial()
		if err != nil {
			t.Log(err)
			return false
		}
		defer conn.Close()

		data := make([]byte, 5)
		n, err := conn.Read(data)
		if err != nil {
			t.Log(err)
			return false
		}
		response := strings.TrimSpace(string(data[:n]))

		return response == "test"
	}, 30*time.Second, 1*time.Second, "unable to dial instance")

	cancel()

	require.NoError(t, <-errs)
}
