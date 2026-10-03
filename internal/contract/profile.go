package contract

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

func httpURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("expected absolute HTTP(S) URL without userinfo/fragment")
	}
	return nil
}

func validateProfile(p Profile) []Diagnostic {
	out := []Diagnostic{}
	add := func(where string, err error) {
		if err != nil {
			out = append(out, diagnostic("PROFILE_INVALID", "semantic", "", where, err))
		}
	}
	_, err := Duration(p.Spec.Limits.Timeout)
	add("/spec/limits/timeout", err)
	secret := func(ref, where string) {
		if _, ok := p.Spec.Secrets[ref]; !ok {
			add(where, fmt.Errorf("unknown profile secret %q", ref))
		}
	}
	credential := func(v Credential, where string) {
		if v.SecretRef != "" {
			secret(v.SecretRef, where)
		}
	}
	for _, name := range sortedKeys(p.Spec.Models) {
		m := p.Spec.Models[name]
		where := "/spec/models/" + name
		if m.BaseURL != "" {
			add(where+"/baseUrl", httpURL(m.BaseURL))
		}
		for key, auth := range m.Auth {
			credential(auth, where+"/auth/"+key)
		}
	}
	headerName := regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
	for _, name := range sortedKeys(p.Spec.MCP) {
		m := p.Spec.MCP[name]
		where := "/spec/mcp/" + name
		if m.Transport == "streamable_http" {
			add(where+"/url", httpURL(m.URL))
			seen := map[string]bool{}
			for _, key := range sortedKeys(m.Headers) {
				lower := strings.ToLower(key)
				if !headerName.MatchString(key) || seen[lower] || contains([]string{"host", "content-length", "connection", "transfer-encoding", "accept", "content-type", "mcp-session-id", "mcp-protocol-version", "last-event-id"}, lower) {
					add(where+"/headers", fmt.Errorf("invalid, duplicate or reserved HTTP header %q", key))
				}
				seen[lower] = true
				v := m.Headers[key]
				credential(v, where+"/headers/"+key)
				if v.Value != nil && strings.ContainsAny(*v.Value, "\r\n\x00") {
					add(where+"/headers/"+key, fmt.Errorf("invalid HTTP header value"))
				}
			}
		}
		if m.Transport == "stdio" {
			sandbox, ok := p.Spec.Sandboxes[m.Sandbox]
			if !ok {
				add(where+"/sandbox", fmt.Errorf("unknown sandbox %q", m.Sandbox))
			}
			for key, v := range m.Env {
				credential(v, where+"/env/"+key)
				if strings.HasPrefix(key, "KNOTRA_") {
					add(where, fmt.Errorf("reserved environment key %s", key))
				}
				if v.SecretRef != "" && !contains(sandbox.AllowedSecrets, v.SecretRef) {
					add(where, fmt.Errorf("MCP env secret %q not allowed by sandbox", v.SecretRef))
				}
			}
		}
		for _, tool := range sortedKeys(m.ToolPolicies) {
			policy := m.ToolPolicies[tool]
			if !contains(m.AllowedTools, tool) {
				add(where+"/toolPolicies/"+tool, fmt.Errorf("policy tool not in allowedTools"))
			}
			if policy.IdempotencyArgument != "" {
				_, _, err := pointer(map[string]any{}, policy.IdempotencyArgument)
				add(where, err)
			}
		}
	}
	hostname := regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
	for _, name := range sortedKeys(p.Spec.Sandboxes) {
		s := p.Spec.Sandboxes[name]
		where := "/spec/sandboxes/" + name
		for _, ref := range s.AllowedSecrets {
			secret(ref, where+"/allowedSecrets")
		}
		seen := map[string]bool{}
		for _, host := range s.Network.Hosts {
			normalized := strings.ToLower(strings.TrimSuffix(host, "."))
			if ip := net.ParseIP(host); ip != nil {
				normalized = ip.String()
			} else {
				if len(normalized) > 253 {
					add(where, fmt.Errorf("DNS name too long"))
				}
				for _, label := range strings.Split(normalized, ".") {
					if !hostname.MatchString(label) {
						add(where, fmt.Errorf("invalid exact DNS host %q", host))
						break
					}
				}
			}
			if seen[normalized] {
				add(where, fmt.Errorf("duplicate normalized host %q", host))
			}
			seen[normalized] = true
		}
	}
	return out
}

func (c *compiler) admit(file string, ceiling *Permissions, parent Limits, depth int) {
	if depth > 32 {
		return
	}
	p := c.plan.Pipelines[file]
	if p == nil {
		return
	}
	spec := &p.Spec
	profile := c.plan.Profile.Spec
	limits, err := narrowLimits(parent, spec.Limits)
	c.add("LIMIT_EXCEEDED", "admission", file, "/spec/limits", err)
	for _, name := range sortedKeys(spec.Models) {
		m := spec.Models[name]
		connection, ok := profile.Models[m.Connection]
		if !ok {
			c.add("RESOURCE_UNKNOWN", "admission", file, "/spec/models/"+name, fmt.Errorf("unknown profile model %q", m.Connection))
		}
		if ceiling != nil && !contains(ceiling.Models, m.Connection) {
			c.add("PERMISSION_DENIED", "admission", file, "/spec/models/"+name, fmt.Errorf("model %q outside delegated permissions", m.Connection))
		}
		if m.Model == "" {
			m.Model = connection.Model
		}
		params := map[string]any{}
		for k, v := range connection.Parameters {
			params[k] = v
		}
		for k, v := range m.Parameters {
			params[k] = v
		}
		m.Parameters = params
		spec.Models[name] = m
	}
	for _, name := range sortedKeys(spec.MCP) {
		m := spec.MCP[name]
		connection, ok := profile.MCP[m.Connection]
		if !ok {
			c.add("RESOURCE_UNKNOWN", "admission", file, "/spec/mcp/"+name, fmt.Errorf("unknown profile MCP %q", m.Connection))
		}
		if ceiling != nil {
			if _, ok := ceiling.MCP[m.Connection]; !ok {
				c.add("PERMISSION_DENIED", "admission", file, "/spec/mcp/"+name, fmt.Errorf("MCP %q outside delegated permissions", m.Connection))
			}
		}
		if m.Session == "run" && !connection.AllowRunSession {
			c.add("PERMISSION_DENIED", "admission", file, "/spec/mcp/"+name, fmt.Errorf("run session not permitted"))
		}
	}
	for _, name := range sortedKeys(spec.Sandboxes) {
		s := spec.Sandboxes[name]
		if _, ok := profile.Sandboxes[s.Profile]; !ok {
			c.add("RESOURCE_UNKNOWN", "admission", file, "/spec/sandboxes/"+name, fmt.Errorf("unknown profile sandbox %q", s.Profile))
		}
		if ceiling != nil && !contains(ceiling.Sandboxes, s.Profile) {
			c.add("PERMISSION_DENIED", "admission", file, "/spec/sandboxes/"+name, fmt.Errorf("sandbox %q outside delegated permissions", s.Profile))
		}
	}
	for _, name := range sortedKeys(spec.Secrets) {
		s := spec.Secrets[name]
		if _, ok := profile.Secrets[s.Ref]; !ok {
			c.add("RESOURCE_UNKNOWN", "admission", file, "/spec/secrets/"+name, fmt.Errorf("unknown profile secret %q", s.Ref))
		}
		if ceiling != nil && !contains(ceiling.Secrets, s.Ref) {
			c.add("PERMISSION_DENIED", "admission", file, "/spec/secrets/"+name, fmt.Errorf("secret %q outside delegated permissions", s.Ref))
		}
	}
	grant := func(alias, tool, where string) {
		resource, ok := spec.MCP[alias]
		if !ok {
			c.add("RESOURCE_UNKNOWN", "admission", file, where, fmt.Errorf("unknown MCP alias %q", alias))
			return
		}
		connection := profile.MCP[resource.Connection]
		if !contains(connection.AllowedTools, tool) {
			c.add("PERMISSION_DENIED", "admission", file, where, fmt.Errorf("tool %s/%s not allowed by profile", resource.Connection, tool))
		}
		if ceiling != nil && !contains(ceiling.MCP[resource.Connection], tool) {
			c.add("PERMISSION_DENIED", "admission", file, where, fmt.Errorf("tool %s/%s outside delegated permissions", resource.Connection, tool))
		}
	}
	// Unused default grants must still be valid declarations.
	for _, alias := range sortedKeys(spec.Defaults.Tools.MCP) {
		for _, tool := range spec.Defaults.Tools.MCP[alias] {
			grant(alias, tool, "/spec/defaults/tools")
		}
	}
	var graph func(Graph, string)
	graph = func(g Graph, prefix string) {
		for _, name := range sortedKeys(g.Nodes) {
			n := g.Nodes[name]
			where := prefix + "/nodes/" + name
			if n.Tools != nil {
				for _, alias := range sortedKeys(n.Tools.MCP) {
					for _, tool := range n.Tools.MCP[alias] {
						grant(alias, tool, where+"/tools")
					}
				}
			}
			if n.Tool != nil {
				grant(n.Tool.Server, n.Tool.Name, where+"/tool")
			}
			if n.Type == "agent" || n.Type == "code" {
				sandbox := profile.Sandboxes[spec.Sandboxes[n.Sandbox].Profile]
				if n.Type == "code" && !contains(sandbox.AllowedTools, "process.exec") {
					c.add("PERMISSION_DENIED", "admission", file, where, fmt.Errorf("code requires process.exec"))
				}
				if n.Tools != nil {
					for _, tool := range n.Tools.Sandbox {
						if !contains(sandbox.AllowedTools, tool) {
							c.add("PERMISSION_DENIED", "admission", file, where, fmt.Errorf("sandbox tool %s not permitted", tool))
						}
					}
				}
				for _, env := range sortedKeys(n.Env) {
					v := n.Env[env]
					if v.Secret != "" {
						canonical := spec.Secrets[v.Secret].Ref
						if !contains(sandbox.AllowedSecrets, canonical) {
							c.add("PERMISSION_DENIED", "admission", file, where+"/env/"+env, fmt.Errorf("secret %s not allowed in sandbox", canonical))
						}
					}
				}
			}
			if n.Foreach != nil {
				graph(n.Foreach.Body, where+"/foreach/body")
			}
			if n.Loop != nil {
				graph(n.Loop.Body, where+"/loop/body")
			}
			if n.Pipeline != nil {
				permissions := n.Pipeline.Permissions
				c.checkPermissions(file, where, permissions, ceiling)
				c.admit(n.Pipeline.File, &permissions, limits, depth+1)
			}
		}
	}
	graph(spec.Graph, "/spec")
}

func (c *compiler) checkPermissions(file, where string, p Permissions, parent *Permissions) {
	profile := c.plan.Profile.Spec
	check := func(category string, values []string, exists func(string) bool, upper []string) {
		for _, id := range values {
			if !exists(id) {
				c.add("RESOURCE_UNKNOWN", "admission", file, where+"/permissions/"+category, fmt.Errorf("unknown canonical profile ID %q", id))
			}
			if parent != nil && !contains(upper, id) {
				c.add("PERMISSION_DENIED", "admission", file, where+"/permissions/"+category, fmt.Errorf("delegation expands parent permissions for %q", id))
			}
		}
	}
	ceiling := Permissions{}
	if parent != nil {
		ceiling = *parent
	}
	check("models", p.Models, func(s string) bool { _, ok := profile.Models[s]; return ok }, ceiling.Models)
	check("sandboxes", p.Sandboxes, func(s string) bool { _, ok := profile.Sandboxes[s]; return ok }, ceiling.Sandboxes)
	check("secrets", p.Secrets, func(s string) bool { _, ok := profile.Secrets[s]; return ok }, ceiling.Secrets)
	for _, id := range sortedKeys(p.MCP) {
		m, ok := profile.MCP[id]
		if !ok {
			c.add("RESOURCE_UNKNOWN", "admission", file, where, fmt.Errorf("unknown canonical MCP %q", id))
		}
		for _, tool := range p.MCP[id] {
			if !contains(m.AllowedTools, tool) || parent != nil && !contains(parent.MCP[id], tool) {
				c.add("PERMISSION_DENIED", "admission", file, where, fmt.Errorf("delegation expands permissions for %s/%s", id, tool))
			}
		}
	}
}

func narrowLimits(parent, requested Limits) (Limits, error) {
	out := parent
	check := func(name string, requested int, target *int) error {
		if requested == 0 {
			return nil
		}
		if *target > 0 && requested > *target {
			return fmt.Errorf("%s exceeds parent limit", name)
		}
		*target = requested
		return nil
	}
	for _, pair := range []struct {
		name      string
		requested int
		target    *int
	}{{"maxConcurrentNodes", requested.MaxConcurrentNodes, &out.MaxConcurrentNodes}, {"maxNodeInstances", requested.MaxNodeInstances, &out.MaxNodeInstances}, {"maxModelCalls", requested.MaxModelCalls, &out.MaxModelCalls}, {"maxToolCalls", requested.MaxToolCalls, &out.MaxToolCalls}} {
		if err := check(pair.name, pair.requested, pair.target); err != nil {
			return out, err
		}
	}
	if requested.Timeout != "" {
		a, err := Duration(requested.Timeout)
		if err != nil {
			return out, err
		}
		if parent.Timeout != "" {
			b, err := Duration(parent.Timeout)
			if err != nil {
				return out, err
			}
			if a > b {
				return out, fmt.Errorf("timeout exceeds parent limit")
			}
		}
		out.Timeout = requested.Timeout
	}
	return out, nil
}

// offlineDelegation checks constraints intrinsic to the package even when no
// EngineProfile is available. Only membership in the external catalog is deferred.
func (c *compiler) offlineDelegation(file string, parent *Permissions, upper Limits, depth int) {
	if depth > 32 {
		return
	}
	pipeline := c.plan.Pipelines[file]
	if pipeline == nil {
		return
	}
	spec := pipeline.Spec
	limits, err := narrowLimits(upper, spec.Limits)
	c.semantic(file, "/spec/limits", err)
	if parent != nil {
		for name, m := range spec.Models {
			if !contains(parent.Models, m.Connection) {
				c.semantic(file, "/spec/models/"+name, fmt.Errorf("model %q outside delegated permissions", m.Connection))
			}
		}
		for name, m := range spec.Sandboxes {
			if !contains(parent.Sandboxes, m.Profile) {
				c.semantic(file, "/spec/sandboxes/"+name, fmt.Errorf("sandbox %q outside delegated permissions", m.Profile))
			}
		}
		for name, m := range spec.Secrets {
			if !contains(parent.Secrets, m.Ref) {
				c.semantic(file, "/spec/secrets/"+name, fmt.Errorf("secret %q outside delegated permissions", m.Ref))
			}
		}
		for name, m := range spec.MCP {
			if _, ok := parent.MCP[m.Connection]; !ok {
				c.semantic(file, "/spec/mcp/"+name, fmt.Errorf("MCP %q outside delegated permissions", m.Connection))
			}
		}
	}
	grant := func(alias, tool, where string) {
		m, ok := spec.MCP[alias]
		if !ok {
			c.semantic(file, where, fmt.Errorf("unknown MCP alias %q", alias))
			return
		}
		if parent != nil && !contains(parent.MCP[m.Connection], tool) {
			c.semantic(file, where, fmt.Errorf("tool %s/%s outside delegated permissions", m.Connection, tool))
		}
	}
	for alias, list := range spec.Defaults.Tools.MCP {
		for _, tool := range list {
			grant(alias, tool, "/spec/defaults/tools")
		}
	}
	var visit func(Graph, string)
	visit = func(g Graph, where string) {
		for _, name := range sortedKeys(g.Nodes) {
			n := g.Nodes[name]
			nodePath := where + "/nodes/" + name
			if n.Tools != nil {
				for alias, list := range n.Tools.MCP {
					for _, tool := range list {
						grant(alias, tool, nodePath+"/tools")
					}
				}
			}
			if n.Tool != nil {
				grant(n.Tool.Server, n.Tool.Name, nodePath+"/tool")
			}
			if n.Foreach != nil {
				visit(n.Foreach.Body, nodePath+"/foreach/body")
			}
			if n.Loop != nil {
				visit(n.Loop.Body, nodePath+"/loop/body")
			}
			if n.Pipeline != nil {
				p := n.Pipeline.Permissions
				if parent != nil {
					for _, entry := range []struct{ requested, allowed []string }{{p.Models, parent.Models}, {p.Sandboxes, parent.Sandboxes}, {p.Secrets, parent.Secrets}} {
						for _, id := range entry.requested {
							if !contains(entry.allowed, id) {
								c.semantic(file, nodePath, fmt.Errorf("delegation expands ancestor permissions for %q", id))
							}
						}
					}
					for id, tools := range p.MCP {
						for _, tool := range tools {
							if !contains(parent.MCP[id], tool) {
								c.semantic(file, nodePath, fmt.Errorf("delegation expands ancestor MCP permissions for %s/%s", id, tool))
							}
						}
					}
				}
				c.offlineDelegation(n.Pipeline.File, &p, limits, depth+1)
			}
		}
	}
	visit(spec.Graph, "/spec")
}
