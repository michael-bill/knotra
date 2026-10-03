// Package contract defines the versioned language and immutable execution plan.
package contract

import "encoding/json"

type Metadata struct {
	Name        string            `json:"name"`
	Title       string            `json:"title,omitempty"`
	Description string            `json:"description,omitempty"`
	Version     string            `json:"version,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}
type Pipeline struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       Spec     `json:"spec"`
}
type Spec struct {
	Files     []string                   `json:"files,omitempty"`
	Schemas   map[string]json.RawMessage `json:"schemas,omitempty"`
	Models    map[string]Model           `json:"models,omitempty"`
	MCP       map[string]MCPResource     `json:"mcp,omitempty"`
	Sandboxes map[string]SandboxResource `json:"sandboxes,omitempty"`
	Secrets   map[string]SecretResource  `json:"secrets,omitempty"`
	Defaults  Defaults                   `json:"defaults,omitempty"`
	Limits    Limits                     `json:"limits,omitempty"`
	Graph
}
type Graph struct {
	Inputs  map[string]Port `json:"inputs,omitempty"`
	Nodes   map[string]Node `json:"nodes"`
	Outputs map[string]Port `json:"outputs"`
}
type Port struct {
	Schema      json.RawMessage `json:"schema,omitempty"`
	SchemaRef   string          `json:"schemaRef,omitempty"`
	Artifact    *ArtifactPort   `json:"artifact,omitempty"`
	Description string          `json:"description,omitempty"`
	Required    *bool           `json:"required,omitempty"`
	Default     json.RawMessage `json:"default,omitempty"`
	Bind        *Binding        `json:"bind,omitempty"`
	Mount       string          `json:"mount,omitempty"`
	Collect     *Collect        `json:"collect,omitempty"`
	Initial     *Binding        `json:"initial,omitempty"`
	Next        *Binding        `json:"next,omitempty"`
}

func (p Port) IsRequired() bool { return p.Required == nil || *p.Required }

type ArtifactPort struct {
	MediaTypes []string `json:"mediaTypes"`
	Collection bool     `json:"collection,omitempty"`
}
type Collect struct {
	Path      string `json:"path"`
	MediaType string `json:"mediaType"`
}
type Binding struct {
	Value       json.RawMessage `json:"value,omitempty"`
	From        string          `json:"from,omitempty"`
	Path        string          `json:"path,omitempty"`
	Expr        string          `json:"expr,omitempty"`
	Coalesce    []Binding       `json:"coalesce,omitempty"`
	pathPresent bool
}

func (b *Binding) UnmarshalJSON(data []byte) error {
	type plain Binding
	var value plain
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*b = Binding(value)
	_, b.pathPresent = fields["path"]
	return nil
}

type TextSource struct {
	Text string `json:"text,omitempty"`
	File string `json:"file,omitempty"`
}
type Node struct {
	Type         string              `json:"type"`
	Description  string              `json:"description,omitempty"`
	Inputs       map[string]Port     `json:"inputs,omitempty"`
	Outputs      map[string]Port     `json:"outputs,omitempty"`
	Needs        []string            `json:"needs,omitempty"`
	When         string              `json:"when,omitempty"`
	Execution    Execution           `json:"execution,omitempty"`
	Sandbox      string              `json:"sandbox,omitempty"`
	Env          map[string]EnvValue `json:"env,omitempty"`
	Tools        *ToolGrants         `json:"tools,omitempty"`
	LLM          *LLMNode            `json:"llm,omitempty"`
	Agent        *AgentNode          `json:"agent,omitempty"`
	Code         *CodeNode           `json:"code,omitempty"`
	Tool         *ToolNode           `json:"tool,omitempty"`
	Switch       *SwitchNode         `json:"switch,omitempty"`
	Human        *HumanNode          `json:"human,omitempty"`
	Foreach      *ForeachNode        `json:"foreach,omitempty"`
	Loop         *LoopNode           `json:"loop,omitempty"`
	Pipeline     *PipelineNode       `json:"pipeline,omitempty"`
	Dependencies []string            `json:"_dependencies,omitempty"`
}
type LLMNode struct {
	Model        string     `json:"model"`
	Instructions TextSource `json:"instructions,omitempty"`
	Prompt       TextSource `json:"prompt"`
}
type AgentNode struct {
	Model        string     `json:"model"`
	Instructions TextSource `json:"instructions,omitempty"`
	Prompt       TextSource `json:"prompt"`
	MaxSteps     int        `json:"maxSteps"`
}
type CodeNode struct {
	Command []string `json:"command"`
}
type ToolNode struct {
	Server    string  `json:"server"`
	Name      string  `json:"name"`
	Arguments Binding `json:"arguments"`
	Response  string  `json:"response,omitempty"`
}
type SwitchNode struct {
	Cases   []SwitchCase `json:"cases"`
	Default string       `json:"default"`
}
type SwitchCase struct {
	Name string `json:"name"`
	When string `json:"when"`
}
type HumanNode struct {
	Prompt TextSource `json:"prompt"`
}
type ForeachNode struct {
	Over        string             `json:"over"`
	Concurrency int                `json:"concurrency"`
	With        map[string]Binding `json:"with"`
	Body        Graph              `json:"body"`
}
type LoopNode struct {
	MaxIterations int                `json:"maxIterations"`
	OnLimit       string             `json:"onLimit,omitempty"`
	State         map[string]Port    `json:"state,omitempty"`
	With          map[string]Binding `json:"with"`
	Body          Graph              `json:"body"`
	Until         string             `json:"until"`
}
type PipelineNode struct {
	File        string      `json:"file"`
	Permissions Permissions `json:"permissions"`
}
type Permissions struct {
	Models    []string            `json:"models"`
	MCP       map[string][]string `json:"mcp"`
	Sandboxes []string            `json:"sandboxes"`
	Secrets   []string            `json:"secrets"`
}
type Execution struct {
	Timeout          string `json:"timeout,omitempty"`
	Retry            *Retry `json:"retry,omitempty"`
	OnUnknownOutcome string `json:"onUnknownOutcome,omitempty"`
}
type Retry struct {
	MaxAttempts int    `json:"maxAttempts"`
	Backoff     string `json:"backoff,omitempty"`
}
type Defaults struct {
	Execution Execution  `json:"execution,omitempty"`
	Tools     ToolGrants `json:"tools,omitempty"`
}
type Limits struct {
	Timeout            string `json:"timeout,omitempty"`
	MaxConcurrentNodes int    `json:"maxConcurrentNodes,omitempty"`
	MaxNodeInstances   int    `json:"maxNodeInstances,omitempty"`
	MaxModelCalls      int    `json:"maxModelCalls,omitempty"`
	MaxToolCalls       int    `json:"maxToolCalls,omitempty"`
}
type ToolGrants struct {
	Inherit *bool               `json:"inherit,omitempty"`
	MCP     map[string][]string `json:"mcp,omitempty"`
	Sandbox []string            `json:"sandbox,omitempty"`
}
type EnvValue struct {
	Value  *string `json:"value,omitempty"`
	Secret string  `json:"secret,omitempty"`
}
type Model struct {
	Connection string         `json:"connection"`
	Model      string         `json:"model,omitempty"`
	Parameters map[string]any `json:"parameters,omitempty"`
	Requires   []string       `json:"requires,omitempty"`
}
type MCPResource struct {
	Connection string `json:"connection"`
	Session    string `json:"session,omitempty"`
}
type SandboxResource struct {
	Profile string `json:"profile"`
}
type SecretResource struct {
	Ref string `json:"ref"`
}
type Profile struct {
	APIVersion string      `json:"apiVersion"`
	Kind       string      `json:"kind"`
	Metadata   Metadata    `json:"metadata"`
	Spec       ProfileSpec `json:"spec"`
}
type ProfileSpec struct {
	Secrets   map[string]SecretSource    `json:"secrets,omitempty"`
	Models    map[string]ModelConnection `json:"models,omitempty"`
	MCP       map[string]MCPConnection   `json:"mcp,omitempty"`
	Sandboxes map[string]SandboxProfile  `json:"sandboxes,omitempty"`
	Limits    Limits                     `json:"limits"`
}
type SecretSource struct {
	Env string `json:"env"`
}
type Credential struct {
	Value     *string `json:"value,omitempty"`
	SecretRef string  `json:"secretRef,omitempty"`
}
type ModelConnection struct {
	Provider   string                `json:"provider"`
	Model      string                `json:"model"`
	BaseURL    string                `json:"baseUrl,omitempty"`
	Auth       map[string]Credential `json:"auth,omitempty"`
	Parameters map[string]any        `json:"parameters,omitempty"`
}
type MCPConnection struct {
	Transport       string                `json:"transport"`
	URL             string                `json:"url,omitempty"`
	Headers         map[string]Credential `json:"headers,omitempty"`
	Sandbox         string                `json:"sandbox,omitempty"`
	Command         []string              `json:"command,omitempty"`
	Env             map[string]Credential `json:"env,omitempty"`
	AllowedTools    []string              `json:"allowedTools"`
	ToolPolicies    map[string]ToolPolicy `json:"toolPolicies,omitempty"`
	AllowRunSession bool                  `json:"allowRunSession,omitempty"`
}
type ToolPolicy struct {
	Effect              string `json:"effect"`
	IdempotencyArgument string `json:"idempotencyArgument,omitempty"`
}
type SandboxProfile struct {
	ResolvedHosts  map[string][]string `json:"_resolvedHosts,omitempty"`
	Image          string              `json:"image"`
	Resources      Resources           `json:"resources"`
	Network        Network             `json:"network"`
	AllowedTools   []string            `json:"allowedTools"`
	AllowedSecrets []string            `json:"allowedSecrets,omitempty"`
}
type Resources struct {
	CPU       float64 `json:"cpu"`
	MemoryMiB int64   `json:"memoryMiB"`
	DiskMiB   int64   `json:"diskMiB"`
	Pids      int64   `json:"pids"`
}
type Network struct {
	Mode  string   `json:"mode"`
	Hosts []string `json:"hosts,omitempty"`
}

// Package preserves the original source bytes; source is the entrypoint text.
type Package struct {
	Entrypoint string `json:"entrypoint"`
	Source     string `json:"source"`
	Files      []File `json:"files"`
}
type File struct {
	Path    string `json:"path"`
	Content []byte `json:"content"`
}
type Plan struct {
	Runtime         RuntimeSnapshot                    `json:"runtime,omitempty"`
	ModelDigests    map[string]string                  `json:"modelDigests,omitempty"`
	CompilerVersion string                             `json:"compilerVersion"`
	CELVersion      string                             `json:"celVersion"`
	Expressions     map[string]json.RawMessage         `json:"expressions,omitempty"`
	MCPTools        map[string]map[string]ToolSnapshot `json:"mcpTools,omitempty"`
	Version         string                             `json:"version"`
	Digest          string                             `json:"digest"`
	Package         Package                            `json:"package"`
	Root            string                             `json:"root"`
	Pipelines       map[string]*Pipeline               `json:"pipelines"`
	Profile         Profile                            `json:"profile"`
}
type RuntimeSnapshot struct {
	HelperSHA256   string `json:"helperSha256,omitempty"`
	FirewallImage  string `json:"firewallImage,omitempty"`
	AdapterVersion string `json:"adapterVersion,omitempty"`
}
type ToolSnapshot struct {
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	Description  string          `json:"description,omitempty"`
}
type Diagnostic struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Phase    string `json:"phase"`
	Message  string `json:"message"`
	Path     string `json:"path"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Column   int    `json:"column,omitempty"`
}

// Value keeps artifact identity out of user JSON; missing is represented separately.
type Value struct {
	JSON       json.RawMessage `json:"json,omitempty"`
	Artifacts  []Artifact      `json:"artifacts,omitempty"`
	Collection bool            `json:"collection,omitempty"`
}
type Values map[string]Value
type Artifact struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	MediaType string            `json:"mediaType"`
	Size      int64             `json:"size"`
	SHA256    string            `json:"sha256"`
	Path      string            `json:"path,omitempty"`
	Origin    map[string]string `json:"origin"`
}
