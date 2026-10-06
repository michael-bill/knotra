package cli

import (
	"fmt"
	"net/url"
	"time"

	"github.com/spf13/cobra"

	"github.com/michael-bill/knotra/internal/protocol"
)

func (s *commandState) definitionsCommand() *cobra.Command {
	root := &cobra.Command{Use: "definitions", Short: "Publish and inspect immutable pipeline packages"}
	var cursor string
	var all bool
	list := &cobra.Command{Use: "list", Short: "List published definitions", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		page, err := fetchPages[protocol.Definition](cmd.Context(), s, "/definitions", cursor, all)
		if err != nil {
			return err
		}
		if s.json {
			return s.printJSON(page)
		}
		rows := [][]string{}

		for _, definition := range page.Items {
			rows = append(rows, []string{definition.ID, definition.Name, definition.Title, definition.PackageDigest})
		}

		if err = s.table([]string{"ID", "NAME", "TITLE", "DIGEST"}, rows); err != nil {
			return err
		}
		return s.nextCursor(page.NextCursor)
	}}
	pageFlags(list, &cursor, &all)
	get := &cobra.Command{Use: "get DEFINITION_ID", Short: "Get a definition with its exact package bytes", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		var result struct {
			Definition protocol.Definition `json:"definition"`
		}
		if err := s.client().Get(cmd.Context(), "/definitions/"+url.PathEscape(args[0]), &result); err != nil {
			return err
		}
		return s.printJSON(result)
	}}
	var packageRoot, key string
	create := &cobra.Command{Use: "create PIPELINE.yaml", Short: "Publish an immutable package version", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		pkg, err := loadPackage(args[0], packageRoot)
		if err != nil {
			return err
		}
		return s.mutation(cmd.Context(), "/definitions", map[string]any{"package": pkg}, key)
	}}
	create.Flags().StringVar(&packageRoot, "package-root", "", "Root of package-relative paths")
	create.Flags().StringVar(&key, "idempotency-key", "", "Stable operation ID")
	root.AddCommand(list, get, create)
	return root
}

func (s *commandState) requestsCommand() *cobra.Command {
	root := &cobra.Command{Use: "requests", Short: "Inspect and answer requests to a human"}
	var cursor, runFilter, statusFilter string
	var all bool
	list := &cobra.Command{Use: "list", Short: "List human requests and response schemas", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		page, err := fetchPages[protocol.HumanRequest](cmd.Context(), s, "/requests", cursor, all)
		if err != nil {
			return err
		}
		filtered := []protocol.HumanRequest{}

		for _, request := range page.Items {
			if (runFilter == "" || request.RunID == runFilter) && (statusFilter == "" || request.Status == statusFilter) {
				filtered = append(filtered, request)
			}
		}

		page.Items = filtered
		if s.json {
			return s.printJSON(page)
		}
		rows := [][]string{}

		for _, request := range page.Items {
			rows = append(
				rows,
				[]string{
					request.ID,
					request.RunID,
					request.InstanceID,
					request.Status,
					request.Deadline.Format(time.RFC3339),
					request.Prompt,
				},
			)
		}

		if err = s.table([]string{"ID", "RUN", "INSTANCE", "STATUS", "DEADLINE", "PROMPT"}, rows); err != nil {
			return err
		}
		return s.nextCursor(page.NextCursor)
	}}
	pageFlags(list, &cursor, &all)
	list.Flags().StringVar(&runFilter, "run", "", "Filter the fetched requests by run ID")
	list.Flags().StringVar(&statusFilter, "status", "", "Filter the fetched requests by status")
	var file, key string
	var outputs []string
	respond := &cobra.Command{Use: "respond REQUEST_ID", Short: "Submit a schema-checked response to one exact request", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if file == "" && len(outputs) == 0 {
			return fmt.Errorf("provide --outputs FILE or --output NAME=JSON; use a file containing {} for an empty response")
		}
		values, err := s.object(file, outputs)
		if err != nil {
			return err
		}
		return s.mutation(cmd.Context(), "/requests/"+url.PathEscape(args[0])+"/response", responseObject(values), key)
	}}
	respond.Flags().StringVar(&file, "outputs", "", "JSON output object file; - reads stdin")
	respond.Flags().StringArrayVar(&outputs, "output", nil, "Output NAME=JSON (repeatable)")
	respond.Flags().StringVar(&key, "idempotency-key", "", "Stable operation ID")
	root.AddCommand(list, respond)
	return root
}

func (s *commandState) operationsCommand() *cobra.Command {
	root := &cobra.Command{Use: "operations", Short: "Inspect and reconcile locally journaled mutation commands"}
	list := &cobra.Command{Use: "list", Short: "List durable command receipts for this endpoint", Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error {
		items, err := s.client().Commands()
		if err != nil {
			return err
		}
		if s.json {
			return s.printJSON(map[string]any{"items": items})
		}
		rows := [][]string{}

		for _, item := range items {
			rows = append(rows, []string{item.ID, item.Status, item.Route, item.EngineID})
		}

		return s.table([]string{"ID", "STATUS", "ROUTE", "ENGINE"}, rows)
	}}
	retry := &cobra.Command{Use: "retry OPERATION_ID", Short: "Reconcile the original payload with the original idempotency key", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		result := map[string]any{}
		if err := s.client().Retry(cmd.Context(), args[0], &result); err != nil {
			return err
		}
		result["operationId"] = args[0]
		return s.printJSON(result)
	}}
	root.AddCommand(list, retry)
	return root
}
