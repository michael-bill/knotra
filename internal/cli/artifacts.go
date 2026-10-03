package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/spf13/cobra"
)

func (s *commandState) artifactsCommand() *cobra.Command {
	root := &cobra.Command{Use: "artifacts", Short: "Upload, inspect and verify immutable files"}
	var cursor string
	var all bool
	list := &cobra.Command{Use: "list", Short: "List registered artifacts", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		page, err := fetchPages[contract.Artifact](cmd.Context(), s, "/artifacts", cursor, all)
		if err != nil {
			return err
		}
		if s.json {
			return s.printJSON(page)
		}
		rows := [][]string{}
		for _, artifact := range page.Items {
			rows = append(rows, []string{artifact.ID, artifact.Name, artifact.MediaType, fmt.Sprint(artifact.Size), artifact.SHA256})
		}
		if err = s.table([]string{"ID", "NAME", "MEDIA TYPE", "BYTES", "SHA256"}, rows); err != nil {
			return err
		}
		return s.nextCursor(page.NextCursor)
	}}
	pageFlags(list, &cursor, &all)
	var output string
	var force bool
	get := &cobra.Command{Use: "get ARTIFACT_ID", Short: "Show artifact metadata or download with --output", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if output != "" {
			return s.download(cmd.Context(), args[0], output, force)
		}
		artifact, err := s.getArtifact(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return s.printJSON(map[string]any{"artifact": artifact})
	}}
	get.Flags().StringVarP(&output, "output", "o", "", "Write verified bytes to this path; - writes stdout")
	get.Flags().BoolVar(&force, "force", false, "Replace an existing output file")
	var downloadOutput string
	var downloadForce bool
	download := &cobra.Command{Use: "download ARTIFACT_ID", Short: "Download bytes and verify both size and SHA-256", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if downloadOutput == "" {
			return fmt.Errorf("--output is required")
		}
		return s.download(cmd.Context(), args[0], downloadOutput, downloadForce)
	}}
	download.Flags().StringVarP(&downloadOutput, "output", "o", "", "Output path; - writes exact bytes to stdout")
	download.Flags().BoolVar(&downloadForce, "force", false, "Replace an existing output file")
	var name, media, key string
	upload := &cobra.Command{Use: "upload FILE", Short: "Upload and register immutable input bytes", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if media == "" {
			return fmt.Errorf("--media-type is required")
		}
		filename := name
		if filename == "" {
			if args[0] == "-" {
				return fmt.Errorf("--name is required for stdin")
			}
			filename = filepath.Base(args[0])
		}
		data, err := s.readFile(args[0], 64<<20)
		if err != nil {
			return err
		}
		return s.mutation(cmd.Context(), "/artifacts", map[string]any{"name": filename, "mediaType": media, "content": data}, key)
	}}
	upload.Flags().StringVar(&name, "name", "", "Artifact display name (defaults to input basename)")
	upload.Flags().StringVar(&media, "media-type", "", "Exact MIME type, e.g. text/plain")
	upload.Flags().StringVar(&key, "idempotency-key", "", "Stable operation ID")
	root.AddCommand(list, get, download, upload)
	return root
}
func (s *commandState) getArtifact(ctx context.Context, id string) (contract.Artifact, error) {
	var result struct {
		Artifact contract.Artifact `json:"artifact"`
	}
	err := s.client().Get(ctx, "/artifacts/"+url.PathEscape(id), &result)
	return result.Artifact, err
}
func (s *commandState) download(ctx context.Context, id, output string, force bool) error {
	if output == "-" && s.json {
		return fmt.Errorf("binary stdout and --json are mutually exclusive; choose an output file")
	}
	artifact, err := s.getArtifact(ctx, id)
	if err != nil {
		return err
	}
	data, err := s.client().Bytes(ctx, "/artifacts/"+url.PathEscape(id)+"/content")
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	actual := hex.EncodeToString(sum[:])
	if int64(len(data)) != artifact.Size || len(artifact.SHA256) != 64 || actual != strings.ToLower(artifact.SHA256) {
		return fmt.Errorf("artifact integrity check failed; no bytes were written")
	}
	if output == "-" {
		_, err = s.options.Out.Write(data)
		return err
	}
	if err = atomicFile(output, data, force); err != nil {
		return err
	}
	if s.json {
		return s.printJSON(map[string]any{"artifact": artifact, "path": output})
	}
	_, err = fmt.Fprintf(s.options.Out, "Saved %s (%d bytes, SHA-256 %s).\n", output, len(data), actual)
	return err
}
