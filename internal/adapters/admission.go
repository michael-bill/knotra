package adapters

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/michael-bill/knotra/internal/contract"
)

// modelInfo performs bounded discovery only; these requests never generate tokens.
func (r *Runner) modelInfo(ctx context.Context, profile contract.Profile, c contract.ModelConnection, path string, input any, output any) error {
	method := http.MethodGet
	var reader io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if key, ok := c.Auth["key"]; ok {
		secret, err := r.credential(profile, key)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	client := r.httpClient()
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("model discovery request failed")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("model discovery returned HTTP %d", res.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(output)
}

func (r *Runner) modelDigest(ctx context.Context, profile contract.Profile, c contract.ModelConnection) (string, error) {
	var tags struct {
		Models []struct {
			Name   string `json:"name"`
			Model  string `json:"model"`
			Digest string `json:"digest"`
		} `json:"models"`
	}
	if err := r.modelInfo(ctx, profile, c, "/api/tags", nil, &tags); err != nil {
		return "", err
	}

	for _, model := range tags.Models {
		if model.Name == c.Model || model.Model == c.Model || model.Name == c.Model+":latest" {
			if model.Digest == "" {
				return "", fmt.Errorf("model digest is absent")
			}
			return model.Digest, nil
		}
	}

	return "", fmt.Errorf("Ollama model %q is not installed", c.Model)
}

func (r *Runner) prepareModels(ctx context.Context, req Request) error {
	if req.Plan.ModelDigests == nil {
		req.Plan.ModelDigests = map[string]string{}
	}

	for alias, resource := range pipeline(req).Spec.Models {
		c, err := modelConfig(req, alias)
		if err != nil {
			return err
		}
		key := resource.Connection + "/" + c.Model
		digest, err := r.modelDigest(ctx, req.Plan.Profile, c)
		if err != nil {
			return err
		}
		var details struct {
			Capabilities []string `json:"capabilities"`
		}
		if err = r.modelInfo(ctx, req.Plan.Profile, c, "/api/show", map[string]any{"model": c.Model}, &details); err != nil {
			return err
		}
		requirements := append([]string(nil), resource.Requires...)
		requirements = append(requirements, "structuredOutput")
		if modelNeedsTools(pipeline(req).Spec.Graph, alias) {
			requirements = append(requirements, "toolCalling")
		}

		for _, capability := range requirements {
			native := map[string]string{"toolCalling": "tools", "structuredOutput": "completion", "imageInput": "vision"}[capability]
			if !slices.Contains(details.Capabilities, native) {
				return fmt.Errorf("model %q lacks required capability %s", c.Model, capability)
			}
		}

		req.Plan.ModelDigests[key] = digest
	}

	return nil
}

func (r *Runner) prepareSandboxes(ctx context.Context, plan *contract.Plan) error {
	used := map[string]bool{}
	usedSecrets := map[string]bool{}
	usedModels := map[string]bool{}
	usedMCP := map[string]bool{}

	for _, p := range plan.Pipelines {
		for _, s := range p.Spec.Sandboxes {
			used[s.Profile] = true
		}

		for _, s := range p.Spec.Secrets {
			usedSecrets[s.Ref] = true
		}

		for _, m := range p.Spec.Models {
			usedModels[m.Connection] = true
		}

		for _, m := range p.Spec.MCP {
			usedMCP[m.Connection] = true
		}
	}

	for name := range usedModels {
		for _, c := range plan.Profile.Spec.Models[name].Auth {
			if c.SecretRef != "" {
				usedSecrets[c.SecretRef] = true
			}
		}
	}

	for name := range usedMCP {
		m := plan.Profile.Spec.MCP[name]
		if m.Transport == "stdio" {
			used[m.Sandbox] = true
		}

		for _, values := range []map[string]contract.Credential{m.Headers, m.Env} {
			for _, c := range values {
				if c.SecretRef != "" {
					usedSecrets[c.SecretRef] = true
				}
			}
		}
	}

	for name := range usedSecrets {
		if _, err := r.secret(plan.Profile, name); err != nil {
			return err
		}
	}

	plan.Runtime.AdapterVersion = Version
	if len(used) == 0 {
		return nil
	}
	helper, err := os.ReadFile(r.HelperPath)
	if err != nil {
		return fmt.Errorf("sandbox helper is unavailable")
	}
	if _, err = elf.NewFile(bytes.NewReader(helper)); err != nil {
		return fmt.Errorf("sandbox helper must be a Linux ELF executable")
	}
	sum := sha256.Sum256(helper)
	plan.Runtime.HelperSHA256 = hex.EncodeToString(sum[:])
	d, err := r.docker()
	if err != nil {
		return err
	}
	defer d.client.CloseIdleConnections()

	for name := range used {
		p, ok := plan.Profile.Spec.Sandboxes[name]
		if !ok {
			return fmt.Errorf("sandbox profile %q missing", name)
		}
		var image struct {
			ID string `json:"Id"`
		}
		if err = d.json(ctx, "GET", "/images/"+urlPath(p.Image)+"/json", nil, &image); err != nil {
			return fmt.Errorf("sandbox %s: image must be pulled before admission: %w", name, err)
		}
		p.Image = image.ID
		if p.Network.Mode == "allowlist" {
			if r.FirewallImage == "" {
				return fmt.Errorf("sandbox %s requires firewall image", name)
			}
			if plan.Runtime.FirewallImage == "" {
				var image struct {
					ID string `json:"Id"`
				}
				if err = d.json(ctx, "GET", "/images/"+urlPath(r.FirewallImage)+"/json", nil, &image); err != nil {
					return err
				}
				plan.Runtime.FirewallImage = image.ID
			}
			p.ResolvedHosts = map[string][]string{}

			for _, host := range p.Network.Hosts {
				addresses := []string{host}
				if net.ParseIP(host) == nil {
					addresses, err = net.DefaultResolver.LookupHost(ctx, host)
					if err != nil {
						return fmt.Errorf("cannot resolve allowlisted host %q", host)
					}
				}

				for _, address := range addresses {
					ip := net.ParseIP(address)
					if ip == nil {
						return fmt.Errorf("invalid allowlisted IP")
					}
					p.ResolvedHosts[host] = append(p.ResolvedHosts[host], ip.String())
				}
			}
		}
		plan.Profile.Spec.Sandboxes[name] = p
	}

	return nil
}

func modelNeedsTools(g contract.Graph, alias string) bool {
	for _, n := range g.Nodes {
		if n.Agent != nil && n.Agent.Model == alias {
			return true
		}
		if n.Foreach != nil && modelNeedsTools(n.Foreach.Body, alias) {
			return true
		}
		if n.Loop != nil && modelNeedsTools(n.Loop.Body, alias) {
			return true
		}
	}

	return false
}
