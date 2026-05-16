package clientd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/role"
	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/wg"
	pb "github.com/coonfuuseed-paandaa/awg-mesh/v2/proto/control_plane"
)

// Config describes the node identity and local clientd runtime settings.
type Config struct {
	NodeName      string
	Roles         []role.Role
	OverlayIP     string
	Region        string
	NodeCertPEM   []byte
	CertPath      string
	KeyPath       string
	Version       string
	InterfaceName string
	Protocol      wg.Protocol
	PublicKey     wg.Key
	StatePath     string
	ApplyTimeout  time.Duration
}

// State is the immutable clientd view of control-plane peer and ownership snapshots.
type State struct {
	Peers            []PeerEntry      `json:"peers"`
	Ownership        []OwnershipEntry `json:"ownership"`
	PeerListVersion  int64            `json:"peer_list_version"`
	OwnershipVersion int64            `json:"ownership_version"`
}

// PeerEntry is the internal peer representation used by reload validation and caching.
type PeerEntry struct {
	PeerName                string      `json:"peer_name"`
	PeerRole                role.Role   `json:"peer_role,omitempty"`
	PeerOverlayIP           string      `json:"peer_overlay_ip"`
	PeerPubkey              []byte      `json:"peer_pubkey,omitempty"`
	PeerEndpointHost        string      `json:"peer_endpoint_host,omitempty"`
	AllowedIPs              []string    `json:"allowed_ips,omitempty"`
	PersistentKeepaliveSecs int32       `json:"persistent_keepalive_secs,omitempty"`
	Protocol                wg.Protocol `json:"protocol,omitempty"`
}

// OwnershipEntry is the internal ownership-ledger representation used by caching.
type OwnershipEntry struct {
	OverlayIP            string `json:"overlay_ip"`
	OwningMaster         string `json:"owning_master"`
	LastReassignedAtUnix int64  `json:"last_reassigned_at_unix"`
	Reason               string `json:"reason,omitempty"`
}

// Configurator applies an immutable clientd state to local networking.
type Configurator interface {
	Apply(ctx context.Context, state State) error
}

// Agent registers clientd with the control plane, consumes streams, persists LKG state, and applies updates.
type Agent struct {
	cfg          Config
	client       pb.ControlPlaneClient
	configurator Configurator
	cache        *StateCache
}

// NewAgent constructs an Agent with explicit dependencies for testability.
func NewAgent(cfg Config, client pb.ControlPlaneClient, configurator Configurator) (*Agent, error) {
	if strings.TrimSpace(cfg.NodeName) == "" {
		return nil, errors.New("node name is required")
	}
	if len(cfg.Roles) == 0 {
		return nil, errors.New("at least one role is required")
	}
	if err := role.ValidateComposability(cfg.Roles); err != nil {
		return nil, fmt.Errorf("validate roles: %w", err)
	}
	if strings.TrimSpace(cfg.OverlayIP) == "" {
		return nil, errors.New("overlay IP is required")
	}
	if strings.TrimSpace(cfg.Region) == "" {
		return nil, errors.New("region is required")
	}
	if strings.TrimSpace(cfg.Version) == "" {
		return nil, errors.New("version is required")
	}
	if cfg.Protocol != wg.ProtocolVanilla && cfg.Protocol != wg.ProtocolAmneziaWG {
		return nil, fmt.Errorf("unsupported protocol %q", cfg.Protocol)
	}
	if (strings.TrimSpace(cfg.CertPath) == "") != (strings.TrimSpace(cfg.KeyPath) == "") {
		return nil, errors.New("cert update requires both cert and key paths")
	}
	if client == nil {
		return nil, errors.New("control-plane client is required")
	}
	if configurator == nil {
		return nil, errors.New("configurator is required")
	}
	statePath := cfg.StatePath
	if strings.TrimSpace(statePath) == "" {
		statePath = filepath.Join(os.TempDir(), "awg-mesh-clientd-state.json")
		cfg.StatePath = statePath
	}
	return &Agent{cfg: cfg, client: client, configurator: configurator, cache: NewStateCache(statePath)}, nil
}

// Run registers the node and processes control-plane streams until cancellation or stream failure.
func (a *Agent) Run(ctx context.Context) error {
	state, err := a.cache.Load()
	if err != nil {
		return err
	}
	if err := a.register(ctx); err != nil {
		return err
	}
	if !state.IsZero() {
		if err := a.apply(ctx, state); err != nil {
			return err
		}
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	peerStream, err := a.client.StreamPeerList(runCtx, &pb.StreamPeerListRequest{NodeName: a.cfg.NodeName})
	if err != nil {
		return fmt.Errorf("open peer-list stream: %w", err)
	}
	ownershipStream, err := a.client.StreamOwnership(runCtx, &pb.StreamOwnershipRequest{SubscriberNode: a.cfg.NodeName})
	if err != nil {
		return fmt.Errorf("open ownership stream: %w", err)
	}
	var certStream pb.ControlPlane_StreamCertUpdateClient
	if strings.TrimSpace(a.cfg.CertPath) != "" {
		certStream, err = a.client.StreamCertUpdate(runCtx, &pb.StreamCertRequest{NodeName: a.cfg.NodeName})
		if err != nil {
			return fmt.Errorf("open cert-update stream: %w", err)
		}
	}

	updates := make(chan streamUpdate, 3)
	go recvPeerUpdates(runCtx, peerStream, updates)
	go recvOwnershipUpdates(runCtx, ownershipStream, updates)
	if certStream != nil {
		go recvCertUpdates(runCtx, certStream, updates)
	}

	peerSnapshotSeen := false
	ownershipSnapshotSeen := false
	for {
		select {
		case <-runCtx.Done():
			return nil
		case update := <-updates:
			if update.err != nil {
				if runCtx.Err() != nil || errors.Is(update.err, context.Canceled) || errors.Is(update.err, context.DeadlineExceeded) {
					return nil
				}
				if errors.Is(update.err, io.EOF) {
					return fmt.Errorf("control-plane stream closed before shutdown: %w", update.err)
				}
				return update.err
			}
			var changed bool
			if update.peers != nil {
				if !peerSnapshotSeen {
					state, changed = state.WithPeerListSnapshot(update.peers)
					peerSnapshotSeen = true
				} else {
					state, changed = state.WithPeerList(update.peers)
				}
			}
			if update.ownership != nil {
				if !ownershipSnapshotSeen {
					state, changed = state.WithOwnershipSnapshot(update.ownership)
					ownershipSnapshotSeen = true
				} else {
					state, changed = state.WithOwnership(update.ownership)
				}
			}
			if update.cert != nil {
				if err := ApplyCertUpdate(a.cfg.CertPath, a.cfg.KeyPath, update.cert); err != nil {
					return err
				}
				a.cfg.NodeCertPEM = append([]byte(nil), update.cert.GetCertPem()...)
				if err := a.register(runCtx); err != nil {
					return fmt.Errorf("re-register after cert update: %w", err)
				}
				continue
			}
			if !changed {
				continue
			}
			if err := a.cache.Save(state); err != nil {
				return err
			}
			if err := a.apply(ctx, state); err != nil {
				return err
			}
		}
	}
}

func (a *Agent) register(ctx context.Context) error {
	roles := make([]string, 0, len(a.cfg.Roles))
	for _, r := range a.cfg.Roles {
		roles = append(roles, string(r))
	}
	resp, err := a.client.RegisterNode(ctx, &pb.RegisterNodeRequest{
		NodeName:    a.cfg.NodeName,
		Roles:       roles,
		NodeCertPem: append([]byte(nil), a.cfg.NodeCertPEM...),
		NodeVersion: a.cfg.Version,
		OverlayIp:   a.cfg.OverlayIP,
		Region:      a.cfg.Region,
		Pubkey:      publicKeyBytes(a.cfg.PublicKey),
		Protocol:    string(a.cfg.Protocol),
	})
	if err != nil {
		return fmt.Errorf("register node: %w", err)
	}
	if resp != nil && !resp.Accepted {
		return fmt.Errorf("register node rejected: %s", resp.RejectReason)
	}
	return nil
}

func publicKeyBytes(key wg.Key) []byte {
	if key.IsZero() {
		return nil
	}
	return append([]byte(nil), key[:]...)
}

func (a *Agent) apply(ctx context.Context, state State) error {
	applyCtx := ctx
	cancel := func() {}
	if a.cfg.ApplyTimeout > 0 {
		applyCtx, cancel = context.WithTimeout(ctx, a.cfg.ApplyTimeout)
	}
	defer cancel()
	if err := a.configurator.Apply(applyCtx, state.Clone()); err != nil {
		return fmt.Errorf("apply clientd state: %w", err)
	}
	return nil
}

type streamUpdate struct {
	peers     *pb.PeerListUpdate
	ownership *pb.OwnershipUpdate
	cert      *pb.CertUpdate
	err       error
}

func recvPeerUpdates(ctx context.Context, stream pb.ControlPlane_StreamPeerListClient, updates chan<- streamUpdate) {
	for {
		msg, err := stream.Recv()
		if err != nil && ctx.Err() != nil {
			return
		}
		update := streamUpdate{peers: msg, err: err}
		if err != nil {
			update.err = fmt.Errorf("peer-list stream ended: %w", err)
		}
		select {
		case updates <- update:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

func recvOwnershipUpdates(ctx context.Context, stream pb.ControlPlane_StreamOwnershipClient, updates chan<- streamUpdate) {
	for {
		msg, err := stream.Recv()
		if err != nil && ctx.Err() != nil {
			return
		}
		update := streamUpdate{ownership: msg, err: err}
		if err != nil {
			update.err = fmt.Errorf("ownership stream ended: %w", err)
		}
		select {
		case updates <- update:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

func recvCertUpdates(ctx context.Context, stream pb.ControlPlane_StreamCertUpdateClient, updates chan<- streamUpdate) {
	for {
		msg, err := stream.Recv()
		if err != nil && ctx.Err() != nil {
			return
		}
		if errors.Is(err, io.EOF) {
			return
		}
		update := streamUpdate{cert: msg, err: err}
		if err != nil {
			update.err = fmt.Errorf("cert-update stream ended: %w", err)
		}
		select {
		case updates <- update:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

// Clone returns a deep copy of the state.
func (s State) Clone() State {
	out := State{PeerListVersion: s.PeerListVersion, OwnershipVersion: s.OwnershipVersion}
	out.Peers = clonePeers(s.Peers)
	out.Ownership = cloneOwnership(s.Ownership)
	return out
}

// IsZero reports whether the state has no stream data.
func (s State) IsZero() bool {
	return len(s.Peers) == 0 && len(s.Ownership) == 0 && s.PeerListVersion == 0 && s.OwnershipVersion == 0
}

// WithPeerList returns a new state if update is newer than the current peer-list version.
func (s State) WithPeerList(update *pb.PeerListUpdate) (State, bool) {
	if update == nil || update.Version <= s.PeerListVersion {
		return s.Clone(), false
	}
	return s.withPeerList(update), true
}

// WithPeerListSnapshot returns a new state from the first full snapshot on a
// newly opened stream. Stream versions are process-local, while the cache
// survives control-plane restarts, so the first snapshot is authoritative even
// when its version is not greater than the cached version.
func (s State) WithPeerListSnapshot(update *pb.PeerListUpdate) (State, bool) {
	if update == nil {
		return s.Clone(), false
	}
	return s.withPeerList(update), true
}

func (s State) withPeerList(update *pb.PeerListUpdate) State {
	out := s.Clone()
	out.PeerListVersion = update.Version
	out.Peers = make([]PeerEntry, 0, len(update.Peers))
	for _, p := range update.Peers {
		if p == nil {
			continue
		}
		out.Peers = append(out.Peers, PeerEntry{
			PeerName:                p.PeerName,
			PeerOverlayIP:           p.PeerOverlayIp,
			PeerPubkey:              append([]byte(nil), p.PeerPubkey...),
			PeerEndpointHost:        p.PeerEndpointHost,
			AllowedIPs:              append([]string(nil), p.AllowedIps...),
			PersistentKeepaliveSecs: p.PersistentKeepaliveSecs,
			Protocol:                wg.Protocol(p.Protocol),
		})
	}
	return out
}

// WithOwnership returns a new state if update is newer than the current ownership version.
func (s State) WithOwnership(update *pb.OwnershipUpdate) (State, bool) {
	if update == nil || update.Version <= s.OwnershipVersion {
		return s.Clone(), false
	}
	return s.withOwnership(update), true
}

// WithOwnershipSnapshot returns a new state from the first full snapshot on a
// newly opened stream. Ownership versions are process-local, while cached state
// can outlive the control-plane process that produced it.
func (s State) WithOwnershipSnapshot(update *pb.OwnershipUpdate) (State, bool) {
	if update == nil {
		return s.Clone(), false
	}
	return s.withOwnership(update), true
}

func (s State) withOwnership(update *pb.OwnershipUpdate) State {
	out := s.Clone()
	out.OwnershipVersion = update.Version
	out.Ownership = make([]OwnershipEntry, 0, len(update.Entries))
	for _, e := range update.Entries {
		if e == nil {
			continue
		}
		out.Ownership = append(out.Ownership, OwnershipEntry{
			OverlayIP:            e.OverlayIp,
			OwningMaster:         e.OwningMaster,
			LastReassignedAtUnix: e.LastReassignedAtUnix,
			Reason:               e.Reason,
		})
	}
	return out
}

func clonePeers(in []PeerEntry) []PeerEntry {
	out := make([]PeerEntry, len(in))
	for i, p := range in {
		out[i] = p
		out[i].PeerPubkey = append([]byte(nil), p.PeerPubkey...)
		out[i].AllowedIPs = append([]string(nil), p.AllowedIPs...)
	}
	return out
}

func cloneOwnership(in []OwnershipEntry) []OwnershipEntry {
	return append([]OwnershipEntry(nil), in...)
}

// StateCache stores last-known-good clientd state using atomic temp-file + rename writes.
type StateCache struct {
	path string
	mu   sync.Mutex
}

// NewStateCache creates a state cache for path.
func NewStateCache(path string) *StateCache {
	return &StateCache{path: path}
}

// Load returns an empty state when the cache file is missing.
func (c *StateCache) Load() (State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	b, err := os.ReadFile(c.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{}, nil
		}
		return State{}, fmt.Errorf("read clientd state cache: %w", err)
	}
	var state State
	if err := json.Unmarshal(b, &state); err != nil {
		return State{}, fmt.Errorf("parse clientd state cache: %w", err)
	}
	return state.Clone(), nil
}

// Save atomically writes state to the cache path.
func (c *StateCache) Save(state State) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return fmt.Errorf("create clientd state directory: %w", err)
	}
	b, err := json.MarshalIndent(state.Clone(), "", "  ")
	if err != nil {
		return fmt.Errorf("encode clientd state cache: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.path), filepath.Base(c.path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create clientd state temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write clientd state temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close clientd state temp file: %w", err)
	}
	if err := os.Rename(tmpName, c.path); err != nil {
		return fmt.Errorf("rename clientd state cache: %w", err)
	}
	return nil
}
