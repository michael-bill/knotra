package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/michael-bill/knotra/internal/contract"
)

func TestDockerIsolationAndChildCleanup(t *testing.T) {
	r := integrationRunner(t)
	req := testRequest("")
	req.Node.Sandbox = "box"
	req.Node.Inputs = map[string]contract.Port{"source": {Artifact: &contract.ArtifactPort{MediaTypes: []string{"text/plain"}}, Mount: "source.txt"}}
	a, err := r.Hooks.PutArtifact(context.Background(), "source", "text/plain", []byte("immutable"))
	if err != nil {
		t.Fatal(err)
	}
	req.Inputs["source"] = contract.Value{Artifacts: []contract.Artifact{a}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s, err := r.nodeSandbox(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	script := `import os,json,subprocess
for path in ["/knotra/input.json","/workspace/source.txt","/etc/passwd"]:
 try: open(path,"w").write("bad"); raise AssertionError(path)
 except OSError: pass
subprocess.Popen(["python","-c","import time;time.sleep(60)"])
print("safe")`
	result, err := s.helper(ctx, "exec", map[string]any{"command": []string{"python", "-c", script}})
	if err != nil {
		t.Fatal(err)
	}
	var process struct {
		ExitCode int `json:"exitCode"`
	}
	if err = json.Unmarshal(result, &process); err != nil || process.ExitCode != 0 {
		t.Fatalf("isolation failed: %s %v", result, err)
	}
	content := strings.Repeat("x", 1<<20)
	if _, err = s.helper(ctx, "write", map[string]any{"path": "large.txt", "content": content}); err != nil {
		t.Fatalf("1MiB write failed: %v", err)
	}
	if _, err = s.helper(ctx, "write", map[string]any{"path": "../escape", "content": "bad"}); err == nil {
		t.Fatal("path escape allowed")
	}
	if _, err = s.helper(ctx, "exec", map[string]any{"command": []string{"python", "-c", `import os;os.symlink("/etc/passwd","link")`}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.helper(ctx, "collect", map[string]any{"path": "link"}); err == nil {
		t.Fatal("symbolic link was collected")
	}
}

func TestDockerNetworkAllowlist(t *testing.T) {
	r := integrationRunner(t)
	if r.FirewallImage == "" {
		t.Skip("set KNOTRA_TEST_FIREWALL_IMAGE")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	d, err := r.docker()
	if err != nil {
		t.Fatal(err)
	}
	var peer struct {
		ID string `json:"Id"`
	}
	if err = d.json(ctx, "POST", "/containers/create", map[string]any{"Image": "python:3.13-alpine", "Entrypoint": []string{"python", "-m", "http.server", "8000"}, "HostConfig": map[string]any{"NetworkMode": "bridge", "ReadonlyRootfs": true, "CapDrop": []string{"ALL"}, "Memory": 64 << 20, "PidsLimit": 16}}, &peer); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.json(context.Background(), "DELETE", "/containers/"+peer.ID+"?force=true", nil, nil) }()
	if err = d.json(ctx, "POST", "/containers/"+peer.ID+"/start", nil, nil); err != nil {
		t.Fatal(err)
	}
	var inspect struct {
		NetworkSettings struct {
			Networks map[string]struct{ IPAddress string }
		}
	}
	if err = d.json(ctx, "GET", "/containers/"+peer.ID+"/json", nil, &inspect); err != nil {
		t.Fatal(err)
	}
	ip := inspect.NetworkSettings.Networks["bridge"].IPAddress
	req := testRequest("")
	req.Plan.Pipelines["main.yaml"].Spec.Models = nil
	p := req.Plan.Profile.Spec.Sandboxes["local"]
	p.Network = contract.Network{Mode: "allowlist", Hosts: []string{ip}}
	req.Plan.Profile.Spec.Sandboxes["local"] = p
	if err = r.Prepare(ctx, req.Plan); err != nil {
		t.Fatal(err)
	}
	req.Node.Sandbox = "box"
	s, err := r.nodeSandbox(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	script := fmt.Sprintf(`import socket,urllib.request,json
response=urllib.request.urlopen("http://%s:8000",timeout=3)
assert response.status==200
sock=socket.socket();sock.settimeout(0.3)
try: sock.connect(("1.1.1.1",443));raise AssertionError("egress escaped")
except (TimeoutError,OSError): pass
print("allowlist enforced")`, ip)
	b, err := s.helper(ctx, "exec", map[string]any{"command": []string{"python", "-c", script}})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		ExitCode int `json:"exitCode"`
	}
	if err = json.Unmarshal(b, &result); err != nil || result.ExitCode != 0 {
		t.Fatalf("network isolation: %s %v", b, err)
	}
}

func TestMCPStdioInDocker(t *testing.T) {
	r := integrationRunner(t)
	req := testRequest("")
	req.Plan.Pipelines["main.yaml"].Spec.Models = nil
	req.Plan.Package.Files = []contract.File{{Path: "server.py", Content: []byte(`import sys,json
for line in sys.stdin:
 q=json.loads(line)
 if "id" not in q: continue
 method=q["method"]
 if method=="initialize": result={"protocolVersion":q["params"]["protocolVersion"],"capabilities":{"tools":{}},"serverInfo":{"name":"stdio-test","version":"1"}}
 elif method=="tools/list": result={"tools":[{"name":"sum","description":"sum","inputSchema":{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}},"required":["a","b"]}}]}
 elif method=="tools/call": result={"content":[],"structuredContent":{"sum":q["params"]["arguments"]["a"]+q["params"]["arguments"]["b"]}}
 elif method=="ping": result={}
 else:
  print(json.dumps({"jsonrpc":"2.0","id":q["id"],"error":{"code":-32601,"message":"unknown"}}),flush=True);continue
 print(json.dumps({"jsonrpc":"2.0","id":q["id"],"result":result}),flush=True)
`)}}
	req.Plan.Profile.Spec.MCP = map[string]contract.MCPConnection{"local": {Transport: "stdio", Sandbox: "local", Command: []string{"python", "/package/server.py"}, AllowedTools: []string{"sum"}, ToolPolicies: map[string]contract.ToolPolicy{"sum": {Effect: "read"}}}}
	req.Plan.Pipelines["main.yaml"].Spec.MCP = map[string]contract.MCPResource{"tools": {Connection: "local"}}
	req.Node = contract.Node{Type: "tool", Tool: &contract.ToolNode{Server: "tools", Name: "sum"}, Outputs: map[string]contract.Port{"result": {Schema: json.RawMessage(`{"type":"object","properties":{"sum":{"const":7}},"required":["sum"]}`)}}}
	req.ToolArguments = json.RawMessage(`{"a":3,"b":4}`)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := r.Prepare(ctx, req.Plan); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Execute(ctx, req); err != nil {
		t.Fatal(err)
	}
}

func TestOllamaRealStructuredAndAgent(t *testing.T) {
	endpoint := os.Getenv("KNOTRA_TEST_OLLAMA")
	if endpoint == "" {
		t.Skip("set KNOTRA_TEST_OLLAMA for real local model calls")
	}
	r := integrationRunner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	req := testRequest(endpoint)
	req.Plan.Profile.Spec.Models["local"] = contract.ModelConnection{Provider: "ollama", Model: "qwen3.5:9b", BaseURL: endpoint, Parameters: map[string]any{"think": false, "temperature": 0, "num_predict": 256}}
	if err := r.Prepare(ctx, req.Plan); err != nil {
		t.Fatal(err)
	}
	values, err := r.Execute(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if string(values["answer"].JSON) != "42" {
		t.Fatalf("unexpected actual model result: %v", values)
	}
	req.InstanceID = "agent"
	req.Node.Type = "agent"
	req.Node.LLM = nil
	req.Node.Sandbox = "box"
	req.Node.Agent = &contract.AgentNode{Model: "model", Prompt: contract.TextSource{Text: "Immediately call knotra_finish with the JSON argument {\"answer\":42}."}, MaxSteps: 3}
	if _, err = r.Execute(ctx, req); err != nil {
		t.Fatal(err)
	}
}
