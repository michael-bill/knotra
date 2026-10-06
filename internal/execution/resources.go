package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/google/uuid"
)

var hostIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidateHostID keeps a stable, portable identity in database rows and Docker labels.
func ValidateHostID(id string) error {
	if !hostIDPattern.MatchString(id) {
		return errors.New("host identity must contain 1–128 ASCII letters, digits, dots, underscores or hyphens and start with a letter or digit")
	}
	return nil
}

// ResourceRecord identifies an owned resource without storing host paths or secrets.
type ResourceRecord struct {
	ID                 string            `json:"id"`
	EngineID           string            `json:"engineId"`
	HostID             string            `json:"hostId"`
	Kind               string            `json:"kind"`
	Lifetime           string            `json:"lifetime"`
	State              string            `json:"state"`
	DispatchGeneration int64             `json:"dispatchGeneration"`
	Ownership          Ownership         `json:"ownership"`
	MCP                *MCPSessionRecord `json:"mcp,omitempty"`
}

// Connection refers to the frozen admitted profile, not resolved credentials.
type MCPSessionRecord struct {
	Connection      string `json:"connection"`
	SessionID       string `json:"sessionId"`
	ProtocolVersion string `json:"protocolVersion"`
}

func (m MCPSessionRecord) Validate() error {
	if m.Connection == "" || len(m.Connection) > 1024 || len(m.SessionID) > 8192 {
		return errors.New("invalid MCP cleanup identity")
	}
	for _, char := range m.SessionID {
		if char < 0x21 || char > 0x7e {
			return errors.New("invalid MCP session header")
		}
	}
	if _, err := time.Parse("2006-01-02", m.ProtocolVersion); err != nil {
		return errors.New("invalid MCP protocol version")
	}
	return nil
}

func (r ResourceRecord) Name() string { return "sandbox-" + r.ID }

func (r ResourceRecord) Validate() error {
	if err := ValidateHostID(r.HostID); err != nil {
		return err
	}
	if id, err := uuid.Parse(r.ID); err != nil || id.String() != r.ID || r.EngineID == "" || r.HostID == "" || (r.Kind != "sandbox" && r.Kind != "mcp_http") {
		return errors.New("invalid resource identity")
	}
	if r.MCP != nil {
		if r.Kind != "mcp_http" {
			return errors.New("MCP cleanup identity on a different resource kind")
		}
		if err := r.MCP.Validate(); err != nil {
			return err
		}
	}
	return r.Ownership.Validate()
}

func (r ResourceRecord) Labels() map[string]string {
	return map[string]string{
		"io.knotra.engine": r.EngineID, "io.knotra.host": r.HostID,
		"io.knotra.resource": r.ID, "io.knotra.run": r.Ownership.RunID,
		"io.knotra.instance": r.Ownership.InstanceID, "io.knotra.worker": r.Ownership.WorkerID,
		"io.knotra.attempt": fmt.Sprint(r.Ownership.Number), "io.knotra.generation": fmt.Sprint(r.Ownership.Generation),
	}
}

const maxMCPEvidenceBytes = 64 << 10

type mcpEvidence struct {
	FormatVersion int             `json:"formatVersion"`
	Resource      ResourceRecord  `json:"resource"`
	RequestID     json.RawMessage `json:"requestId,omitempty"`
	Completed     bool            `json:"completed,omitempty"`
}

func mcpEvidenceKey(resource ResourceRecord) (string, error) {
	if err := resource.Validate(); err != nil {
		return "", err
	}
	if resource.Kind != "mcp_http" {
		return "", errors.New("MCP evidence requires an HTTP session resource")
	}
	owner, err := resource.Ownership.OutcomeKey()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte("mcp-session/" + resource.ID + "/" + owner))
	return hex.EncodeToString(sum[:]), nil
}

// PutMCPSession persists initialization evidence before its database update.
// It uses the outcome store's immutable publication, but a separate key namespace.
func (s *OutcomeFiles) PutMCPSession(resource ResourceRecord) error {
	key, err := mcpEvidenceKey(resource)
	if err != nil {
		return err
	}
	if resource.MCP == nil {
		return errors.New("MCP initialization has no cleanup identity")
	}
	data, err := json.Marshal(mcpEvidence{FormatVersion: StateFormatVersion, Resource: resource})
	if err != nil {
		return err
	}
	// Leave room for the checksum envelope; bounds apply only to MCP identities.
	if len(data) > maxMCPEvidenceBytes-1024 {
		return errors.New("MCP cleanup evidence exceeds byte limit")
	}
	_, err = s.put(key, data)
	return err
}

// GetMCPSession checks the complete immutable owner against the durable ledger.
// Cleanup state and dispatch generation may have advanced since initialization.
func (s *OutcomeFiles) GetMCPSession(resource ResourceRecord) (*MCPSessionRecord, error) {
	key, err := mcpEvidenceKey(resource)
	if err != nil {
		return nil, err
	}
	data, err := s.readBounded(key, maxMCPEvidenceBytes)
	if err != nil {
		return nil, err
	}
	var evidence mcpEvidence
	if err := decodeStrict(data, &evidence); err != nil {
		return nil, errors.Join(ErrInvalidOutcome, err)
	}
	stored := evidence.Resource
	if evidence.FormatVersion != StateFormatVersion || !sameMCPResource(stored, resource) {
		return nil, fmt.Errorf("%w: MCP evidence does not match its recorded owner", ErrInvalidOutcome)
	}
	if resource.MCP != nil && *resource.MCP != *stored.MCP {
		return nil, fmt.Errorf("%w: MCP evidence conflicts with the recorded session", ErrInvalidOutcome)
	}
	return stored.MCP, nil
}

func sameMCPResource(stored, expected ResourceRecord) bool {
	return stored.MCP != nil && stored.Validate() == nil && stored.ID == expected.ID && stored.EngineID == expected.EngineID && stored.HostID == expected.HostID && stored.Kind == expected.Kind && stored.Lifetime == expected.Lifetime && stored.Ownership == expected.Ownership && (expected.MCP == nil || *expected.MCP == *stored.MCP)
}

func mcpCallKey(resource ResourceRecord) (string, error) {
	key, err := mcpEvidenceKey(resource)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte("mcp-call/" + key))
	return hex.EncodeToString(sum[:]), nil
}

func validateMCPRequestID(id json.RawMessage) error {
	if len(id) == 0 || len(id) > 1024 {
		return errors.New("invalid MCP request ID")
	}
	var number *int64
	var text *string
	if (json.Unmarshal(id, &number) != nil || number == nil) && (json.Unmarshal(id, &text) != nil || text == nil) {
		return errors.New("MCP request ID must be an integer or string")
	}
	return nil
}

// PutMCPCall durably replaces the current RPC identity before dispatch. Calls on
// a session are serialized; no arguments, responses or credentials are stored.
func (s *OutcomeFiles) PutMCPCall(resource ResourceRecord, id json.RawMessage, completed bool) error {
	key, err := mcpCallKey(resource)
	if err != nil {
		return err
	}
	if resource.MCP == nil {
		return errors.New("MCP call has no initialized session")
	}
	if err := validateMCPRequestID(id); err != nil {
		return err
	}
	data, err := json.Marshal(mcpEvidence{FormatVersion: StateFormatVersion, Resource: resource, RequestID: id, Completed: completed})
	if err != nil {
		return err
	}
	if len(data) > maxMCPEvidenceBytes-1024 {
		return errors.New("MCP call evidence exceeds byte limit")
	}
	_, err = s.publish(key, data, true)
	return err
}

// GetMCPCall returns only the last unconfirmed request, fenced to the original
// session and resource owner. Initialization-only evidence has no active call.
func (s *OutcomeFiles) GetMCPCall(resource ResourceRecord) (json.RawMessage, error) {
	key, err := mcpCallKey(resource)
	if err != nil {
		return nil, err
	}
	data, err := s.readBounded(key, maxMCPEvidenceBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var evidence mcpEvidence
	if err := decodeStrict(data, &evidence); err != nil {
		return nil, errors.Join(ErrInvalidOutcome, err)
	}
	if evidence.FormatVersion != StateFormatVersion || !sameMCPResource(evidence.Resource, resource) {
		return nil, fmt.Errorf("%w: MCP call does not match its recorded owner", ErrInvalidOutcome)
	}
	if err := validateMCPRequestID(evidence.RequestID); err != nil {
		return nil, errors.Join(ErrInvalidOutcome, err)
	}
	if evidence.Completed {
		return nil, nil
	}
	return evidence.RequestID, nil
}
