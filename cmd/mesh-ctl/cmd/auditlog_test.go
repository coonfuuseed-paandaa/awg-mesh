package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	controlpb "github.com/coonfuuseed-paandaa/awg-mesh/proto/control_plane"
	"google.golang.org/protobuf/proto"
)

func TestRunAuditLogQueryCommandSendsFiltersAndOutputsJSON(t *testing.T) {
	configDir := writeControlPlaneMTLSConfig(t)
	server := &capturingAuditServer{
		entries: []*controlpb.AuditEntry{
			{
				TsUnix:    100,
				EventType: "register",
				NodeName:  "master-01",
				Detail:    "roles=[master]",
				Actor:     "operator",
			},
		},
	}
	addr, _, teardown := startMTLSControlPlaneTestServer(t, configDir, server)
	defer teardown()

	var out bytes.Buffer
	err := runAuditLogQueryCommand(auditLogQueryOptions{
		controlPlane: addr,
		configDir:    configDir,
		sinceUnix:    10,
		untilUnix:    200,
		eventType:    "register",
		node:         "master-01",
		limit:        5,
		output:       "json",
		timeout:      2 * time.Second,
		stdout:       &out,
	})
	if err != nil {
		t.Fatalf("runAuditLogQueryCommand: %v", err)
	}

	req := server.capturedRequest(t)
	if req.GetSinceUnix() != 10 || req.GetUntilUnix() != 200 || req.GetEventTypeFilter() != "register" || req.GetNodeFilter() != "master-01" || req.GetLimit() != 5 {
		t.Fatalf("unexpected QueryAudit request: %+v", req)
	}

	var got auditLogJSONOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode JSON output: %v\n%s", err, out.String())
	}
	if got.Count != 1 || len(got.Entries) != 1 {
		t.Fatalf("unexpected JSON output: %+v", got)
	}
	if got.Entries[0].EventType != "register" || got.Entries[0].NodeName != "master-01" {
		t.Fatalf("unexpected audit entry: %+v", got.Entries[0])
	}
}

func TestRunAuditLogQueryCommandOutputsHuman(t *testing.T) {
	configDir := writeControlPlaneMTLSConfig(t)
	server := &capturingAuditServer{
		entries: []*controlpb.AuditEntry{{
			TsUnix:    100,
			EventType: "decommission",
			NodeName:  "egress-01",
			Detail:    "reassigned=2",
			Actor:     "operator",
		}},
	}
	addr, _, teardown := startMTLSControlPlaneTestServer(t, configDir, server)
	defer teardown()

	var out bytes.Buffer
	err := runAuditLogQueryCommand(auditLogQueryOptions{
		controlPlane: addr,
		configDir:    configDir,
		output:       "human",
		timeout:      2 * time.Second,
		stdout:       &out,
	})
	if err != nil {
		t.Fatalf("runAuditLogQueryCommand: %v", err)
	}
	text := out.String()
	if !strings.Contains(text, "EVENT") || !strings.Contains(text, "decommission") || !strings.Contains(text, "egress-01") {
		t.Fatalf("unexpected human output: %q", text)
	}
}

func TestRunAuditLogQueryCommandOutputsPromTextfile(t *testing.T) {
	configDir := writeControlPlaneMTLSConfig(t)
	server := &capturingAuditServer{
		entries: []*controlpb.AuditEntry{{
			TsUnix:    100,
			EventType: "register",
			NodeName:  "master-01",
			Detail:    "ignored by metric labels",
			Actor:     "operator",
		}},
	}
	addr, _, teardown := startMTLSControlPlaneTestServer(t, configDir, server)
	defer teardown()

	var out bytes.Buffer
	err := runAuditLogQueryCommand(auditLogQueryOptions{
		controlPlane: addr,
		configDir:    configDir,
		output:       "prom-textfile",
		timeout:      2 * time.Second,
		stdout:       &out,
	})
	if err != nil {
		t.Fatalf("runAuditLogQueryCommand: %v", err)
	}
	text := out.String()
	for _, want := range []string{
		"# TYPE awg_mesh_audit_event_timestamp_seconds gauge",
		`event_type="register"`,
		`node_name="master-01"`,
		`actor="operator"`,
		"awg_mesh_audit_events_total 1",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("prom textfile output missing %q:\n%s", want, text)
		}
	}
}

type capturingAuditServer struct {
	controlpb.UnimplementedControlPlaneServer
	mu      sync.Mutex
	request *controlpb.QueryAuditRequest
	entries []*controlpb.AuditEntry
}

func (s *capturingAuditServer) QueryAudit(req *controlpb.QueryAuditRequest, stream controlpb.ControlPlane_QueryAuditServer) error {
	cp := proto.Clone(req).(*controlpb.QueryAuditRequest)
	s.mu.Lock()
	s.request = cp
	s.mu.Unlock()
	for _, entry := range s.entries {
		if err := stream.Send(entry); err != nil {
			return err
		}
	}
	return nil
}

func (s *capturingAuditServer) capturedRequest(t *testing.T) *controlpb.QueryAuditRequest {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.request == nil {
		t.Fatal("QueryAudit was not called")
	}
	return s.request
}
