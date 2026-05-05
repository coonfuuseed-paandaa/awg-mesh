package node

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/wg"
)

type fakeMasterTransport struct {
	name       string
	protocol   wg.Protocol
	closeCount int
}

func (t *fakeMasterTransport) Protocol() wg.Protocol       { return t.protocol }
func (t *fakeMasterTransport) Name() string                { return t.name }
func (t *fakeMasterTransport) Configure(wg.Config) error   { return nil }
func (t *fakeMasterTransport) AddPeer(wg.PeerConfig) error { return nil }
func (t *fakeMasterTransport) RemovePeer(wg.Key) error     { return nil }
func (t *fakeMasterTransport) Stats() (*wg.Device, error)  { return &wg.Device{Name: t.name}, nil }
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
