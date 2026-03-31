package cmd

import (
	"fmt"
	"net/netip"

	"github.com/spf13/cobra"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
)

func newIPCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ip",
		Short: "Manage overlay IP ranges",
	}

	cmd.AddCommand(newIPListCommand())
	cmd.AddCommand(newIPRangeCommand())

	return cmd
}

func newIPListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all overlay IP ranges",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			topo, err := topology.LoadTopology(topologyPath)
			if err != nil {
				return fmt.Errorf("load topology %q: %w", topologyPath, err)
			}

			fmt.Printf("Overlay space: %s\n\n", topo.Overlay.Space)
			if len(topo.Overlay.Ranges) == 0 {
				fmt.Println("No ranges defined.")
				return nil
			}

			fmt.Printf("%-20s %-20s %-16s\n", "NAME", "CIDR", "BALANCER_IP")
			for _, r := range topo.Overlay.Ranges {
				balancer := r.BalancerIP
				if balancer == "" {
					balancer = "-"
				}
				fmt.Printf("%-20s %-20s %-16s\n", r.Name, r.CIDR, balancer)
			}
			return nil
		},
	}
}

func newIPRangeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "range",
		Short: "Manage overlay IP ranges",
	}

	cmd.AddCommand(newIPRangeAddCommand())
	cmd.AddCommand(newIPRangeResizeCommand())
	cmd.AddCommand(newIPRangeRenameCommand())
	cmd.AddCommand(newIPRangeDeleteCommand())
	cmd.AddCommand(newIPRangeSetBalancerCommand())

	return cmd
}

func newIPRangeAddCommand() *cobra.Command {
	var cidr string
	var balancerIP string

	cmd := &cobra.Command{
		Use:   "add [name]",
		Short: "Add a new overlay IP range",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			if cidr == "" {
				return fmt.Errorf("--cidr is required")
			}

			topo, err := topology.LoadTopology(topologyPath)
			if err != nil {
				return fmt.Errorf("load topology %q: %w", topologyPath, err)
			}

			// Ensure name is unique.
			for _, r := range topo.Overlay.Ranges {
				if r.Name == name {
					return fmt.Errorf("range %q already exists", name)
				}
			}

			newNamedRange := topology.NamedRange{
				Name:       name,
				CIDR:       cidr,
				BalancerIP: balancerIP,
			}

			newRange, err := topology.ParseRange(newNamedRange)
			if err != nil {
				return fmt.Errorf("parse new range: %w", err)
			}

			// Check overlap against existing ranges.
			for _, existing := range topo.Overlay.Ranges {
				existingRange, err := topology.ParseRange(existing)
				if err != nil {
					continue
				}
				if topology.RangesOverlap(newRange, existingRange) {
					return fmt.Errorf("new range %q overlaps with existing range %q", name, existing.Name)
				}
			}

			// Validate balancer IP is inside the new range.
			if balancerIP != "" {
				bip, err := netip.ParseAddr(balancerIP)
				if err != nil {
					return fmt.Errorf("parse balancer IP %q: %w", balancerIP, err)
				}
				if !newRange.Contains(bip) {
					return fmt.Errorf("balancer IP %s is not inside range %s", balancerIP, cidr)
				}
			}

			topo.Overlay.Ranges = append(topo.Overlay.Ranges, newNamedRange)

			if err := topology.SaveTopology(topologyPath, topo); err != nil {
				return fmt.Errorf("save topology: %w", err)
			}

			fmt.Printf("Added range %q (%s).\n", name, cidr)
			return nil
		},
	}

	cmd.Flags().StringVar(&cidr, "cidr", "", "CIDR block for the range (e.g. 10.100.0.0/24)")
	cmd.Flags().StringVar(&balancerIP, "balancer-ip", "", "Optional balancer IP within the range")

	return cmd
}

func newIPRangeResizeCommand() *cobra.Command {
	var cidr string

	cmd := &cobra.Command{
		Use:   "resize [name]",
		Short: "Change the CIDR of an existing range",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			if cidr == "" {
				return fmt.Errorf("--cidr is required")
			}

			topo, err := topology.LoadTopology(topologyPath)
			if err != nil {
				return fmt.Errorf("load topology %q: %w", topologyPath, err)
			}

			idx := findRangeIndex(topo, name)
			if idx < 0 {
				return fmt.Errorf("range %q not found", name)
			}

			newNamedRange := topology.NamedRange{
				Name:       name,
				CIDR:       cidr,
				BalancerIP: topo.Overlay.Ranges[idx].BalancerIP,
			}

			newRange, err := topology.ParseRange(newNamedRange)
			if err != nil {
				return fmt.Errorf("parse new CIDR: %w", err)
			}

			// Check overlap against all other ranges.
			for i, existing := range topo.Overlay.Ranges {
				if i == idx {
					continue
				}
				existingRange, err := topology.ParseRange(existing)
				if err != nil {
					continue
				}
				if topology.RangesOverlap(newRange, existingRange) {
					return fmt.Errorf("new CIDR %s overlaps with range %q", cidr, existing.Name)
				}
			}

			// If balancer IP was set, verify it still falls within the new CIDR.
			if topo.Overlay.Ranges[idx].BalancerIP != "" {
				bip, err := netip.ParseAddr(topo.Overlay.Ranges[idx].BalancerIP)
				if err == nil && !newRange.Contains(bip) {
					return fmt.Errorf("existing balancer IP %s is outside new CIDR %s; update it first with set-balancer", topo.Overlay.Ranges[idx].BalancerIP, cidr)
				}
			}

			topo.Overlay.Ranges[idx].CIDR = cidr

			if err := topology.SaveTopology(topologyPath, topo); err != nil {
				return fmt.Errorf("save topology: %w", err)
			}

			fmt.Printf("Resized range %q to %s.\n", name, cidr)
			return nil
		},
	}

	cmd.Flags().StringVar(&cidr, "cidr", "", "New CIDR block for the range")

	return cmd
}

func newIPRangeRenameCommand() *cobra.Command {
	var newName string

	cmd := &cobra.Command{
		Use:   "rename [name]",
		Short: "Rename an overlay IP range",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			if newName == "" {
				return fmt.Errorf("--new-name is required")
			}

			topo, err := topology.LoadTopology(topologyPath)
			if err != nil {
				return fmt.Errorf("load topology %q: %w", topologyPath, err)
			}

			idx := findRangeIndex(topo, name)
			if idx < 0 {
				return fmt.Errorf("range %q not found", name)
			}

			// Ensure the new name is not already taken.
			for i, r := range topo.Overlay.Ranges {
				if i != idx && r.Name == newName {
					return fmt.Errorf("range %q already exists", newName)
				}
			}

			topo.Overlay.Ranges[idx].Name = newName

			if err := topology.SaveTopology(topologyPath, topo); err != nil {
				return fmt.Errorf("save topology: %w", err)
			}

			fmt.Printf("Renamed range %q to %q.\n", name, newName)
			return nil
		},
	}

	cmd.Flags().StringVar(&newName, "new-name", "", "New name for the range")

	return cmd
}

func newIPRangeDeleteCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "delete [name]",
		Short: "Delete an overlay IP range",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			topo, err := topology.LoadTopology(topologyPath)
			if err != nil {
				return fmt.Errorf("load topology %q: %w", topologyPath, err)
			}

			idx := findRangeIndex(topo, name)
			if idx < 0 {
				return fmt.Errorf("range %q not found", name)
			}

			// Check that no node has an overlay IP within this range.
			r, err := topology.ParseRange(topo.Overlay.Ranges[idx])
			if err != nil {
				return fmt.Errorf("parse range: %w", err)
			}

			if err := checkNodesUseRange(topo, r, name); err != nil {
				return err
			}

			// Remove the range by swapping with the last element and truncating.
			last := len(topo.Overlay.Ranges) - 1
			topo.Overlay.Ranges[idx] = topo.Overlay.Ranges[last]
			topo.Overlay.Ranges = topo.Overlay.Ranges[:last]

			if err := topology.SaveTopology(topologyPath, topo); err != nil {
				return fmt.Errorf("save topology: %w", err)
			}

			fmt.Printf("Deleted range %q.\n", name)
			return nil
		},
	}
}

func newIPRangeSetBalancerCommand() *cobra.Command {
	var balancerIP string

	cmd := &cobra.Command{
		Use:   "set-balancer [name]",
		Short: "Set the balancer IP of an overlay range",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			if balancerIP == "" {
				return fmt.Errorf("--balancer-ip is required")
			}

			topo, err := topology.LoadTopology(topologyPath)
			if err != nil {
				return fmt.Errorf("load topology %q: %w", topologyPath, err)
			}

			idx := findRangeIndex(topo, name)
			if idx < 0 {
				return fmt.Errorf("range %q not found", name)
			}

			r, err := topology.ParseRange(topo.Overlay.Ranges[idx])
			if err != nil {
				return fmt.Errorf("parse range: %w", err)
			}

			bip, err := netip.ParseAddr(balancerIP)
			if err != nil {
				return fmt.Errorf("parse balancer IP %q: %w", balancerIP, err)
			}

			if !r.Contains(bip) {
				return fmt.Errorf("balancer IP %s is not inside range %s (%s)", balancerIP, name, topo.Overlay.Ranges[idx].CIDR)
			}

			topo.Overlay.Ranges[idx].BalancerIP = balancerIP

			if err := topology.SaveTopology(topologyPath, topo); err != nil {
				return fmt.Errorf("save topology: %w", err)
			}

			fmt.Printf("Set balancer IP for range %q to %s.\n", name, balancerIP)
			return nil
		},
	}

	cmd.Flags().StringVar(&balancerIP, "balancer-ip", "", "Balancer IP within the range")

	return cmd
}

// findRangeIndex returns the index of a named range in the topology, or -1 if not found.
func findRangeIndex(topo *topology.Topology, name string) int {
	for i, r := range topo.Overlay.Ranges {
		if r.Name == name {
			return i
		}
	}
	return -1
}

// checkNodesUseRange returns an error if any node in the topology has an overlay IP inside r.
func checkNodesUseRange(topo *topology.Topology, r topology.Range, rangeName string) error {
	for _, master := range topo.Masters {
		ip, err := netip.ParseAddr(master.OverlayIP)
		if err == nil && r.Contains(ip) {
			return fmt.Errorf("cannot delete range %q: master %q has overlay IP %s inside it", rangeName, master.Name, master.OverlayIP)
		}
	}
	for _, ep := range topo.Endpoints {
		ip, err := netip.ParseAddr(ep.OverlayIP)
		if err == nil && r.Contains(ip) {
			return fmt.Errorf("cannot delete range %q: endpoint %q has overlay IP %s inside it", rangeName, ep.Name, ep.OverlayIP)
		}
	}
	for _, client := range topo.Clients {
		ip, err := netip.ParseAddr(client.OverlayIP)
		if err == nil && r.Contains(ip) {
			return fmt.Errorf("cannot delete range %q: client %q has overlay IP %s inside it", rangeName, client.Name, client.OverlayIP)
		}
	}
	return nil
}
