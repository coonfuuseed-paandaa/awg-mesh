package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	controlpb "github.com/coonfuuseed-paandaa/awg-mesh/proto/control_plane"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	auditOutputHuman         = "human"
	auditOutputJSON          = "json"
	auditOutputPromTextfile  = "prom-textfile"
	defaultAuditQueryTimeout = 10 * time.Second
	maxAuditQueryLimit       = 1<<31 - 1
)

type auditLogQueryOptions struct {
	controlPlane string
	sinceUnix    int64
	untilUnix    int64
	eventType    string
	node         string
	limit        int
	output       string
	timeout      time.Duration
	stdout       io.Writer
}

type auditLogJSONOutput struct {
	Count   int             `json:"count"`
	Entries []auditLogEntry `json:"entries"`
}

type auditLogEntry struct {
	TsUnix    int64  `json:"ts_unix"`
	Timestamp string `json:"timestamp"`
	EventType string `json:"event_type"`
	NodeName  string `json:"node_name"`
	Detail    string `json:"detail"`
	Actor     string `json:"actor"`
}

func newAuditLogCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit-log",
		Short: "Query control-plane audit logs",
	}

	cmd.AddCommand(newAuditLogQueryCommand())
	return cmd
}

func newAuditLogQueryCommand() *cobra.Command {
	options := auditLogQueryOptions{
		output:  auditOutputHuman,
		timeout: defaultAuditQueryTimeout,
	}

	cmd := &cobra.Command{
		Use:   "query",
		Short: "Query audit events from the control plane",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			options.stdout = cmd.OutOrStdout()
			return runAuditLogQueryCommand(options)
		},
	}

	cmd.Flags().StringVar(&options.controlPlane, "control-plane", "", "Control-plane gRPC address")
	cmd.Flags().Int64Var(&options.sinceUnix, "since-unix", 0, "Inclusive lower timestamp bound as Unix seconds")
	cmd.Flags().Int64Var(&options.untilUnix, "until-unix", 0, "Inclusive upper timestamp bound as Unix seconds")
	cmd.Flags().StringVar(&options.eventType, "event-type", "", "Audit event type filter")
	cmd.Flags().StringVar(&options.node, "node", "", "Node name filter")
	cmd.Flags().IntVar(&options.limit, "limit", 0, "Maximum events to return (0 means all)")
	cmd.Flags().StringVar(&options.output, "output", auditOutputHuman, "Output format (human, json, prom-textfile)")
	cmd.Flags().DurationVar(&options.timeout, "timeout", defaultAuditQueryTimeout, "Query timeout")
	return cmd
}

func runAuditLogQueryCommand(options auditLogQueryOptions) error {
	validated, err := validateAuditLogQueryOptions(options)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), validated.timeout)
	defer cancel()

	conn, err := grpc.NewClient(validated.controlPlane, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("connect control-plane %q: %w", validated.controlPlane, err)
	}
	defer func() { _ = conn.Close() }()

	stream, err := controlpb.NewControlPlaneClient(conn).QueryAudit(ctx, &controlpb.QueryAuditRequest{
		SinceUnix:       validated.sinceUnix,
		UntilUnix:       validated.untilUnix,
		EventTypeFilter: validated.eventType,
		NodeFilter:      validated.node,
		Limit:           int32(validated.limit),
	})
	if err != nil {
		return fmt.Errorf("query audit log: %w", err)
	}

	entries, err := collectAuditEntries(stream)
	if err != nil {
		return err
	}
	return writeAuditEntries(commandOutput(validated.stdout), validated.output, entries)
}

func validateAuditLogQueryOptions(options auditLogQueryOptions) (auditLogQueryOptions, error) {
	controlPlane := strings.TrimSpace(options.controlPlane)
	if controlPlane == "" {
		return auditLogQueryOptions{}, fmt.Errorf("--control-plane is required")
	}
	if options.limit < 0 {
		return auditLogQueryOptions{}, fmt.Errorf("--limit must be >= 0")
	}
	if options.limit > maxAuditQueryLimit {
		return auditLogQueryOptions{}, fmt.Errorf("--limit must be <= %d", maxAuditQueryLimit)
	}
	if options.sinceUnix != 0 && options.untilUnix != 0 && options.untilUnix < options.sinceUnix {
		return auditLogQueryOptions{}, fmt.Errorf("--until-unix must be greater than or equal to --since-unix")
	}
	output, err := normalizeAuditOutput(options.output)
	if err != nil {
		return auditLogQueryOptions{}, err
	}
	timeout := options.timeout
	if timeout <= 0 {
		timeout = defaultAuditQueryTimeout
	}

	return auditLogQueryOptions{
		controlPlane: controlPlane,
		sinceUnix:    options.sinceUnix,
		untilUnix:    options.untilUnix,
		eventType:    strings.TrimSpace(options.eventType),
		node:         strings.TrimSpace(options.node),
		limit:        options.limit,
		output:       output,
		timeout:      timeout,
		stdout:       options.stdout,
	}, nil
}

func collectAuditEntries(stream controlpb.ControlPlane_QueryAuditClient) ([]auditLogEntry, error) {
	entries := make([]auditLogEntry, 0)
	for {
		entry, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return entries, nil
		}
		if err != nil {
			return entries, fmt.Errorf("receive audit entry: %w", err)
		}
		entries = append(entries, auditEntryFromProto(entry))
	}
}

func auditEntryFromProto(entry *controlpb.AuditEntry) auditLogEntry {
	if entry == nil {
		return auditLogEntry{}
	}
	timestamp := ""
	if entry.GetTsUnix() != 0 {
		timestamp = time.Unix(entry.GetTsUnix(), 0).UTC().Format(time.RFC3339)
	}
	return auditLogEntry{
		TsUnix:    entry.GetTsUnix(),
		Timestamp: timestamp,
		EventType: entry.GetEventType(),
		NodeName:  entry.GetNodeName(),
		Detail:    entry.GetDetail(),
		Actor:     entry.GetActor(),
	}
}

func writeAuditEntries(out io.Writer, output string, entries []auditLogEntry) error {
	switch output {
	case auditOutputHuman:
		return writeAuditEntriesHuman(out, entries)
	case auditOutputJSON:
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(auditLogJSONOutput{Count: len(entries), Entries: entries})
	case auditOutputPromTextfile:
		return writeAuditEntriesPromTextfile(out, entries)
	default:
		return fmt.Errorf("unsupported audit output %q", output)
	}
}

func writeAuditEntriesHuman(out io.Writer, entries []auditLogEntry) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "TS\tEVENT\tNODE\tACTOR\tDETAIL"); err != nil {
		return err
	}
	for _, entry := range entries {
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			orDash(entry.Timestamp), orDash(entry.EventType), orDash(entry.NodeName), orDash(entry.Actor), orDash(entry.Detail)); err != nil {
			return err
		}
	}
	return w.Flush()
}

func writeAuditEntriesPromTextfile(out io.Writer, entries []auditLogEntry) error {
	if _, err := fmt.Fprintln(out, "# HELP awg_mesh_audit_events_total Audit events returned by mesh-ctl audit-log query."); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(out, "# TYPE awg_mesh_audit_events_total gauge"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "awg_mesh_audit_events_total %d\n", len(entries)); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(out, "# HELP awg_mesh_audit_event_timestamp_seconds Audit event timestamps returned by mesh-ctl audit-log query."); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(out, "# TYPE awg_mesh_audit_event_timestamp_seconds gauge"); err != nil {
		return err
	}
	for i, entry := range entries {
		if _, err := fmt.Fprintf(out,
			"awg_mesh_audit_event_timestamp_seconds{sequence=\"%d\",event_type=\"%s\",node_name=\"%s\",actor=\"%s\"} %d\n",
			i,
			promLabelValue(entry.EventType),
			promLabelValue(entry.NodeName),
			promLabelValue(entry.Actor),
			entry.TsUnix,
		); err != nil {
			return err
		}
	}
	return nil
}

func normalizeAuditOutput(output string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(output)) {
	case "", auditOutputHuman:
		return auditOutputHuman, nil
	case auditOutputJSON:
		return auditOutputJSON, nil
	case auditOutputPromTextfile:
		return auditOutputPromTextfile, nil
	default:
		return "", fmt.Errorf("unsupported --output %q (supported: human, json, prom-textfile)", output)
	}
}

func promLabelValue(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\n", "\\n")
	value = strings.ReplaceAll(value, "\"", "\\\"")
	return value
}

func orDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}
