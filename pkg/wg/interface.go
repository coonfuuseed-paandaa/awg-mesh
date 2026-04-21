//go:build linux

package wg

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/conn"
	"github.com/amnezia-vpn/amneziawg-go/device"
	"github.com/amnezia-vpn/amneziawg-go/ipc"
	"github.com/amnezia-vpn/amneziawg-go/tun"
)

// Interface is a high-level AWG interface wrapper.
type Interface struct {
	name   string
	dev    *device.Device
	tunDev tun.Device
	uapi   net.Listener
}

// NewInterface creates and starts a new AWG interface instance.
func NewInterface(name string, mtu int, logger *device.Logger) (*Interface, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("interface name is required")
	}
	if mtu <= 0 {
		return nil, fmt.Errorf("mtu must be positive, got %d", mtu)
	}

	if logger == nil {
		logger = device.NewLogger(device.LogLevelSilent, "")
	}

	tunDev, err := tun.CreateTUN(name, mtu)
	if err != nil {
		return nil, fmt.Errorf("create TUN device: %w", err)
	}

	dev := device.NewDevice(tunDev, conn.NewDefaultBind(), logger)

	uapiFile, err := ipc.UAPIOpen(name)
	if err != nil {
		dev.Close()
		return nil, fmt.Errorf("open UAPI socket: %w", err)
	}

	uapiListener, err := net.FileListener(uapiFile)
	closeErr := uapiFile.Close()
	if err != nil {
		dev.Close()
		if closeErr != nil {
			return nil, fmt.Errorf("create UAPI listener: %w (also failed closing uapi file: %v)", err, closeErr)
		}
		return nil, fmt.Errorf("create UAPI listener: %w", err)
	}
	if closeErr != nil {
		_ = uapiListener.Close()
		dev.Close()
		return nil, fmt.Errorf("close UAPI file: %w", closeErr)
	}

	iface := &Interface{
		name:   name,
		dev:    dev,
		tunDev: tunDev,
		uapi:   uapiListener,
	}

	go iface.serveUAPI()

	return iface, nil
}

const uapiConnectionTimeout = 30 * time.Second

func (iface *Interface) serveUAPI() {
	for {
		conn, err := iface.uapi.Accept()
		if err != nil {
			return
		}
		_ = conn.SetDeadline(time.Now().Add(uapiConnectionTimeout))
		go iface.dev.IpcHandle(conn)
	}
}

// Close releases all interface resources.
func (iface *Interface) Close() error {
	if iface == nil {
		return nil
	}

	var listenerErr error
	if iface.uapi != nil {
		if err := iface.uapi.Close(); err != nil {
			listenerErr = fmt.Errorf("close uapi listener: %w", err)
		}
	}

	if iface.dev != nil {
		iface.dev.Close()
	}

	return listenerErr
}

// Name returns the interface name.
func (iface *Interface) Name() string {
	return iface.name
}

// Configure applies a device configuration.
func (iface *Interface) Configure(cfg Config) error {
	if iface == nil {
		return errors.New("interface is nil")
	}
	return NewUAPIClient().ConfigureDevice(iface.name, cfg)
}

// OpenExistingInterface attempts to obtain a handle to an already-running AWG
// interface by connecting to its UAPI socket. The returned Interface has no
// TUN device handle and no UAPI listener — Close() on it is a no-op that
// succeeds immediately. If the UAPI socket is absent (e.g. the old binary is
// gone), this function returns an error and the caller should fall back to raw
// netlink deletion.
func OpenExistingInterface(name string) (*Interface, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("interface name is required")
	}
	// Probe the UAPI socket with a transient connection. If the socket does not
	// exist or is not accepting connections, the old process is gone and we
	// cannot obtain a managed handle.
	conn, err := net.Dial("unix", fmt.Sprintf("/var/run/amneziawg/%s.sock", name))
	if err != nil {
		return nil, fmt.Errorf("open existing interface %q: uapi socket not available: %w", name, err)
	}
	_ = conn.Close()
	// Return a minimal handle. Close() is a safe no-op for this variant because
	// dev and uapi are nil — the kernel interface is removed via netlink by the
	// caller after Close() returns.
	return &Interface{name: name}, nil
}

// GetDevice fetches current device state.
func (iface *Interface) GetDevice() (*Device, error) {
	if iface == nil {
		return nil, errors.New("interface is nil")
	}
	return NewUAPIClient().Device(iface.name)
}
