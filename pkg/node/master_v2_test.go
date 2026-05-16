package node

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/wg"
)

type fakeMasterTransport struct {
	name       string
	protocol   wg.Protocol
	pubkey     wg.Key
	statsStart chan struct{}
	statsBlock chan struct{}
	statsOnce  sync.Once
	closeCount int
}

func (t *fakeMasterTransport) Protocol() wg.Protocol       { return t.protocol }
func (t *fakeMasterTransport) Name() string                { return t.name }
func (t *fakeMasterTransport) Configure(wg.Config) error   { return nil }
func (t *fakeMasterTransport) AddPeer(wg.PeerConfig) error { return nil }
func (t *fakeMasterTransport) RemovePeer(wg.Key) error     { return nil }
func (t *fakeMasterTransport) Stats() (*wg.Device, error) {
	if t.statsStart != nil {
		t.statsOnce.Do(func() { close(t.statsStart) })
	}
	if t.statsBlock != nil {
		<-t.statsBlock
	}
	return &wg.Device{Name: t.name, PublicKey: t.pubkey}, nil
}
func (t *fakeMasterTransport) Close() error {
	t.closeCount++
	return nil
}

func TestNewMasterValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     MasterConfig
		wantErr string
	}{
		{name: "missing name", cfg: MasterConfig{OverlayIP: "172.21.92.2"}, wantErr: "name is required"},
		{name: "missing overlay", cfg: MasterConfig{Name: "master-01"}, wantErr: "overlay IP is required"},
		{name: "bad overlay", cfg: MasterConfig{Name: "master-01", OverlayIP: "not-ip"}, wantErr: "parse master overlay IP"},
		{
			name: "coordination missing mesh endpoint",
			cfg: MasterConfig{
				Name:      "master-01",
				OverlayIP: "172.21.92.2",
				Coordination: &MasterCoordinationConfig{
					ListenAddr: "127.0.0.1:0",
					StateDir:   t.TempDir(),
				},
			},
			wantErr: "mesh endpoint is required",
		},
		{
			name: "coordination invalid mesh endpoint",
			cfg: MasterConfig{
				Name:             "master-01",
				OverlayIP:        "172.21.92.2",
				MeshEndpointHost: "203.0.113.10",
				Coordination: &MasterCoordinationConfig{
					ListenAddr: "127.0.0.1:0",
					StateDir:   t.TempDir(),
				},
			},
			wantErr: "must be host:port",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewMaster(tt.cfg)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestMasterRunStartsAndClosesOnCancel(t *testing.T) {
	t.Parallel()

	var clientTransport *fakeMasterTransport
	var meshTransport *fakeMasterTransport
	master, err := NewMaster(MasterConfig{
		Name:      "master-01",
		OverlayIP: "172.21.92.2",
		DualListener: wg.DualListenerConfig{
			VanillaFactory: func(name string) (wg.Transport, error) {
				clientTransport = &fakeMasterTransport{name: name, protocol: wg.ProtocolVanilla}
				return clientTransport, nil
			},
			AWGFactory: func(name string) (wg.Transport, error) {
				meshTransport = &fakeMasterTransport{name: name, protocol: wg.ProtocolAmneziaWG}
				return meshTransport, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewMaster: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- master.Run(ctx)
	}()

	waitFor(t, func() bool { return master.Status().Listeners.Started })
	status := master.Status()
	if status.Coordination.Enabled || status.Coordination.Started || status.Coordination.BoundAddr != "" {
		t.Fatalf("master without coordination config must keep coordination disabled, got %#v", status.Coordination)
	}
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run returned error after cancel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("master Run did not return after cancellation")
	}
	if clientTransport.closeCount != 1 || meshTransport.closeCount != 1 {
		t.Fatalf("expected both transports closed once, got client=%d mesh=%d", clientTransport.closeCount, meshTransport.closeCount)
	}
}

func TestMasterRunReturnsStartupError(t *testing.T) {
	t.Parallel()

	master, err := NewMaster(MasterConfig{
		Name:      "master-01",
		OverlayIP: "172.21.92.2",
		DualListener: wg.DualListenerConfig{
			VanillaFactory: func(name string) (wg.Transport, error) {
				return &fakeMasterTransport{name: name, protocol: wg.ProtocolVanilla}, nil
			},
			AWGFactory: func(name string) (wg.Transport, error) {
				return nil, errors.New("mesh startup failed")
			},
		},
	})
	if err != nil {
		t.Fatalf("NewMaster: %v", err)
	}

	err = master.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "mesh startup failed") {
		t.Fatalf("expected startup error, got %v", err)
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition was not met before deadline")
}
