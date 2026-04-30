//go:build linux

package node

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// VRFAnchorIfaceNamePrefix is prepended to the stripped VRF name to form the
// anchor dummy interface name (e.g. "vrf_overlay" → "wg-vrf-overlay").
const VRFAnchorIfaceNamePrefix = "wg-vrf-"

// Test seams — overridable by unit tests to inject mock netlink behaviour.
var netlinkLinkAdd = netlink.LinkAdd
var netlinkLinkDel = netlink.LinkDel
var netlinkLinkByName = netlink.LinkByName
var netlinkLinkSetMaster = netlink.LinkSetMaster
var netlinkLinkSetNoMaster = netlink.LinkSetNoMaster
var netlinkLinkSetUp = netlink.LinkSetUp
var netlinkAddrAdd = netlink.AddrAdd

// isVRFSupportedOnce guards the single probe attempt so the kernel call is
// made at most once per process. Exposed for test reset via resetIsVRFSupportedOnce.
var (
	isVRFSupportedOnce   sync.Once
	isVRFSupportedResult error
)

// resetIsVRFSupportedOnce resets the sync.Once so tests can re-probe with
// different injected netlink behaviours. Must not be called in production code.
var resetIsVRFSupportedOnce = func() {
	isVRFSupportedOnce = sync.Once{}
	isVRFSupportedResult = nil
}

// VRFManager owns the lifetime of a single VRF instance for one awg-mesh-node
// process. Create with NewVRFManager; call Setup() before EnslaveInterface.
type VRFManager struct {
	name       string
	table      uint32
	overlayIP  net.IP
	anchorName string

	mu      sync.Mutex
	created bool
}

// NewVRFManager constructs a manager with the given VRF name, routing table
// number, and overlay IP to assign to the anchor dummy interface. No kernel
// operations are performed until Setup() is called.
//
// Anchor name is derived by stripping any leading "vrf_" prefix from name and
// prepending VRFAnchorIfaceNamePrefix:
//
//	"vrf_overlay" → "wg-vrf-overlay"
//	"tenantA"     → "wg-vrf-tenantA"
func NewVRFManager(name string, table uint32, overlayIP net.IP) *VRFManager {
	stripped := name
	if len(name) > 4 && name[:4] == "vrf_" {
		stripped = name[4:]
	}
	return &VRFManager{
		name:       name,
		table:      table,
		overlayIP:  overlayIP,
		anchorName: VRFAnchorIfaceNamePrefix + stripped,
	}
}

// Setup creates the VRF master device (if absent) and the anchor dummy
// interface, enslaves the anchor, assigns the overlay IP/32 to it, and brings
// both interfaces up. The operation is idempotent: calling Setup on an already-
// configured VRF returns nil without duplicating any kernel objects.
func (m *VRFManager) Setup() error {
	if err := IsVRFSupported(); err != nil {
		return fmt.Errorf("vrf setup: %w", err)
	}

	// Locate or create the VRF master device.
	var vrfLink netlink.Link
	existing, err := netlinkLinkByName(m.name)
	if err == nil {
		// Interface exists — reuse if it is the right type and table.
		if vrf, ok := existing.(*netlink.Vrf); ok && vrf.Table == m.table {
			vrfLink = existing
		} else {
			return fmt.Errorf("vrf setup: interface %q exists but is not a VRF with table %d", m.name, m.table)
		}
	} else {
		// Create the VRF master device.
		newVRF := &netlink.Vrf{
			LinkAttrs: netlink.LinkAttrs{Name: m.name},
			Table:     m.table,
		}
		if addErr := netlinkLinkAdd(newVRF); addErr != nil {
			return fmt.Errorf("vrf setup: create VRF %q table %d: %w", m.name, m.table, addErr)
		}
		vrfLink, err = netlinkLinkByName(m.name)
		if err != nil {
			return fmt.Errorf("vrf setup: get VRF %q after create: %w", m.name, err)
		}
		if upErr := netlinkLinkSetUp(vrfLink); upErr != nil {
			return fmt.Errorf("vrf setup: bring up VRF %q: %w", m.name, upErr)
		}
	}

	// Locate or create the anchor dummy interface.
	var anchorLink netlink.Link
	existingAnchor, anchorErr := netlinkLinkByName(m.anchorName)
	if anchorErr == nil {
		if _, ok := existingAnchor.(*netlink.Dummy); ok {
			anchorLink = existingAnchor
		} else {
			return fmt.Errorf("vrf setup: anchor interface %q exists but is not a dummy", m.anchorName)
		}
	} else {
		newDummy := &netlink.Dummy{
			LinkAttrs: netlink.LinkAttrs{Name: m.anchorName},
		}
		if addErr := netlinkLinkAdd(newDummy); addErr != nil {
			return fmt.Errorf("vrf setup: create anchor dummy %q: %w", m.anchorName, addErr)
		}
		anchorLink, err = netlinkLinkByName(m.anchorName)
		if err != nil {
			return fmt.Errorf("vrf setup: get anchor %q after create: %w", m.anchorName, err)
		}
	}

	// Enslave anchor to VRF.
	if masterErr := netlinkLinkSetMaster(anchorLink, vrfLink); masterErr != nil {
		return fmt.Errorf("vrf setup: enslave anchor %q to VRF %q: %w", m.anchorName, m.name, masterErr)
	}

	// Assign overlay IP/32 to anchor (idempotent: EEXIST is ignored).
	overlayAddr := &netlink.Addr{
		IPNet: &net.IPNet{IP: m.overlayIP, Mask: net.CIDRMask(32, 32)},
	}
	if addrErr := netlinkAddrAdd(anchorLink, overlayAddr); addrErr != nil {
		// Ignore address-already-exists errors so Setup() is idempotent.
		if !errors.Is(addrErr, unix.EEXIST) {
			return fmt.Errorf("vrf setup: assign overlay IP %s to anchor %q: %w", m.overlayIP, m.anchorName, addrErr)
		}
	}

	// Bring anchor up.
	if upErr := netlinkLinkSetUp(anchorLink); upErr != nil {
		return fmt.Errorf("vrf setup: bring up anchor %q: %w", m.anchorName, upErr)
	}

	m.mu.Lock()
	m.created = true
	m.mu.Unlock()
	return nil
}

// EnslaveInterface adds the named interface as a VRF slave. The call is
// serialised by m.mu so concurrent enslavement is race-free.
func (m *VRFManager) EnslaveInterface(ifaceName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	child, err := netlinkLinkByName(ifaceName)
	if err != nil {
		return fmt.Errorf("enslave %q: get interface: %w", ifaceName, err)
	}
	vrfLink, err := netlinkLinkByName(m.name)
	if err != nil {
		return fmt.Errorf("enslave %q: get VRF %q (Setup not called?): %w", ifaceName, m.name, err)
	}
	if err := netlinkLinkSetMaster(child, vrfLink); err != nil {
		return fmt.Errorf("enslave %q to VRF %q: %w", ifaceName, m.name, err)
	}
	return nil
}

// UnslaveInterface removes the named interface from the VRF. The call is
// serialised by m.mu so it is safe to call concurrently with EnslaveInterface.
func (m *VRFManager) UnslaveInterface(ifaceName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	child, err := netlinkLinkByName(ifaceName)
	if err != nil {
		return fmt.Errorf("unslave %q: get interface: %w", ifaceName, err)
	}
	if err := netlinkLinkSetNoMaster(child); err != nil {
		return fmt.Errorf("unslave %q from VRF %q: %w", ifaceName, m.name, err)
	}
	return nil
}

// Teardown removes the anchor dummy interface and the VRF master device. Any
// previously enslaved interfaces are automatically unenslaved by the kernel.
// Errors from individual delete operations are silently ignored and do not
// abort the teardown sequence — the method always returns nil. Callers that
// need diagnostic visibility on teardown errors should wrap VRFManager and
// observe via netlink themselves.
func (m *VRFManager) Teardown() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if anchorLink, err := netlinkLinkByName(m.anchorName); err == nil {
		if delErr := netlinkLinkDel(anchorLink); delErr != nil {
			// Best-effort: warn and continue.
			_ = delErr
		}
	}

	if vrfLink, err := netlinkLinkByName(m.name); err == nil {
		if _, ok := vrfLink.(*netlink.Vrf); ok {
			if delErr := netlinkLinkDel(vrfLink); delErr != nil {
				_ = delErr
			}
		}
	}

	m.created = false
	return nil
}

// IsCreated reports whether Setup has successfully completed at least once.
func (m *VRFManager) IsCreated() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.created
}

// Name returns the VRF master device name supplied at construction.
func (m *VRFManager) Name() string { return m.name }

// Table returns the routing table number supplied at construction.
func (m *VRFManager) Table() uint32 { return m.table }

// IsVRFSupported probes the kernel for VRF capability by attempting to create
// a temporary probe VRF and immediately deleting it. The result is cached via
// sync.Once so the kernel call is made at most once per process.
//
// Error classification (per FR-10.5):
//   - EOPNOTSUPP or "not supported" string → reason=kernel_too_old
//   - ENODEV or "module" / "no such device" string → reason=module_not_loaded
//   - Other errors → reason=unknown
func IsVRFSupported() error {
	isVRFSupportedOnce.Do(func() {
		probeName := fmt.Sprintf("vrfprobe%d", os.Getpid())
		probeTable := uint32(9999)
		probe := &netlink.Vrf{
			LinkAttrs: netlink.LinkAttrs{Name: probeName},
			Table:     probeTable,
		}
		if err := netlinkLinkAdd(probe); err != nil {
			isVRFSupportedResult = classifyVRFError(err)
			return
		}
		// Probe succeeded — clean up immediately.
		if probeLink, err := netlinkLinkByName(probeName); err == nil {
			_ = netlinkLinkDel(probeLink)
		}
	})
	return isVRFSupportedResult
}

// classifyVRFError maps a raw netlink error from a VRF LinkAdd attempt to a
// structured error with a reason tag so operators get actionable messages.
func classifyVRFError(err error) error {
	if errors.Is(err, unix.EOPNOTSUPP) {
		return fmt.Errorf("vrf_unsupported reason=kernel_too_old: %w", err)
	}
	if errors.Is(err, unix.ENODEV) {
		return fmt.Errorf("vrf_unsupported reason=module_not_loaded: run 'modprobe vrf' to load the kernel module: %w", err)
	}
	// Fall back to string heuristics for kernels that wrap the errno differently.
	msg := err.Error()
	if strings.Contains(msg, "not supported") || strings.Contains(msg, "operation not supported") {
		return fmt.Errorf("vrf_unsupported reason=kernel_too_old: %w", err)
	}
	if strings.Contains(msg, "module") || strings.Contains(msg, "no such device") {
		return fmt.Errorf("vrf_unsupported reason=module_not_loaded: run 'modprobe vrf' to load the kernel module: %w", err)
	}
	return fmt.Errorf("vrf_unsupported reason=unknown: %w", err)
}
