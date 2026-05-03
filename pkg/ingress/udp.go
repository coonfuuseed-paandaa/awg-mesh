package ingress

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"
)

type UDPFlow struct {
	Client    string
	Hostname  string
	Target    string
	LastSeen  time.Time
	CreatedAt time.Time
}

type UDPFlowTable struct {
	mu      sync.Mutex
	timeout time.Duration
	flows   map[string]UDPFlow
}

func NewUDPFlowTable(timeout time.Duration) *UDPFlowTable {
	if timeout <= 0 {
		timeout = DefaultUDPIdleTimeout
	}
	return &UDPFlowTable{timeout: timeout, flows: make(map[string]UDPFlow)}
}

func (t *UDPFlowTable) Resolve(client net.Addr, route Route, now time.Time) UDPFlow {
	if now.IsZero() {
		now = time.Now()
	}
	key := udpFlowKey(client, route)
	t.mu.Lock()
	defer t.mu.Unlock()
	flow, exists := t.flows[key]
	if !exists {
		flow = UDPFlow{
			Client:    client.String(),
			Hostname:  route.Hostname,
			Target:    route.Target,
			CreatedAt: now,
		}
	}
	flow.LastSeen = now
	t.flows[key] = flow
	return flow
}

func (t *UDPFlowTable) Expire(now time.Time) int {
	if now.IsZero() {
		now = time.Now()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	removed := 0
	next := make(map[string]UDPFlow, len(t.flows))
	for key, flow := range t.flows {
		if now.Sub(flow.LastSeen) > t.timeout {
			removed++
			continue
		}
		next[key] = flow
	}
	t.flows = next
	return removed
}

func (t *UDPFlowTable) Count(route Route) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	count := 0
	for _, flow := range t.flows {
		if flow.Hostname == route.Hostname {
			count++
		}
	}
	return count
}

type UDPPacketSender interface {
	Send(ctx context.Context, target string, payload []byte) error
}

type UDPForwarder struct {
	registry *Registry
	health   *HealthTracker
	table    *UDPFlowTable
	sender   UDPPacketSender
	metrics  *Metrics
}

func NewUDPForwarder(registry *Registry, health *HealthTracker, table *UDPFlowTable, sender UDPPacketSender, metrics *Metrics) *UDPForwarder {
	if table == nil {
		table = NewUDPFlowTable(DefaultUDPIdleTimeout)
	}
	if sender == nil {
		sender = netUDPSender{}
	}
	return &UDPForwarder{registry: registry, health: health, table: table, sender: sender, metrics: metrics}
}

func (f *UDPForwarder) Forward(ctx context.Context, client net.Addr, hostname string, payload []byte, now time.Time) (UDPFlow, error) {
	if f == nil || f.registry == nil {
		return UDPFlow{}, fmt.Errorf("udp forwarder is not configured")
	}
	route, ok := f.registry.Lookup(hostname)
	if !ok {
		if f.metrics != nil {
			f.metrics.RecordRejection(ProtocolUDP, hostname, "unknown-host")
		}
		return UDPFlow{}, fmt.Errorf("udp route for hostname %q not found", hostname)
	}
	if route.Protocol != ProtocolUDP {
		if f.metrics != nil {
			f.metrics.RecordRejection(ProtocolUDP, route.Hostname, "protocol-mismatch")
		}
		return UDPFlow{}, fmt.Errorf("route %q is protocol %q, not udp", route.Hostname, route.Protocol)
	}
	if f.health != nil && !f.health.IsHealthy(route) {
		if f.metrics != nil {
			f.metrics.RecordProxyError(route, "target-unhealthy")
		}
		return UDPFlow{}, fmt.Errorf("udp target %q is unhealthy", route.Target)
	}
	flow := f.table.Resolve(client, route, now)
	if err := f.sender.Send(ctx, route.Target, append([]byte(nil), payload...)); err != nil {
		if f.metrics != nil {
			f.metrics.RecordProxyError(route, "send-error")
		}
		return UDPFlow{}, err
	}
	if f.metrics != nil {
		f.metrics.RecordRequest(route)
		f.metrics.SetActiveUDPFlows(route, f.table.Count(route))
	}
	return flow, nil
}

type netUDPSender struct{}

func (netUDPSender) Send(ctx context.Context, target string, payload []byte) error {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "udp", target)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write(payload)
	return err
}

func udpFlowKey(client net.Addr, route Route) string {
	clientKey := ""
	if client != nil {
		clientKey = client.String()
	}
	return clientKey + "|" + route.Hostname + "|" + route.Target
}
