package test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/Code-Hex/vz/v3"
	"github.com/pkg/term/termios"
	"golang.org/x/sys/unix"
)

const (
	KB = 1 << 10
	MB = KB << 10
	GB = MB << 10
)

func EnsureDiskImage(diskpath string, size int64) error {
	if err := vz.CreateDiskImage(diskpath, size); err != nil {
		return err
	}
	return nil
}

type VMInstanceConfig struct {
	VMLinuzPath string
	InitrdPath  string
	DiskPath    string
	DiskSize    int64
	MemorySize  uint64
	CPUs        uint
	Port        uint32
}

type VMInstance struct {
	VMInstanceConfig

	socketDevice *vz.VirtioSocketDevice
	startedCh    chan struct{}
	started      atomic.Bool
}

func NewVMInstance(config VMInstanceConfig) *VMInstance {
	return &VMInstance{
		VMInstanceConfig: config,
		startedCh:        make(chan struct{}),
	}
}

func (v *VMInstance) start() {
	if v.started.CompareAndSwap(false, true) {
		close(v.startedCh)
	}
}

func (v *VMInstance) Dial() (net.Conn, error) {
	<-v.startedCh
	return v.socketDevice.Connect(v.Port)
}

func (v *VMInstance) Run(ctx context.Context) error {
	kernelCommandLineArguments := []string{
		"console=hvc0",
		"root=/dev/vda",
		fmt.Sprintf("vsock_port=%d", v.Port),
	}

	if err := EnsureDiskImage(v.DiskPath, v.DiskSize); err != nil {
		if !os.IsExist(err) {
			return err
		}
	}

	bootLoader, err := vz.NewLinuxBootLoader(
		v.VMLinuzPath,
		vz.WithCommandLine(strings.Join(kernelCommandLineArguments, " ")),
		vz.WithInitrd(v.InitrdPath),
	)
	if err != nil {
		return err
	}

	config, err := vz.NewVirtualMachineConfiguration(
		bootLoader,
		v.CPUs,
		v.MemorySize,
	)
	if err != nil {
		return err
	}

	v.setRawStdin()

	serialPortAttachment, err := vz.NewFileHandleSerialPortAttachment(os.Stdin, os.Stdout)
	if err != nil {
		return err
	}
	consoleConfig, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(serialPortAttachment)
	if err != nil {
		return err
	}
	config.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{
		consoleConfig,
	})

	natAttachment, err := vz.NewNATNetworkDeviceAttachment()
	if err != nil {
		return err
	}
	networkConfig, err := vz.NewVirtioNetworkDeviceConfiguration(natAttachment)
	if err != nil {
		return err
	}
	config.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{
		networkConfig,
	})
	mac, err := vz.NewRandomLocallyAdministeredMACAddress()
	if err != nil {
		return err
	}
	networkConfig.SetMACAddress(mac)

	entropyConfig, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return err
	}
	config.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{
		entropyConfig,
	})

	diskImageAttachment, err := vz.NewDiskImageStorageDeviceAttachment(
		v.DiskPath,
		false,
	)
	if err != nil {
		log.Fatal(err)
	}
	storageDeviceConfig, err := vz.NewVirtioBlockDeviceConfiguration(diskImageAttachment)
	if err != nil {
		return err
	}
	config.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{
		storageDeviceConfig,
	})

	memoryBalloonDevice, err := vz.NewVirtioTraditionalMemoryBalloonDeviceConfiguration()
	if err != nil {
		return err
	}
	config.SetMemoryBalloonDevicesVirtualMachineConfiguration([]vz.MemoryBalloonDeviceConfiguration{
		memoryBalloonDevice,
	})

	vsockDevice, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return err
	}
	config.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{
		vsockDevice,
	})
	validated, err := config.Validate()
	if !validated || err != nil {
		return err
	}

	vm, err := vz.NewVirtualMachine(config)
	if err != nil {
		return err
	}

	devices := vm.SocketDevices()
	if len(devices) != 1 {
		return errors.New("invalid socket device length")
	}
	v.socketDevice = devices[0]
	if err := vm.Start(); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			result, err := vm.RequestStop()
			if err != nil {
				return err
			}
			if !result {
				return errors.New("unable to stop VM")
			}
			return nil
		case newState := <-vm.StateChangedNotify():
			if newState == vz.VirtualMachineStateRunning {
				v.start()
			}
			if newState == vz.VirtualMachineStateStopped {
				return errors.New("stopped prematurely")
			}
		}
	}
}

func (v *VMInstance) setRawStdin() {
	f := os.Stdin

	var attr unix.Termios

	// Get settings for terminal
	termios.Tcgetattr(f.Fd(), &attr)

	// Put stdin into raw mode, disabling local echo, input canonicalization,
	// and CR-NL mapping.
	attr.Iflag &^= syscall.ICRNL
	attr.Lflag &^= syscall.ICANON | syscall.ECHO

	// Set minimum characters when reading = 1 char
	attr.Cc[syscall.VMIN] = 1

	// set timeout when reading as non-canonical mode
	attr.Cc[syscall.VTIME] = 0

	// reflects the changed settings
	termios.Tcsetattr(f.Fd(), termios.TCSANOW, &attr)
}
