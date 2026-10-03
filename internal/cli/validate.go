package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/michael-bill/knotra/internal/contract"
)

func (s *commandState) validateCommand() *cobra.Command {
	var root, profile, inputsFile, artifactsFile string
	var inputs, artifacts []string
	var remote bool
	cmd := &cobra.Command{Use: "validate PIPELINE.yaml", Short: "Validate a package offline or perform authoritative engine admission", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		pkg, err := loadPackage(args[0], root)
		if err != nil {
			return err
		}
		in, err := s.object(inputsFile, inputs)
		if err != nil {
			return err
		}
		art, err := s.artifactBindings(artifactsFile, artifacts)
		if err != nil {
			return err
		}
		var result struct {
			Valid       bool                  `json:"valid"`
			Diagnostics []contract.Diagnostic `json:"diagnostics"`
		}
		if remote {
			if profile == "" {
				return fmt.Errorf("--profile must name an engine profile for remote validation")
			}
			err = s.client().Validate(
				cmd.Context(),
				map[string]any{"package": pkg, "profile": profile, "inputs": in, "artifacts": art},
				&result,
			)
			if err != nil {
				return err
			}
		} else {
			var trusted *contract.Profile
			if profile != "" {
				data, err := s.readFile(profile, 1<<20)
				if err != nil {
					return err
				}
				p, diags := contract.ParseProfile(data)
				if contract.HasErrors(diags) {
					result.Diagnostics = diags
				} else {
					trusted = &p
				}
			}
			if !contract.HasErrors(result.Diagnostics) {
				plan, diags := contract.Compile(pkg, trusted)
				result.Diagnostics = diags
				if !contract.HasErrors(diags) && (inputsFile != "" || len(inputs) > 0) {
					values := contract.Values{}
					ports := map[string]contract.Port{}

					for name, port := range plan.Pipelines[plan.Root].Spec.Inputs {
						if port.Artifact == nil {
							ports[name] = port
						}
					}

					for name, raw := range in {
						values[name] = contract.Value{JSON: raw}
					}

					if _, err = contract.ValidatePorts(ports, values, true); err != nil {
						result.Diagnostics = append(
							result.Diagnostics,
							contract.Diagnostic{
								Severity: "error",
								Code:     "INPUT_INVALID",
								Phase:    "semantic",
								Path:     "/inputs",
								Message:  err.Error(),
							},
						)
					}
				}
			}
			if len(art) > 0 {
				result.Diagnostics = append(
					result.Diagnostics,
					contract.Diagnostic{
						Severity: "warning",
						Code:     "ADMISSION_PENDING",
						Phase:    "admission",
						Path:     "/artifacts",
						Message:  "Artifact registration and permissions require --remote validation.",
					},
				)
			}
			result.Valid = !contract.HasErrors(result.Diagnostics)
		}
		if result.Diagnostics == nil {
			result.Diagnostics = []contract.Diagnostic{}
		}
		if s.json {
			if err = s.printJSON(result); err != nil {
				return err
			}
		} else {
			if err = s.diagnostics(result.Diagnostics); err != nil {
				return err
			}
			if result.Valid {
				_, err = fmt.Fprintln(s.options.Out, "Package is valid for the checks performed.")
			}
		}
		if err != nil {
			return err
		}
		if !result.Valid {
			return &ExitError{Code: 2, Message: "pipeline validation failed"}
		}
		return nil
	}}
	cmd.Flags().StringVar(&root, "package-root", "", "Root of package-relative paths")
	cmd.Flags().StringVar(&profile, "profile", "", "EngineProfile YAML file locally, or profile ID with --remote")
	cmd.Flags().BoolVar(&remote, "remote", false, "Run authoritative admission against the engine")
	inputFlags(cmd, &inputsFile, &inputs, &artifactsFile, &artifacts)
	return cmd
}

func inputFlags(cmd *cobra.Command, file *string, values *[]string, artifactFile *string, artifacts *[]string) {
	cmd.Flags().StringVar(file, "inputs", "", "JSON object file; - reads stdin")
	cmd.Flags().StringArrayVar(values, "input", nil, "JSON input NAME=JSON (repeatable)")
	cmd.Flags().StringVar(artifactFile, "artifacts", "", "JSON object mapping ports to registered artifact IDs")
	cmd.Flags().StringArrayVar(artifacts, "artifact", nil, "Artifact input NAME=ID or NAME=[IDS] (repeatable)")
}

func responseObject(outputs map[string]json.RawMessage) map[string]any {
	return map[string]any{"outputs": outputs}
}
